package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"strconv"
	"strings"
	"time"

	"github.com/hibiken/asynq"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/belune-hq/belune/internal/naming"
	"github.com/belune-hq/belune/internal/runtime"
	"github.com/belune-hq/belune/internal/store/generated"
)

// settingDailyCleanup gates the periodic (scheduled) cleanup run. Absent or any
// value other than "false" means enabled — preserving the historical always-on
// behaviour.
const settingDailyCleanup = "daily_cleanup_enabled"

// settingOrphanedBackupRetentionDays bounds how long a backup is kept after the
// database it came from is deleted. Without a bound the reversal this release
// makes — keep by default — would mean remote storage that only ever grows,
// which is its own kind of surprise bill.
//
// The clock runs from the deletion, not from when the backup was taken: keeping
// is a decision made at deletion time, so a two-year-old backup of a database
// deleted yesterday has just been deliberately kept.
const settingOrphanedBackupRetentionDays = "orphaned_backup_retention_days"

// defaultOrphanedBackupRetentionDays is long enough that "I deleted the wrong
// database" is recoverable well past the point anyone notices, and short enough
// that forgotten archives do not accumulate indefinitely. Setting it to 0
// disables expiry entirely, for installs that would rather decide by hand.
const defaultOrphanedBackupRetentionDays = 90

// expiredBackupPageSize bounds one pass of the expiry sweep. Sibling of
// cleanupBackupsPageSize in the service, and for the same reason.
const expiredBackupPageSize = 500

type cleanupPayload struct {
	ApplicationID string `json:"application_id,omitempty"`
	RetainCount   int    `json:"retain_count,omitempty"`
	// Scheduled marks the periodic run enqueued by the scheduler. Only scheduled
	// runs are gated by the daily_cleanup_enabled setting; manual and per-app
	// cleanups always run.
	Scheduled bool `json:"scheduled,omitempty"`
	// Actions selects which cleanup steps to run. Empty = full cleanup (all
	// steps), which the periodic run and the "Run full cleanup" button use.
	// Values: "deployments" | "images" | "volumes" | "containers" | "build_cache".
	Actions []string `json:"actions,omitempty"`
}

