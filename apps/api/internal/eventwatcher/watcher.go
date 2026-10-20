package eventwatcher

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/belune-hq/belune/internal/runtime"
	"github.com/belune-hq/belune/internal/status"
	"github.com/belune-hq/belune/internal/store/generated"
	"github.com/belune-hq/belune/internal/ws"
)

const (
	labelApplicationID = runtime.LabelApplicationID
	labelDatabaseID    = runtime.LabelDatabaseID
	// containerStateRunning is Docker's State value for a container that is
	// actually up. ContainerInfo.Status carries State ("running", "exited",
	// "created", "paused", …), not the human-readable "Up 5 minutes" string.
	containerStateRunning = "running"
)

// Watcher monitors Docker container events and synchronizes application and
// database status with actual container state.
type Watcher struct {
	runtime     runtime.ContainerRuntime
	queries     *generated.Queries
	broadcaster *ws.ContainerStatusBroadcaster
}

func New(rt runtime.ContainerRuntime, queries *generated.Queries, broadcaster *ws.ContainerStatusBroadcaster) *Watcher {
	return &Watcher{
		runtime:     rt,
		queries:     queries,
		broadcaster: broadcaster,
	}
}

// Run starts the event watcher. It first reconciles current state, then
// listens for Docker events. On error it reconnects with backoff.
func (w *Watcher) Run(ctx context.Context) {
	// Initial reconciliation
	w.reconcile(ctx)

	backoff := time.Second
	maxBackoff := 30 * time.Second

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		err := w.watchEvents(ctx)
		if ctx.Err() != nil {
			return
		}

		slog.Warn("eventwatcher: event stream ended, reconnecting", "error", err, "backoff", backoff)
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}

		// Re-reconcile on reconnect
		w.reconcile(ctx)

		backoff = min(backoff*2, maxBackoff)
	}
}

// containerIndex sorts the host's containers into the two groups reconciliation
// cares about: those that are demonstrably up, and those in a state that proves
// nothing either way.
type containerIndex struct {
	runningApps  map[string]string // app ID → container name
	runningDBs   map[string]bool
	runningNames map[string]bool

	// transient* hold resources whose container is neither up nor settled —
	// mid-restart, mid-removal, or created but not yet started. Their stored
	// status must be left alone rather than corrected from such a sample.
	transientApps  map[string]bool
	transientDBs   map[string]bool
	transientNames map[string]bool
}

// isTransientState reports whether a Docker container state is a passing phase
// rather than a settled outcome.
//
// This matters because containers are created with RestartPolicy
// "unless-stopped", so a crash-looping container genuinely sits in "restarting".
// Treating that as "not running" would flip the application to stopped — and
// "stopped" means *deliberate* in this vocabulary, so a crash loop would read as
// though the operator had turned it off. "created" is included because a deploy
// briefly passes through it between creating and starting the container.
func isTransientState(state string) bool {
	switch state {
	case "restarting", "removing", "created", "paused":
		return true
	}
	return false
}

// buildContainerIndex sorts containers by state, keyed by application ID,
// database ID, and container name.
//
// The state filter is essential. ListContainers asks Docker with All: true, so
// it returns stopped containers as well, and a stopped container keeps every
// label it was created with. Indexing them all made an exited container look
// like proof its application was up: restarting the control plane left every app
// reading "running" while nothing was serving, and a correctly-stopped app could
// even be flipped back to running. Only containers in Docker's "running" state
// count as evidence of a live service.
//
// Names are indexed for databases provisioned before the database-id label
// existed, which are recognised by container name instead.
func buildContainerIndex(containers []runtime.ContainerInfo) *containerIndex {
	idx := &containerIndex{
		runningApps:    make(map[string]string),
		runningDBs:     make(map[string]bool),
		runningNames:   make(map[string]bool),
		transientApps:  make(map[string]bool),
		transientDBs:   make(map[string]bool),
		transientNames: make(map[string]bool),
	}
	for _, c := range containers {
		appID, hasApp := c.Labels[labelApplicationID]
		dbID, hasDB := c.Labels[labelDatabaseID]

		switch {
		case c.Status == containerStateRunning:
			idx.runningNames[c.Name] = true
			if hasApp {
				idx.runningApps[appID] = c.Name
			}
			if hasDB {
				idx.runningDBs[dbID] = true
			}
		case isTransientState(c.Status):
			idx.transientNames[c.Name] = true
			if hasApp {
				idx.transientApps[appID] = true
			}
			if hasDB {
				idx.transientDBs[dbID] = true
			}
		}
	}
	return idx
}