func (h *TaskHandler) HandleCleanupTask(ctx context.Context, t *asynq.Task) error {
	var payload cleanupPayload
	if len(t.Payload()) > 0 {
		if err := json.Unmarshal(t.Payload(), &payload); err != nil {
			slog.Debug("could not parse cleanup payload, using defaults", "error", err)
		}
	}

	if payload.RetainCount <= 0 {
		payload.RetainCount = 3
	}

	// Scheduled runs honour the daily-cleanup toggle; manual/per-app runs ignore it.
	if payload.Scheduled && !h.dailyCleanupEnabled(ctx) {
		slog.Info("daily cleanup disabled by setting; skipping scheduled run")
		return nil
	}

	full := len(payload.Actions) == 0
	wants := func(action string) bool {
		if full {
			return true
		}
		for _, a := range payload.Actions {
			if a == action {
				return true
			}
		}
		return false
	}

	slog.Info("handling cleanup task", "retain_count", payload.RetainCount,
		"application_id", payload.ApplicationID, "actions", payload.Actions)

	totalRemoved := 0
	appsProcessed := 0

	if wants("deployments") {
		if payload.ApplicationID != "" {
			// Single-app cleanup: one JOIN query instead of GetApplication + GetProject.
			appID, err := parseUUID(payload.ApplicationID)
			if err != nil {
				return errors.Join(fmt.Errorf("invalid application_id (permanent): %w", err), asynq.SkipRetry)
			}
			row, err := h.Queries.GetApplicationWithProjectSlug(ctx, appID)
			if err != nil {
				return fmt.Errorf("get application: %w", err)
			}
			h.cleanupAppDeployments(ctx, row.ID, row.ServerID, row.Type, row.Slug, row.ProjectSlug, payload.RetainCount, &totalRemoved)
			appsProcessed = 1
		} else {
			// Bulk cleanup: single JOIN query — no per-row GetProject call.
			rows, err := h.Queries.ListAllApplicationsWithProjectSlug(ctx)
			if err != nil {
				return fmt.Errorf("list applications: %w", err)
			}
			for _, row := range rows {
				h.cleanupAppDeployments(ctx, row.ID, row.ServerID, row.Type, row.Slug, row.ProjectSlug, payload.RetainCount, &totalRemoved)
			}
			appsProcessed = len(rows)
		}
	}

	// Pruning is a whole-host operation, so it addresses the host directly
	// rather than any one resource's placement. Multi-server turns this into a
	// sweep per server (see the orphan sweep below).
	if wants("images") || wants("volumes") || wants("build_cache") {
		rt, err := h.Runtimes.Local(ctx)
		if err != nil {
			slog.Warn("failed to reach the Docker host to prune", "error", err)
		} else {
			if wants("images") {
				if err := rt.PruneImages(ctx); err != nil {
					slog.Warn("failed to prune images", "error", err)
				}
			}
			if wants("volumes") {
				if err := rt.PruneVolumes(ctx); err != nil {
					slog.Warn("failed to prune volumes", "error", err)
				}
			}
			if wants("build_cache") {
				if err := rt.PruneBuildCache(ctx); err != nil {
					slog.Warn("failed to prune build cache", "error", err)
				}
			}
		}
	}
	if wants("containers") {
		// Remove orphan containers: managed containers with no matching application in DB.
		h.cleanupOrphanContainers(ctx)
	}
	if wants("orphaned_backups") {
		h.cleanupExpiredOrphanedBackups(ctx)
	}

	// Reap idle preview apps only in a full bulk cleanup — never for a targeted
	// single-app or single-action run.
	if full && payload.ApplicationID == "" {
		h.cleanupStalePreviews(ctx)
	}

	slog.Info("cleanup completed",
		"applications_processed", appsProcessed,
		"deployments_removed", totalRemoved,
	)
	return nil
}

// dailyCleanupEnabled reports whether the periodic cleanup should run. Absent or
// unreadable setting defaults to enabled (preserving prior always-on behaviour).
func (h *TaskHandler) dailyCleanupEnabled(ctx context.Context) bool {
	s, err := h.Queries.GetSetting(ctx, settingDailyCleanup)
	if err != nil {
		return true
	}
	return s.Value != "false"
}

// cleanupAppDeployments removes images and DB records for deployments of a
// single application beyond the retain count. Errors are logged as warnings
// and never returned — cleanup is best-effort and must not abort sibling apps.
func (h *TaskHandler) cleanupAppDeployments(ctx context.Context, appID, serverID pgtype.UUID, appType, appSlug, projectSlug string, retainCount int, totalRemoved *int) {
	applicationIDStr := formatUUID(appID)
	if applicationIDStr == "" {
		return
	}

	oldDeployments, err := h.Queries.ListOldDeployments(ctx, generated.ListOldDeploymentsParams{
		ApplicationID: appID,
		Offset:        int32(retainCount),
	})
	if err != nil {
		slog.Warn("cleanup: failed to list old deployments", "application_id", applicationIDStr, "error", err)
		return
	}

	// Images live on the host the application is placed on. Resolved once for
	// the batch; if the host is unreachable the images are left alone, but the
	// deployment rows must still be pruned.
	var rt runtime.ContainerRuntime
	if appType == "git" {
		var err error
		if rt, err = h.Runtimes.For(ctx, serverID); err != nil {
			slog.Warn("cleanup: could not reach the application's server to remove images",
				"application_id", applicationIDStr, "error", err)
		}
	}

	for _, dep := range oldDeployments {
		deploymentIDStr := formatUUID(dep.ID)

		if rt != nil && deploymentIDStr != "" {
			imageName := naming.ImageTag(projectSlug, appSlug, applicationIDStr, deploymentIDStr)
			oldImageName := fmt.Sprintf("belune-%s:%s", applicationIDStr[:8], deploymentIDStr[:8])
			if err := rt.RemoveImage(ctx, imageName); err != nil {
				slog.Debug("could not remove image", "image", imageName, "error", err)
			} else {
				slog.Info("removed old image", "image", imageName)
			}
			if err := rt.RemoveImage(ctx, oldImageName); err != nil {
				slog.Debug("could not remove legacy image", "image", oldImageName, "error", err)
			}
		}

		if err := h.Queries.DeleteDeployment(ctx, dep.ID); err != nil {
			slog.Warn("failed to delete deployment", "deployment_id", deploymentIDStr, "error", err)
		} else {
			*totalRemoved++
		}
	}
}

// orphanedBackupRetentionDays reads the configured retention. An absent,
// unreadable or nonsensical value falls back to the default rather than to
// "never expire" or "expire now" — the first quietly grows storage forever, the
// second deletes backups on a parse error.
func (h *TaskHandler) orphanedBackupRetentionDays(ctx context.Context) int {
	s, err := h.Queries.GetSetting(ctx, settingOrphanedBackupRetentionDays)
	if err != nil {
		return defaultOrphanedBackupRetentionDays
	}
	days, err := strconv.Atoi(strings.TrimSpace(s.Value))
	if err != nil || days < 0 {
		slog.Warn("orphaned backup retention is not a valid number of days; using the default",
			"value", s.Value, "default", defaultOrphanedBackupRetentionDays)
		return defaultOrphanedBackupRetentionDays
	}
	return days
}

// cleanupExpiredOrphanedBackups erases backups kept past their retention, along
// with the tombstones left describing nothing.
//
// Best-effort per backup, like every other prune here: a remote object that
// cannot be deleted must not stop the rest of the sweep.
func (h *TaskHandler) cleanupExpiredOrphanedBackups(ctx context.Context) {
	// First, and unconditionally: a tombstone also empties out when its last
	// backup is deleted by hand from the project inventory, which has nothing to
	// do with expiry. Leaving this below the disabled-check would mean turning
	// retention off also turned off reaping the rows that describe nothing.
	if err := h.Queries.DeleteEmptyDatabaseTombstones(ctx); err != nil {
		slog.Warn("orphaned backup cleanup: failed to remove empty tombstones", "error", err)
	}

	days := h.orphanedBackupRetentionDays(ctx)
	if days == 0 {
		// Explicitly disabled. Deliberately silent at info level: this is a
		// choice an operator made, not a condition to report every day.
		slog.Debug("orphaned backup retention is disabled; keeping all")
		return
	}

	cutoff := pgtype.Timestamptz{Time: time.Now().AddDate(0, 0, -days), Valid: true}
	del := h.globalRemoteDeleter()
	removed := 0
	var lastFirstID pgtype.UUID

	// Paged, because each row costs a network round trip to its destination and
	// an install that has kept backups for months can have a lot of them. An
	// uncapped listing behind a per-row remote delete is the v0.1.3 shape: a
	// single task that grows without bound as the install ages.
	for {
		rows, err := h.Queries.ListExpiredOrphanedBackups(ctx, generated.ListExpiredOrphanedBackupsParams{
			DeletedAt: cutoff,
			Limit:     expiredBackupPageSize,
		})
		if err != nil {
			slog.Warn("orphaned backup cleanup: failed to list expired backups", "error", err)
			break
		}
		if len(rows) == 0 {
			break
		}
		// The listing has no offset, so each pass must shrink the set it walks.
		// deleteBackupArtifacts logs a failed row delete rather than reporting
		// it, so progress is measured instead: a page whose first row is the one
		// we just tried means nothing moved, and looping again would hand back
		// the same page forever.
		if rows[0].ID == lastFirstID {
			slog.Warn("orphaned backup cleanup: stopping, a page could not be removed",
				"remaining", len(rows))
			break
		}
		lastFirstID = rows[0].ID

		for _, b := range rows {
			h.deleteBackupArtifacts(ctx, b, del)
			removed++
		}
		if len(rows) < expiredBackupPageSize {
			break
		}
	}

	// Again, now that expiry has emptied some of them.
	if err := h.Queries.DeleteEmptyDatabaseTombstones(ctx); err != nil {
		slog.Warn("orphaned backup cleanup: failed to remove empty tombstones", "error", err)
	}

	if removed > 0 {
		slog.Info("orphaned backup cleanup: removed expired backups",
			"count", removed, "retention_days", days)
	}
}