// reconcile compares running containers with DB status and fixes mismatches.
func (w *Watcher) reconcile(ctx context.Context) {
	containers, err := w.runtime.ListContainers(ctx)
	if err != nil {
		slog.Warn("eventwatcher: reconciliation failed to list containers", "error", err)
		return
	}

	// Build maps of running container app/database IDs. Databases are also keyed
	// by container name (which equals the slug) so databases provisioned before
	// the database-id label existed are still recognized as running and not
	// falsely flipped to stopped.
	idx := buildContainerIndex(containers)

	// Check all applications and reconcile status
	apps, err := w.queries.ListAllApplications(ctx)
	if err != nil {
		slog.Warn("eventwatcher: reconciliation failed to list applications", "error", err)
		return
	}

	for _, app := range apps {
		appIDStr := pgUUIDToString(app.ID)
		containerName, isRunning := idx.runningApps[appIDStr]

		if !isRunning {
			// Mid-restart, mid-removal, or created-not-yet-started proves
			// nothing: reconciliation samples only at startup and on event
			// stream reconnect, so it must not turn a passing phase into a
			// stored verdict. The start/die events settle it a moment later.
			if idx.transientApps[appIDStr] {
				slog.Debug("eventwatcher: skipping application in a transient container state",
					"app_id", appIDStr, "status", app.Status)
				continue
			}
			// The container is gone. Anything that implies "up" (running or
			// unhealthy) is corrected to stopped; a deliberate stop or a
			// recorded error is left alone.
			if app.Status == status.ApplicationRunning || app.Status == status.ApplicationUnhealthy {
				slog.Info("eventwatcher: reconciling stopped application", "app_id", appIDStr)
				w.updateAppStatus(ctx, app.ID, appIDStr, status.ApplicationStopped)
			}
			continue
		}

		// The container is up, so the app is running — unless a command health
		// check says otherwise. This is the only place that catches an app that
		// went unhealthy while the control plane was down, since Docker only
		// emits a health event on a *change*.
		desired := status.ApplicationRunning
		if app.HealthCheckType == "command" {
			if h, err := w.runtime.ContainerHealth(ctx, containerName); err == nil && h == "unhealthy" {
				desired = status.ApplicationUnhealthy
			}
		}
		// "error" is included because the container is ground truth: if it is
		// up, the app is running (or unhealthy), whatever went wrong before.
		// Without this an app could sit in "error" indefinitely while serving.
		if app.Status != desired &&
			(app.Status == status.ApplicationStopped ||
				app.Status == status.ApplicationError ||
				app.Status == status.ApplicationRunning ||
				app.Status == status.ApplicationUnhealthy) {
			slog.Info("eventwatcher: reconciling application", "app_id", appIDStr, "from", app.Status, "to", desired)
			w.updateAppStatus(ctx, app.ID, appIDStr, desired)
		}
	}

	// Check all databases and reconcile status. Only the steady running/stopped
	// states are reconciled; transient states (creating, upgrading, backing_up,
	// failed) are left for their owning task to resolve.
	dbs, err := w.queries.ListAllDatabases(ctx)
	if err != nil {
		slog.Warn("eventwatcher: reconciliation failed to list databases", "error", err)
		return
	}

	for _, db := range dbs {
		dbIDStr := pgUUIDToString(db.ID)
		isRunning := idx.runningDBs[dbIDStr] || idx.runningNames[db.Slug]

		// Same reasoning as applications: a container mid-restart or mid-removal
		// is not evidence of anything, so leave the stored status alone.
		if !isRunning && (idx.transientDBs[dbIDStr] || idx.transientNames[db.Slug]) {
			slog.Debug("eventwatcher: skipping database in a transient container state",
				"database_id", dbIDStr, "status", db.Status)
			continue
		}

		if db.Status == status.DatabaseRunning && !isRunning {
			slog.Info("eventwatcher: reconciling stopped database", "database_id", dbIDStr)
			w.updateDatabaseStatus(ctx, db.ID, dbIDStr, status.DatabaseStopped)
		} else if db.Status == status.DatabaseStopped && isRunning {
			slog.Info("eventwatcher: reconciling running database", "database_id", dbIDStr)
			w.updateDatabaseStatus(ctx, db.ID, dbIDStr, status.DatabaseRunning)
		}
	}

	slog.Info("eventwatcher: reconciliation complete", "containers", len(containers), "apps", len(apps), "databases", len(dbs))
}

// watchEvents subscribes to Docker events and processes them.
func (w *Watcher) watchEvents(ctx context.Context) error {
	eventCh, errCh := w.runtime.ContainerEvents(ctx, map[string][]string{
		"label": {"managed-by=belune"},
		"event": {"start", "stop", "die", "restart", "oom", "health_status"},
	})

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()

		case err := <-errCh:
			return err

		case event, ok := <-eventCh:
			if !ok {
				return nil
			}
			w.handleEvent(ctx, event)
		}
	}
}

// handleEvent processes a single Docker container event, dispatching to the
// application or database handler based on which managed-resource label the
// container carries.
func (w *Watcher) handleEvent(ctx context.Context, event runtime.ContainerEvent) {
	if appID := event.Labels[labelApplicationID]; appID != "" {
		w.handleApplicationEvent(ctx, event, appID)
		return
	}
	if dbID := event.Labels[labelDatabaseID]; dbID != "" {
		w.handleDatabaseEvent(ctx, event, dbID)
		return
	}
}

func (w *Watcher) handleApplicationEvent(ctx context.Context, event runtime.ContainerEvent, appID string) {
	var appUUID pgtype.UUID
	if err := appUUID.Scan(appID); err != nil {
		slog.Debug("eventwatcher: invalid application-id label", "value", appID)
		return
	}

	newStatus, ok := ApplicationStatusForEvent(event)
	if !ok {
		return
	}
	if newStatus == status.ApplicationError && event.Status == "oom" {
		slog.Warn("eventwatcher: container killed by OOM", "app_id", appID, "container", event.ContainerName)
	}

	// Two events need the current status to be interpreted; the rest stand on
	// their own, so the read is skipped for them.
	if event.Status == "die" {
		current, err := w.queries.GetApplication(ctx, appUUID)
		if err == nil && !ApplyDieStatus(newStatus, current.Status) {
			slog.Debug("eventwatcher: keeping existing status",
				"app_id", appID, "keeping", current.Status, "ignored", newStatus)
			return
		}
	}
	if isHealthEvent(event.Status) {
		// A health verdict only means something while the container is meant to
		// be up. If the app is stopped, errored, or inactive, a late or stale
		// health event must not resurrect it — the container's own start/stop
		// events own those transitions.
		current, err := w.queries.GetApplication(ctx, appUUID)
		if err == nil &&
			current.Status != status.ApplicationRunning &&
			current.Status != status.ApplicationUnhealthy {
			slog.Debug("eventwatcher: ignoring health event on non-running app",
				"app_id", appID, "status", current.Status, "event", event.Status)
			return
		}
	}

	slog.Info("eventwatcher: container event",
		"app_id", appID,
		"event", event.Status,
		"container", event.ContainerName,
		"new_status", newStatus,
	)

	w.updateAppStatus(ctx, appUUID, appID, newStatus)
}