// cleanupStalePreviews deletes preview applications whose last_activity_at is
// older than PreviewIdleDays. Delegates to ApplicationService.Delete which
// stops the container and cascades DB rows (domains, env, deployments); Caddy
// routes are reaped by the proxy reconciler on its next tick.
func (h *TaskHandler) cleanupStalePreviews(ctx context.Context) {
	if h.Config == nil || h.Config.PreviewIdleDays <= 0 || h.AppService == nil {
		return
	}
	cutoff := time.Now().Add(-time.Duration(h.Config.PreviewIdleDays) * 24 * time.Hour)
	stale, err := h.Queries.ListStalePreviewsWithProjectSlug(ctx, pgtype.Timestamptz{Time: cutoff, Valid: true})
	if err != nil {
		slog.Warn("preview GC: failed to list stale previews", "error", err)
		return
	}
	if len(stale) == 0 {
		return
	}

	removed := 0
	for _, row := range stale {
		if err := h.AppService.Delete(ctx, row.ID, row.ProjectSlug, row.Slug); err != nil {
			slog.Warn("preview GC: failed to delete preview",
				"application", row.Name, "error", err,
			)
			continue
		}
		slog.Info("preview GC: removed idle preview",
			"application", row.Name,
			"branch", row.Branch.String,
			"last_activity_at", row.LastActivityAt.Time,
		)
		removed++
	}
	if removed > 0 {
		slog.Info("preview GC: reaped idle previews", "count", removed, "cutoff_days", h.Config.PreviewIdleDays)
	}
}

// isRunningHelper reports whether this is a helper container that has not
// finished. Docker's state is "running" while it works and "created" in the
// instant before it starts; anything else means it is over.
func isRunningHelper(ctr runtime.ContainerInfo) bool {
	if ctr.Labels[runtime.LabelHelper] != "true" {
		return false
	}
	return ctr.Status == "running" || ctr.Status == "created"
}

// containerClaims is what the database says exists: the ids that can vouch for
// a container, plus the names that can vouch for one created before those ids
// were labelled onto it.
type containerClaims struct {
	applications map[string]bool
	databases    map[string]bool
	// names covers containers carrying neither id label. Belune ran before
	// either label existed and containers survive an upgrade untouched, so
	// those are still out there on real installs — the event watcher keeps the
	// same fallback for exactly this reason (see buildContainerIndex).
	names map[string]bool
}

// claimed reports whether a live row vouches for this container.
//
// An id label is the primary evidence, and it is decisive in both directions: a
// container naming an application or database that no longer exists is an
// orphan whatever the container is called. That is what makes this survive a
// container rename, and what covers databases structurally instead of by
// someone remembering to add them to a list.
//
// The name fallback is consulted only for containers carrying neither label —
// and a label that does not parse counts as no label, because a container we
// cannot read is not thereby garbage.
func (c containerClaims) claimed(ctr runtime.ContainerInfo) bool {
	if id, ok := labelledUUID(ctr.Labels, runtime.LabelApplicationID); ok {
		return c.applications[id]
	}
	if id, ok := labelledUUID(ctr.Labels, runtime.LabelDatabaseID); ok {
		return c.databases[id]
	}
	return c.names[ctr.Name]
}

// labelledUUID reads a label and normalises it, so a comparison never turns on
// how a particular writer happened to format the same UUID.
func labelledUUID(labels map[string]string, key string) (string, bool) {
	raw, present := labels[key]
	if !present {
		return "", false
	}
	parsed, err := parseUUID(raw)
	if err != nil {
		return "", false
	}
	id := formatUUID(parsed)
	return id, id != ""
}