// isHealthEvent reports whether a Docker event action is a health-status
// transition, which Docker phrases as "health_status: healthy" or
// "health_status: unhealthy".
func isHealthEvent(action string) bool {
	return strings.HasPrefix(action, "health_status")
}

// ApplicationStatusForEvent maps a Docker container event to an application
// status, returning ok=false for events that should not change status.
//
// A crash and a clean stop are distinguished by exit code, so an app that
// crash-loops no longer looks identical to one the user deliberately stopped.
func ApplicationStatusForEvent(event runtime.ContainerEvent) (newStatus string, ok bool) {
	// Docker phrases health events as "health_status: healthy" /
	// "health_status: unhealthy". A healthy verdict means the container is up
	// and passing, so it also clears a prior unhealthy/starting state back to
	// running.
	if isHealthEvent(event.Status) {
		if strings.HasSuffix(event.Status, "healthy") && !strings.HasSuffix(event.Status, "unhealthy") {
			return status.ApplicationRunning, true
		}
		return status.ApplicationUnhealthy, true
	}

	switch event.Status {
	case "start", "restart":
		return status.ApplicationRunning, true
	case "stop":
		return status.ApplicationStopped, true
	case "die":
		return exitStatus(event.Labels["exitCode"]), true
	case "oom":
		return status.ApplicationError, true
	default:
		return "", false
	}
}

// ApplyDieStatus reports whether the status derived from a `die` event should
// be written over the status the application already has.
//
// A `die` is ambiguous: it carries an exit code but says nothing about who
// asked for the exit, and it races with the writes made by whoever did the
// asking. A `stop` event is not ambiguous — Docker emits it only because
// something asked the container to stop — so callers apply those unconditionally
// and never consult this.
//
// That distinction is the fix for the bug where stopping an application left it
// showing "error". Docker emits `die` then `stop`; an earlier version guarded on
// the status alone, so a stop whose process exited non-zero wrote "error" on the
// die, and then the very event that would have corrected it was swallowed for
// looking like a downgrade. The application stayed errored indefinitely.
func ApplyDieStatus(derived, current string) bool {
	// Never downgrade a failed deploy to "stopped". The compensating cleanup
	// removes the half-created container, and that die can land after
	// failDeployment has already recorded the error.
	if derived == status.ApplicationStopped && current == status.ApplicationError {
		return false
	}
	// Never turn a deliberate stop into an error. An application already
	// recorded as stopped has nothing left running to crash, so this die is
	// reporting the stop that was asked for — whatever exit code came back.
	//
	// Wrapper commands make that code untrustworthy in exactly this case:
	// `npm start` exits 1 when its child is terminated by SIGTERM instead of
	// propagating 143, so the exit code alone cannot tell a stop from a crash.
	if derived == status.ApplicationError && current == status.ApplicationStopped {
		return false
	}
	return true
}

// exitStatus decides whether an exit code describes a crash.
//
// Being killed by a signal is not a crash. `docker stop` sends SIGTERM, and a
// process that does not install a handler is terminated by it — which Docker
// reports as exit 128+signal, so 143 for SIGTERM and 137 for SIGKILL after the
// grace period. Plenty of application images never trap SIGTERM, so treating
// every non-zero exit as a crash marked ordinary, deliberate stops as errors.
//
// The database mapping gets away with the simpler rule because Postgres and
// MySQL do trap SIGTERM and exit 0. Generalising that to arbitrary application
// images was the mistake this corrects.
//
// An OOM kill also exits 137, but Docker emits a distinct `oom` event first and
// the caller refuses to downgrade an errored application back to stopped, so
// the more informative status survives.
//
// What remains an error is an exit code the application chose itself: a failed
// start, a fatal config error, a panic.
func exitStatus(exitCode string) string {
	switch exitCode {
	case "", "0":
		return status.ApplicationStopped
	}
	code, err := strconv.Atoi(exitCode)
	if err != nil {
		// Unparseable: prefer the non-alarming reading rather than inventing a
		// failure the user cannot explain.
		return status.ApplicationStopped
	}
	if code > 128 && code <= 128+64 {
		return status.ApplicationStopped
	}
	return status.ApplicationError
}