// collectClaimsByServer reads every application and database into the set of
// things that can vouch for a container, grouped by the host they are placed
// on. A server with nothing placed on it gets an empty set, which is correct:
// every managed container there is then genuinely leftover.
//
// Every lookup returns an error rather than a partial set. A partial set does
// not skip work here, it deletes live containers — which is exactly how the
// daily run came to remove every managed database.
func (h *TaskHandler) collectClaimsByServer(ctx context.Context) (map[string]containerClaims, error) {
	byServer := map[string]containerClaims{}
	claimsFor := func(serverID pgtype.UUID) containerClaims {
		key := formatUUID(serverID)
		c, ok := byServer[key]
		if !ok {
			c = containerClaims{
				applications: map[string]bool{},
				databases:    map[string]bool{},
				names:        map[string]bool{},
			}
			byServer[key] = c
		}
		return c
	}

	apps, err := h.Queries.ListAllApplicationsWithProjectSlug(ctx)
	if err != nil {
		return nil, fmt.Errorf("list applications: %w", err)
	}
	for _, row := range apps {
		appID := formatUUID(row.ID)
		if appID == "" {
			continue
		}
		claims := claimsFor(row.ServerID)
		claims.applications[appID] = true
		// The pre-label fallback, and only that: a container carrying an
		// application-id is decided by the id above, so these names no longer
		// stand between a renamed container and the sweep.
		claims.names[naming.ContainerName(row.ProjectSlug, row.Slug, appID)] = true
		claims.names[naming.IntermediateContainerName(row.ProjectSlug, appID)] = true
		claims.names[naming.OldContainerName(appID)] = true
	}

	databases, err := h.Queries.ListAllDatabasesWithServerID(ctx)
	if err != nil {
		return nil, fmt.Errorf("list databases: %w", err)
	}
	for _, db := range databases {
		claims := claimsFor(db.ServerID)
		if dbID := formatUUID(db.ID); dbID != "" {
			claims.databases[dbID] = true
		}
		// The slug is the container name — see provision_db_task.go.
		claims.names[db.Slug] = true
	}

	return byServer, nil
}

// cleanupOrphanContainers removes managed containers that belong to nothing in
// the database, on every host Belune manages.
//
// ⚠️ This sweep deletes what it cannot match, so every way of matching has to
// be right at once. It matches by the id labels a container carries, falling
// back to names for containers older than those labels; helpers still at work
// are spared by a third label. Anything that lists containers here without
// teaching this function to recognise them will destroy them.
func (h *TaskHandler) cleanupOrphanContainers(ctx context.Context) {
	servers, err := h.Queries.ListManagedServers(ctx)
	if err != nil {
		slog.Warn("orphan cleanup: failed to list servers", "error", err)
		return
	}
	if len(servers) == 0 {
		// Not reachable by any current path — the local server is seeded active
		// and nothing revokes it — but if it ever were, reaping would stop dead
		// and every other exit here logs. Silence is the one outcome nobody
		// could diagnose.
		slog.Warn("orphan cleanup: no managed servers, so nothing was swept")
		return
	}

	claimsByServer, err := h.collectClaimsByServer(ctx)
	if err != nil {
		slog.Warn("orphan cleanup: failed to read what the database claims", "error", err)
		return
	}

	// There is deliberately no "refuse when the allowlist is empty" guard here.
	// It reads like cheap insurance, but the lookups above return on error, so
	// an empty set is not a failed build — it is a host with no applications and
	// no databases on it, where every managed container genuinely is leftover.
	// Refusing there stalls reaping forever on exactly the install that needs
	// it, and catches nothing that can actually happen. What protects the
	// dangerous case is elsewhere: every lookup returns rather than continuing,
	// an unreachable host is skipped rather than swept, and helpers still at
	// work are spared by label.

	// Merged across every host, as a veto below.
	anywhere := containerClaims{
		applications: map[string]bool{},
		databases:    map[string]bool{},
		names:        map[string]bool{},
	}
	for _, c := range claimsByServer {
		maps.Copy(anywhere.applications, c.applications)
		maps.Copy(anywhere.databases, c.databases)
		maps.Copy(anywhere.names, c.names)
	}

	removed := 0
	for _, server := range servers {
		removed += h.reapOrphansOn(ctx, server, claimsByServer[formatUUID(server.ID)], anywhere)
	}

	if removed > 0 {
		slog.Info("orphan cleanup: removed containers", "count", removed)
	}
}

// reapOrphansOn sweeps a single host, comparing what is running there against
// what the database places there. Only containers older than 1 hour are
// considered, to avoid racing with an in-progress deployment.
//
// A host that cannot be reached or listed is skipped, never swept: "I could not
// ask" must not read as "nothing there is claimed".
func (h *TaskHandler) reapOrphansOn(ctx context.Context, server generated.Server, claims, anywhere containerClaims) int {
	const orphanAge = time.Hour

	rt, err := h.Runtimes.For(ctx, server.ID)
	if err != nil {
		slog.Warn("orphan cleanup: failed to reach a server", "server", server.Name, "error", err)
		return 0
	}

	containers, err := rt.ListContainers(ctx)
	if err != nil {
		slog.Warn("orphan cleanup: failed to list containers", "server", server.Name, "error", err)
		return 0
	}

	removed, claimedElsewhere := 0, 0
	for _, ctr := range containers {
		if claims.claimed(ctr) {
			continue
		}
		// ⚠️ Claimed, but by a different host. Until the resolver can actually
		// tell hosts apart every server id resolves to this same daemon, so
		// without this a second server row would make each host's sweep delete
		// the other's containers — the whole install, on the second pass. It
		// stays right once the resolver is real, too: a container running here
		// but claimed there is a misplacement to look into, not garbage.
		if anywhere.claimed(ctr) {
			// Debug per container, one summary below: while the resolver still
			// returns the same daemon for every server id this is the expected
			// case for every container on the box, and a line each would bury
			// the genuine warnings this function emits.
			slog.Debug("orphan cleanup: container is claimed by a different server; leaving it alone",
				"container", ctr.Name, "server", server.Name)
			claimedElsewhere++
			continue
		}
		// A helper doing work inside a volume can never be claimed: it has no
		// name of its own and no row to vouch for it. While it is running it is
		// not an orphan, it is a job in progress — and a volume restore killed
		// between `find -delete` and `tar x` leaves the volume empty. One that
		// has exited is genuinely leftover and falls through to be reaped.
		if isRunningHelper(ctr) {
			slog.Debug("orphan cleanup: skipping a helper still at work", "container", ctr.Name, "server", server.Name)
			continue
		}
		if time.Since(ctr.CreatedAt) < orphanAge {
			slog.Debug("orphan cleanup: skipping recent container", "container", ctr.Name, "age", time.Since(ctr.CreatedAt).Round(time.Second))
			continue
		}
		slog.Info("orphan cleanup: removing container", "container", ctr.Name, "server", server.Name, "created_at", ctr.CreatedAt)
		if err := rt.StopContainer(ctx, ctr.Name); err != nil {
			slog.Debug("orphan cleanup: stop failed (may already be stopped)", "container", ctr.Name, "error", err)
		}
		if err := rt.RemoveContainer(ctx, ctr.Name); err != nil {
			slog.Warn("orphan cleanup: failed to remove container", "container", ctr.Name, "error", err)
			continue
		}
		removed++
	}

	if claimedElsewhere > 0 {
		slog.Warn("orphan cleanup: containers on this host are claimed by another server",
			"server", server.Name, "count", claimedElsewhere)
	}
	return removed
}

func formatUUID(u pgtype.UUID) string {
	if !u.Valid {
		return ""
	}
	b := u.Bytes
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