// isTransitionalDBStatus reports whether a database is in a state owned by a
// running task (provision/reload/reconfigure/upgrade/backup), during which
// container lifecycle events must not overwrite the status the task set.
func isTransitionalDBStatus(s string) bool {
	return s == status.DatabaseCreating ||
		s == status.DatabaseUpgrading ||
		s == status.DatabaseBackingUp
}

// databaseStatusForEvent maps a Docker container event to a database status,
// returning ok=false for events that should not change status.
func databaseStatusForEvent(event runtime.ContainerEvent) (newStatus string, ok bool) {
	switch event.Status {
	case "start", "restart":
		return status.DatabaseRunning, true
	case "stop":
		return status.DatabaseStopped, true
	case "die":
		// A clean stop exits 0; a non-zero exit means the container crashed
		// (e.g. a misconfigured image), which we surface as failed.
		if event.Labels["exitCode"] == "0" {
			return status.DatabaseStopped, true
		}
		return status.DatabaseFailed, true
	case "oom":
		return status.DatabaseFailed, true
	default:
		return "", false
	}
}

func (w *Watcher) handleDatabaseEvent(ctx context.Context, event runtime.ContainerEvent, dbID string) {
	var dbUUID pgtype.UUID
	if err := dbUUID.Scan(dbID); err != nil {
		slog.Debug("eventwatcher: invalid database-id label", "value", dbID)
		return
	}

	newStatus, ok := databaseStatusForEvent(event)
	if !ok {
		return
	}

	// A database in a transitional state is owned by a running task
	// (provision/reload/reconfigure/upgrade/backup), which recreates the
	// container — removing the old one and starting a new one. The die/stop/start
	// events that recreate emits would otherwise clobber the task's `creating`
	// status with a transient stopped/failed, and the task's own terminal write
	// races them. The task sets the final status itself, so ignore container
	// events while it is in flight. Mirrors the reconcile loop's transient skip.
	if current, err := w.queries.GetDatabase(ctx, dbUUID); err == nil && isTransitionalDBStatus(current.Status) {
		slog.Debug("eventwatcher: keeping transitional database status",
			"database_id", dbID, "keeping", current.Status, "ignored", newStatus)
		return
	}

	if event.Status == "oom" {
		slog.Warn("eventwatcher: database container killed by OOM", "database_id", dbID, "container", event.ContainerName)
	}

	slog.Info("eventwatcher: database container event",
		"database_id", dbID,
		"event", event.Status,
		"container", event.ContainerName,
		"new_status", newStatus,
	)

	w.updateDatabaseStatus(ctx, dbUUID, dbID, newStatus)
}

func (w *Watcher) updateAppStatus(ctx context.Context, appUUID pgtype.UUID, appIDStr, newStatus string) {
	_, err := w.queries.UpdateApplicationStatus(ctx, generated.UpdateApplicationStatusParams{
		ID:     appUUID,
		Status: newStatus,
	})
	if err != nil {
		slog.Warn("eventwatcher: failed to update app status", "app_id", appIDStr, "error", err)
		return
	}

	if w.broadcaster != nil {
		w.broadcaster.BroadcastStatus(appIDStr, newStatus)
	}
}

func (w *Watcher) updateDatabaseStatus(ctx context.Context, dbUUID pgtype.UUID, dbIDStr, newStatus string) {
	_, err := w.queries.UpdateDatabaseStatus(ctx, generated.UpdateDatabaseStatusParams{
		ID:     dbUUID,
		Status: newStatus,
	})
	if err != nil {
		slog.Warn("eventwatcher: failed to update database status", "database_id", dbIDStr, "error", err)
		return
	}

	if w.broadcaster != nil {
		w.broadcaster.BroadcastDatabaseStatus(dbIDStr, newStatus)
	}
}

func pgUUIDToString(u pgtype.UUID) string {
	if !u.Valid {
		return ""
	}
	b := u.Bytes
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
