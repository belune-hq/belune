package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/hibiken/asynq"
	"github.com/jackc/pgx/v5/pgtype"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/belune-hq/belune/internal/build"
	"github.com/belune-hq/belune/internal/git"
	"github.com/belune-hq/belune/internal/instances"
	"github.com/belune-hq/belune/internal/naming"
	"github.com/belune-hq/belune/internal/notify"
	"github.com/belune-hq/belune/internal/pkg/buildlog"
	"github.com/belune-hq/belune/internal/pkg/metrics"
	"github.com/belune-hq/belune/internal/pkg/redact"
	"github.com/belune-hq/belune/internal/pkg/tracing"
	"github.com/belune-hq/belune/internal/proxy"
	"github.com/belune-hq/belune/internal/runtime"
	"github.com/belune-hq/belune/internal/status"
	"github.com/belune-hq/belune/internal/store"
	"github.com/belune-hq/belune/internal/store/generated"
)

type deployPayload struct {
	ApplicationID    string            `json:"application_id"`
	DeploymentID     string            `json:"deployment_id"`
	RollbackImageTag string            `json:"rollback_image_tag,omitempty"` // non-empty = skip build, redeploy this image
	CommitSHA        string            `json:"commit_sha,omitempty"`         // non-empty = rebuild this exact commit instead of branch HEAD
	TraceCarrier     map[string]string `json:"trace_carrier,omitempty"`      // W3C trace context for span linking across the queue
}

// deployTarget is one container this deploy creates: one process type's single
// instance. Everything that varies per type lives here, so no stage reads a shared
// field that the next container's create would overwrite (the compensator bug).
type deployTarget struct {
	typeName      string
	command       string         // "" leaves the image's CMD alone
	health        resolvedHealth // resolved by resolveHealthCheck; nothing reads app.HealthCheckType
	instanceID    pgtype.UUID
	instanceIdx   int32
	containerName string // read from the instance row, never minted
	containerID   string // set once the container is created
	routable      bool   // an instance of the routable type
	// prevName is the container this one REPLACES during a roll, and "" when the
	// target is created in place (workers, stateful apps, a first deploy). While
	// rolling, containerName is already the NEW container's name and the instance
	// row still describes the old one until cutover. prevExists is whether the old
	// container is actually there to be drained.
	prevName   string
	prevExists bool
	// cpuLimit and memoryLimit are what the container is created with: the process
	// type's own when set, otherwise the application's (NULL on the type means
	// inherit). 0 is unlimited, as on the application.
	cpuLimit    float64
	memoryLimit int64
}

// limitsFor resolves a type's effective CPU and memory limits. The rule itself lives in
// instances.EffectiveLimits, shared with the metrics ceiling.
func limitsFor(app generated.Application, pt generated.ApplicationProcessType) (cpu float64, memory int64) {
	return instances.EffectiveLimits(app.CpuLimit, app.MemoryLimit, pt.CpuLimit, pt.MemoryLimit)
}

// all returns every target, workers first and the routable ones last: the order
// they are deployed in.
func (dc *deployContext) all() []*deployTarget {
	out := append(append([]*deployTarget{}, dc.workers...), dc.routable)
	return append(out, dc.routableRest...)
}

// routables returns every instance of the routable type in idx order, the first being
// dc.routable. This is the route's upstream list.
func (dc *deployContext) routables() []*deployTarget {
	return append([]*deployTarget{dc.routable}, dc.routableRest...)
}

// routableNames is the upstream container list, in idx order.
func (dc *deployContext) routableNames() []string {
	ts := dc.routables()
	names := make([]string, 0, len(ts))
	for _, t := range ts {
		names = append(names, t.containerName)
	}
	return names
}

// deployContext holds all state accumulated across deploy stages.
type deployContext struct {
	payload       deployPayload
	applicationID pgtype.UUID
	deploymentID  pgtype.UUID
	appRow        generated.GetApplicationWithProjectSlugRow
	app           generated.Application
	// runtime is the host this application is placed on, resolved once in
	// loadApplication so no stage can re-resolve to a different host midway.
	// ⚠️ It does NOT yet cover the build: h.Chain is still wired to the local
	// Docker client, so a git-source app placed on a remote host would build
	// its image here and then fail to find that tag there. Offloading the
	// build is its own piece of work (BUILDKIT_HOST), not this refactor.
	runtime runtime.ContainerRuntime
	// routable is the ONE container the proxy routes to; workers are every other
	// process type, in deterministic (name) order. All run the SAME image. The
	// routable path -- create, wire proxy, verify, finalize -- is the sequence proven
	// on a real install, so workers are wrapped around it and never mixed into it.
	//
	// With replicas the routable type has several instances: routable is the lowest
	// idx (the one the health gate probes first and the logs name) and routableRest
	// the others, in idx order. workers holds one target per INSTANCE of every
	// non-routable type with replicas > 0.
	routable     *deployTarget
	routableRest []*deployTarget
	workers      []*deployTarget
	env          map[string]string
	imageName    string
	commitSHA    string
	domains      []generated.Domain
	// cleanedUp is set once the previous container has been torn down. Before
	// that point a failed deploy has left the old container serving, so the
	// application is still up and must not be flagged as errored.
	cleanedUp bool
	// buildOnly marks the standalone build task, which produces an image but
	// never touches the running container — so a failure never changes the
	// application's status.
	buildOnly bool
	// compensators are cleanup functions appended as resources are created.
	// On any post-creation failure they run in reverse order.
	compensators []func()
}

// HandleDeployTask is the asynq task handler for application deployments.
// It drives a sequence of named stages; any failure after resource creation
// triggers the compensating cleanup chain.
func (h *TaskHandler) HandleDeployTask(ctx context.Context, t *asynq.Task) error {
	var payload deployPayload
	if err := json.Unmarshal(t.Payload(), &payload); err != nil {
		return fmt.Errorf("unmarshal deploy payload: %w", err)
	}

	// Restore trace context from the enqueue site so the worker's spans
	// appear as children of the HTTP handler that triggered this deploy.
	ctx = tracing.ExtractContext(ctx, payload.TraceCarrier)
	ctx, rootSpan := tracing.Tracer().Start(ctx, "deploy.run",
		trace.WithAttributes(
			attribute.String("application.id", payload.ApplicationID),
			attribute.String("deployment.id", payload.DeploymentID),
			attribute.Bool("deploy.is_rollback", payload.RollbackImageTag != ""),
		),
	)
	defer rootSpan.End()

	slog.Info("handling deploy task", "application_id", payload.ApplicationID, "deployment_id", payload.DeploymentID)

	applicationID, err := parseUUID(payload.ApplicationID)
	if err != nil {
		recordSpanErr(rootSpan, err)
		return errors.Join(fmt.Errorf("invalid application_id (permanent): %w", err), asynq.SkipRetry)
	}
	deploymentID, err := parseUUID(payload.DeploymentID)
	if err != nil {
		recordSpanErr(rootSpan, err)
		return errors.Join(fmt.Errorf("invalid deployment_id (permanent): %w", err), asynq.SkipRetry)
	}

	// Defense-in-depth: if the deployment row is already in a terminal state,
	// a duplicate task slipped past webhook dedup and the asynq TaskID lock
	// (e.g., a stale retry after the first run finished). Skip without
	// retrying instead of clobbering the existing success/failure.
	existing, err := h.Queries.GetDeployment(ctx, deploymentID)
	if err != nil {
		recordSpanErr(rootSpan, err)
		return errors.Join(fmt.Errorf("fetch deployment (permanent): %w", err), asynq.SkipRetry)
	}
	if existing.Status == status.DeploymentSuccess || existing.Status == status.DeploymentFailed {
		rootSpan.SetAttributes(attribute.String("deploy.skipped_reason", "already_terminal"))
		slog.Info("deploy task skipped: deployment already terminal",
			"deployment_id", payload.DeploymentID,
			"status", existing.Status,
		)
		return nil
	}

	dc := &deployContext{
		payload:       payload,
		applicationID: applicationID,
		deploymentID:  deploymentID,
	}

	// Stage 1: load application and decrypt env vars — permanent failure if missing
	if err := runStage(ctx, "deploy.load_application", func(ctx context.Context) error {
		return h.loadApplication(ctx, dc)
	}); err != nil {
		h.failDeployment(ctx, dc, "deploy", fmt.Sprintf("load application: %v", err))
		recordSpanErr(rootSpan, err)
		return errors.Join(fmt.Errorf("load application (permanent): %w", err), asynq.SkipRetry)
	}

	// Stage 1.5: defence-in-depth quota check. Quotas are validated at
	// application-creation time, but limits may have been tightened since,
	// or new previews may have pushed the parent's project over its cap. We
	// verify *current* aggregate usage is still within scope here so a
	// shrunken quota stops further deploys instead of silently overflowing.
	if err := runStage(ctx, "deploy.check_quota", func(ctx context.Context) error {
		return h.checkDeployQuota(ctx, dc)
	}); err != nil {
		h.failDeployment(ctx, dc, "deploy", fmt.Sprintf("quota check: %v", err))
		recordSpanErr(rootSpan, err)
		return errors.Join(fmt.Errorf("quota check (permanent): %w", err), asynq.SkipRetry)
	}

	// Stage 2: build or pull the image FIRST, while the previous container is
	// still running. A build can take minutes, and doing it before teardown
	// means the app keeps serving throughout — and a failed build never takes
	// the running app down, because cleanup below has not happened yet.
	if err := runStage(ctx, "deploy.prepare_image", func(ctx context.Context) error {
		return h.prepareImage(ctx, dc)
	}); err != nil {
		h.failDeployment(ctx, dc, "deploy", fmt.Sprintf("%v", err))
		recordSpanErr(rootSpan, err)
		return err
	}

	// Stage 3: networking. Idempotent and log-only on error. Shared by every
	// process type: they all live on the application's one project network.
	_ = runStage(ctx, "deploy.ensure_networks", func(ctx context.Context) error {
		h.ensureNetworks(ctx, dc)
		return nil
	})

	// Stage 4: the NON-ROUTABLE process types, one at a time, in name order, BEFORE
	// the routable one is touched. They all run the same image, so if a worker cannot
	// start on it the web type almost certainly cannot either, and because web's
	// container has not been torn down yet the app is still serving the old version
	// when the deploy fails. That is a canary, not an accident of ordering.
	//
	// Failure policy for 0.3.0 (there is no rolling infrastructure until Phase D):
	// the deployment is marked failed, and the compensators remove the containers
	// THIS deploy created. A worker whose old container was already removed before
	// its new one failed is DOWN, not old-generation-serving, exactly as a
	// single-container deploy has always behaved.
	for _, w := range dc.workers {
		if err := runStage(ctx, "deploy.worker_"+w.typeName, func(ctx context.Context) error {
			removeContainer(ctx, dc, w.containerName)
			if err := h.createAndStart(ctx, dc, w); err != nil {
				return fmt.Errorf("create container: %w", err)
			}
			return h.verifyWorker(ctx, dc, w)
		}); err != nil {
			h.runCompensators(dc)
			h.failDeployment(ctx, dc, "deploy", fmt.Sprintf("process type %q: %v", w.typeName, err))
			recordSpanErr(rootSpan, err)
			return fmt.Errorf("process type %q: %w", w.typeName, err)
		}
	}

	// Stage 5: the routable type. A stateless application ROLLS: the new generation is
	// created beside the old, health-gated, and only then does the route move, so a
	// failure at any point leaves the old generation serving. An application with
	// persistent volumes does not: two containers writing one volume is a data hazard
	// (two databases on one data directory), so it keeps the stop-then-create sequence
	// that has always been proven for it.
	var routeErr error
	if h.shouldRoll(ctx, dc) {
		routeErr = h.rollRoutable(ctx, dc, rootSpan)
	} else {
		routeErr = h.recreateRoutable(ctx, dc, rootSpan)
	}
	if routeErr != nil {
		return routeErr
	}

	rootSpan.SetAttributes(
		attribute.String("container.id", dc.routable.containerID),
		attribute.String("deploy.commit_sha", dc.commitSHA),
	)
	rootSpan.SetStatus(codes.Ok, "deploy completed")

	slog.Info("deploy completed",
		"application_id", payload.ApplicationID,
		"deployment_id", payload.DeploymentID,
		"container", dc.routable.containerID,
		"commit", dc.commitSHA,
	)
	return nil
}

// recreateRoutable is the stop-then-create sequence, kept for applications that must
// not run two generations at once (persistent volumes). Stage 5..8 as they have always
// been: this is the point of no return, so once cleanup has run a failure flags the
// application errored.
func (h *TaskHandler) recreateRoutable(ctx context.Context, dc *deployContext, rootSpan trace.Span) error {
	payload := dc.payload
	// Stage 5: the routable type -- the sequence proven on a real install, unchanged.
	// This is the point of no return for the application: tearing down the previous
	// container leaves nothing serving, so from here a failure flags the app errored.
	// The application-scoped legacy names are swept once, here, with the container
	// they belong to; sweeping them earlier would take that container down before
	// the workers had proven the image.
	_ = runStage(ctx, "deploy.cleanup_existing", func(ctx context.Context) error {
		h.cleanupLegacyNames(ctx, dc)
		for _, t := range dc.routables() {
			removeContainer(ctx, dc, t.containerName)
		}
		dc.cleanedUp = true
		return nil
	})

	// create and start container — appends a compensator on success
	if err := runStage(ctx, "deploy.create_and_start", func(ctx context.Context) error {
		for _, t := range dc.routables() {
			if err := h.createAndStart(ctx, dc, t); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		h.runCompensators(dc)
		h.failDeployment(ctx, dc, "deploy", fmt.Sprintf("create container: %v", err))
		recordSpanErr(rootSpan, err)
		return fmt.Errorf("create container: %w", err)
	}

	// Stage 6: wire proxy routes — appends a compensator per successful AddRoute
	if err := runStage(ctx, "deploy.wire_proxy", func(ctx context.Context) error {
		return h.wireProxy(ctx, dc)
	}); err != nil {
		h.runCompensators(dc)
		h.failDeployment(ctx, dc, "deploy", fmt.Sprintf("wire proxy: %v", err))
		recordSpanErr(rootSpan, err)
		return fmt.Errorf("wire proxy: %w", err)
	}

	// Stage 7: post-route health verification — polls the container's health
	// endpoint (direct, not via Caddy) for up to 2 minutes. A pass marks the
	// deployment healthy; a fail rolls back via compensators so a broken
	// build never silently replaces a working one. Apps with no
	// HealthCheckPath skip this stage and are recorded as 'skipped'.
	if err := runStage(ctx, "deploy.verify_health", func(ctx context.Context) error {
		return h.verifyHealth(ctx, dc)
	}); err != nil {
		h.runCompensators(dc)
		h.failDeployment(ctx, dc, "deploy", fmt.Sprintf("health check failed: %v", err))
		recordSpanErr(rootSpan, err)
		return fmt.Errorf("health check: %w", err)
	}

	// Stage 8: atomic finalize — mark deployment success + application running
	if err := runStage(ctx, "deploy.finalize", func(ctx context.Context) error {
		return h.finalize(ctx, dc)
	}); err != nil {
		slog.Error("failed to commit deploy success status", "application_id", payload.ApplicationID, "error", err)
		recordSpanErr(rootSpan, err)
	} else {
		h.notifyDeployment(ctx, dc.deploymentID, status.DeploymentSuccess, "deploy", "")
	}

	return nil
}

// runStage wraps a deploy stage in its own span so per-stage latency and
// failures are attributable in traces. Emits a histogram sample per run so
// operators can track per-stage p95 / success ratios.
func runStage(ctx context.Context, name string, fn func(context.Context) error) error {
	ctx, span := tracing.Tracer().Start(ctx, name)
	defer span.End()
	start := time.Now()
	err := fn(ctx)
	resultLabel := "ok"
	if err != nil {
		resultLabel = "error"
		recordSpanErr(span, err)
	}
	metrics.RecordDeployStage(name, resultLabel, time.Since(start))
	return err
}

// recordSpanErr tags the span as failed and attaches the error.
func recordSpanErr(span trace.Span, err error) {
	span.RecordError(err)
	span.SetStatus(codes.Error, err.Error())
}

// loadApplication fetches the application row, maps it, and decrypts env vars.
func (h *TaskHandler) loadApplication(ctx context.Context, dc *deployContext) error {
	appRow, err := h.Queries.GetApplicationWithProjectSlug(ctx, dc.applicationID)
	if err != nil {
		return fmt.Errorf("fetch application: %w", err)
	}
	dc.appRow = appRow
	dc.app = generated.Application{
		ID: appRow.ID, ProjectID: appRow.ProjectID, Name: appRow.Name,
		Type: appRow.Type, SourceRepo: appRow.SourceRepo, SourceImage: appRow.SourceImage,
		DockerfilePath: appRow.DockerfilePath, BuildType: appRow.BuildType,
		BuilderImage: appRow.BuilderImage, CustomBuildpacks: appRow.CustomBuildpacks,
		Status:   appRow.Status,
		CpuLimit: appRow.CpuLimit, MemoryLimit: appRow.MemoryLimit,
		// Without this the connected-account clone path is silently skipped: the
		// deploy falls back to an unauthenticated clone and fails on a private repo
		// with "could not read Username". CloneToken is never even reached.
		GitIntegrationID:              appRow.GitIntegrationID,
		GitCredentialsEncrypted:       appRow.GitCredentialsEncrypted,
		HealthCheckPath:               appRow.HealthCheckPath,
		HealthCheckTimeoutSeconds:     appRow.HealthCheckTimeoutSeconds,
		HealthCheckExpectStatus:       appRow.HealthCheckExpectStatus,
		HealthCheckType:               appRow.HealthCheckType,
		HealthCheckCommand:            appRow.HealthCheckCommand,
		HealthCheckIntervalSeconds:    appRow.HealthCheckIntervalSeconds,
		HealthCheckRetries:            appRow.HealthCheckRetries,
		HealthCheckStartPeriodSeconds: appRow.HealthCheckStartPeriodSeconds,
		ReadonlyRootfs:                appRow.ReadonlyRootfs,
		ContainerCaps:                 appRow.ContainerCaps,
		ContainerPort:                 appRow.ContainerPort,
		RootDirectory:                 appRow.RootDirectory,
	}
	// READ the names from the instance rows; never mint one. Deploy is stop ->
	// remove -> create, all on one name, so minting a different name here would leave
	// the old container beside the new one. A row written before the roll holds the
	// legacy name (applications.slug), which is what keeps old installs invisible.
	//
	// Every instance of the routable type, in idx order (SQL's order, not re-sorted:
	// it is the route's upstream order).
	routableRows, err := h.Queries.ListRoutableInstancesByApplication(ctx, appRow.ID)
	if err != nil {
		return fmt.Errorf("resolve instances: %w", err)
	}
	if len(routableRows) == 0 {
		return fmt.Errorf("resolve instance: %w", instances.ErrNoInstance)
	}
	pt, err := h.Queries.GetProcessType(ctx, routableRows[0].ProcessTypeID)
	if err != nil {
		return fmt.Errorf("load process type: %w", err)
	}
	cpu, mem := limitsFor(dc.app, pt)
	target := func(t generated.ApplicationProcessType, in generated.ApplicationInstance, cpu float64, mem int64) *deployTarget {
		return &deployTarget{
			typeName:      t.Name,
			command:       t.Command,
			health:        resolveHealthCheck(dc.app, t),
			instanceID:    in.ID,
			instanceIdx:   in.Idx,
			containerName: in.ContainerName,
			cpuLimit:      cpu,
			memoryLimit:   mem,
		}
	}
	dc.routable = target(pt, routableRows[0], cpu, mem)
	dc.routable.routable = true
	for _, in := range routableRows[1:] {
		rt := target(pt, in, cpu, mem)
		rt.routable = true
		dc.routableRest = append(dc.routableRest, rt)
	}

	// Every other process type, from the same image. Resolved HERE, before anything
	// is torn down, so a type that cannot be resolved fails the deploy while the old
	// generation is still serving. No instance row is ever created by a deploy.
	types, err := h.Queries.ListProcessTypesByApplication(ctx, dc.applicationID)
	if err != nil {
		return fmt.Errorf("list process types: %w", err)
	}
	for _, t := range types { // ORDER BY routable DESC, name: workers come out by name
		if t.ID == pt.ID {
			continue
		}
		rows, err := h.Queries.ListInstancesByProcessType(ctx, t.ID)
		if err != nil {
			return fmt.Errorf("list instances of process type %q: %w", t.Name, err)
		}
		// replicas = 0 is a PAUSED type: zero rows is its correct state, and it must
		// not fail the deploy (it would make every deploy after a pause fail). The
		// error is kept for the case it was written for: a type that should run and
		// has no row, which is a bug.
		if t.Replicas == 0 {
			continue
		}
		if len(rows) == 0 {
			return fmt.Errorf("process type %q has no instance row", t.Name)
		}
		cpu, mem := limitsFor(dc.app, t)
		for _, in := range rows { // ORDER BY idx
			dc.workers = append(dc.workers, target(t, in, cpu, mem))
		}
	}

	rt, err := h.Runtimes.For(ctx, appRow.ServerID)
	if err != nil {
		return fmt.Errorf("resolve runtime: %w", err)
	}
	dc.runtime = rt

	// Decrypt env vars: project-level first (base), then app-level (override)
	env := make(map[string]string)
	projectEnvVars, err := h.Queries.ListProjectEnvVars(ctx, dc.app.ProjectID)
	if err != nil {
		slog.Warn("failed to fetch project env vars, continuing without them", "error", err)
	}
	for _, ev := range projectEnvVars {
		decrypted, err := h.Keyring.Decrypt(ev.ValueEncrypted)
		if err != nil {
			slog.Warn("failed to decrypt project env var, skipping", "key", ev.Key, "error", err)
			continue
		}
		env[ev.Key] = string(decrypted)
	}

	appEnvVars, err := h.Queries.ListEnvVarsByApplication(ctx, dc.applicationID)
	if err != nil {
		slog.Warn("failed to fetch app env vars, continuing without them", "error", err)
	}
	for _, ev := range appEnvVars {
		decrypted, err := h.Keyring.Decrypt(ev.ValueEncrypted)
		if err != nil {
			slog.Warn("failed to decrypt env var, skipping", "key", ev.Key, "error", err)
			continue
		}
		env[ev.Key] = string(decrypted)
	}
	dc.env = env
	return nil
}

// removeContainer stops and removes one container by name; log-only on error,
// because it may not exist.
func removeContainer(ctx context.Context, dc *deployContext, name string) {
	if err := dc.runtime.StopContainer(ctx, name); err != nil {
		slog.Debug("could not stop container before deploy (may not exist)", "container", name, "error", err)
	}
	if err := dc.runtime.RemoveContainer(ctx, name); err != nil {
		slog.Debug("could not remove container before deploy (may not exist)", "container", name, "error", err)
	}
}

// cleanupLegacyNames sweeps the two pre-rename container names. They are scoped to
// the APPLICATION, not to a process type: they name the single container that
// existed before the renames, which the routable type now owns. So it runs once,
// with the routable type's own cleanup, and is not repeated per type.
func (h *TaskHandler) cleanupLegacyNames(ctx context.Context, dc *deployContext) {
	removeContainer(ctx, dc, naming.OldContainerName(dc.payload.ApplicationID))
	removeContainer(ctx, dc, naming.IntermediateContainerName(dc.appRow.ProjectSlug, dc.payload.ApplicationID))
}

// ensureNetworks creates the project-scoped network and attaches Caddy to it
// so the reverse proxy can reach this project's containers. Both operations
// are idempotent.
//
// v0.0.9-alpha Phase 2: stopped attaching app containers to the shared
// `belune-infra` network. Apps in different projects can no longer see each
// other on the Docker network plane. Caddy still bridges them by joining
// each project network on demand.
func (h *TaskHandler) ensureNetworks(ctx context.Context, dc *deployContext) {
	projectNetwork := naming.ProjectNetworkName(dc.appRow.ProjectSlug)
	if err := dc.runtime.CreateNetwork(ctx, projectNetwork); err != nil {
		slog.Debug("could not create project network (may already exist)", "network", projectNetwork, "error", err)
	}
	if name := h.Config.CaddyContainerName; name != "" {
		if err := dc.runtime.ConnectContainerToNetwork(ctx, name, projectNetwork); err != nil {
			// Caddy may legitimately be on a different orchestration plane
			// (e.g. running on the host, not in Docker). Warn rather than
			// fail — the deploy itself can still succeed.
			slog.Warn("could not attach caddy to project network", "caddy", name, "network", projectNetwork, "error", err)
		}
	}
	// Attach the control-plane container too, so the post-deploy health probe
	// (verifyHealth) can reach the app container by name on its project network.
	// App-to-app isolation is unaffected — only the trusted control plane bridges
	// in, which it must to health-check. Idempotent; warn (not fail) when the API
	// runs outside Docker.
	if name := h.Config.APIContainerName; name != "" {
		if err := dc.runtime.ConnectContainerToNetwork(ctx, name, projectNetwork); err != nil {
			slog.Warn("could not attach control-plane container to project network", "api", name, "network", projectNetwork, "error", err)
		}
	}
}

// prepareImage resolves the image to deploy: rollback, image-pull, or git-build path.
func (h *TaskHandler) prepareImage(ctx context.Context, dc *deployContext) error {
	if dc.payload.RollbackImageTag != "" {
		dc.imageName = dc.payload.RollbackImageTag
		slog.Info("rolling back to image", "image", dc.imageName, "application_id", dc.payload.ApplicationID)
		h.updateDeploymentStatus(ctx, dc.deploymentID, status.DeploymentPending, status.DeploymentDeploying)
		return nil
	}

	switch dc.app.Type {
	case "image":
		dc.imageName = dc.app.SourceImage.String
		// Image apps have no build, but the pull is the only interesting step, so
		// stream it to the same build-logs channel the UI already renders (git
		// builds use it too). Mark the deployment "building" during the pull so the
		// frontend subscribes and shows the live/persisted log instead of a blank
		// panel.
		h.updateDeploymentStatus(ctx, dc.deploymentID, status.DeploymentPending, status.DeploymentBuilding)
		pub := buildlog.NewPublisher(h.RedisClient, dc.payload.DeploymentID)
		sink := buildlog.NewLogSink(pub, ctx)
		out := sink.Writer("stdout")
		persistPullLog := func() {
			sink.Flush()
			pub.Close(ctx)
			if b := sink.NDJSON(); b != "" {
				if err := h.Queries.UpdateDeploymentBuildLogs(ctx, generated.UpdateDeploymentBuildLogsParams{
					ID:        dc.deploymentID,
					BuildLogs: pgtype.Text{String: b, Valid: true},
				}); err != nil {
					slog.Error("could not store the build logs", "error", err)
				}
			}
		}

		fmt.Fprintf(out, "Pulling image %s\n", dc.imageName)
		slog.Info("pulling image", "image", dc.imageName)
		pullCtx, pullCancel := context.WithTimeout(ctx, time.Duration(h.Config.ImagePullTimeoutMinutes)*time.Minute)
		if err := dc.runtime.PullImage(pullCtx, dc.imageName); err != nil {
			pullCancel()
			fmt.Fprintf(out, "Failed to pull image: %v\n", err)
			persistPullLog()
			return fmt.Errorf("pull image: %w", err)
		}
		// Pin to the resolved digest so a later Reload/rollback recreates the
		// exact image that was deployed, not whatever the mutable tag (e.g.
		// :latest) points at by then. Falls back to the tag if no repo digest.
		if digest, derr := dc.runtime.ResolveImageDigest(pullCtx, dc.imageName); derr != nil {
			slog.Warn("could not resolve image digest; using tag", "image", dc.imageName, "error", derr)
		} else if digest != "" {
			fmt.Fprintf(out, "Pinned to digest %s\n", digest)
			slog.Info("pinned image to digest", "image", dc.imageName, "digest", digest)
			dc.imageName = digest
		}
		pullCancel()
		fmt.Fprintln(out, "Image ready; starting container")
		persistPullLog()

	case "git":
		if err := h.buildFromGit(ctx, dc); err != nil {
			return err
		}

	default:
		return fmt.Errorf("unknown application type %s: %w", dc.app.Type, asynq.SkipRetry)
	}

	// Store the image tag for future rollback support.
	if err := h.Queries.UpdateDeploymentImageTag(ctx, generated.UpdateDeploymentImageTagParams{
		ID:       dc.deploymentID,
		ImageTag: pgtype.Text{String: dc.imageName, Valid: true},
	}); err != nil {
		slog.Error("could not record the image tag; rollback will have none", "error", err)
	}
	return nil
}

// buildFromGit clones the repository, runs the build chain, and sets dc.imageName.
func (h *TaskHandler) buildFromGit(ctx context.Context, dc *deployContext) error {
	dc.imageName = naming.ImageTag(dc.appRow.ProjectSlug, dc.appRow.Slug, dc.payload.ApplicationID, dc.payload.DeploymentID)

	tmpDir, err := os.MkdirTemp("", "belune-build-*")
	if err != nil {
		return fmt.Errorf("create temp dir: %w", err)
	}
	defer func() {
		if err := os.RemoveAll(tmpDir); err != nil {
			slog.Error("could not remove the build directory", "error", err)
		}
	}()

	buildCtx, buildCancel := context.WithTimeout(ctx, time.Duration(h.Config.BuildTimeoutMinutes)*time.Minute)
	defer buildCancel()

	// Resolve git credentials. A connected provider integration takes priority;
	// otherwise fall back to the app-level PAT, then to an unauthenticated clone.
	var cloneURL string
	if dc.app.GitIntegrationID.Valid && h.GitIntegrationService != nil {
		token, provider, username, err := h.GitIntegrationService.CloneToken(ctx, dc.app.GitIntegrationID, dc.app.SourceRepo.String)
		if err != nil {
			slog.Warn("failed to mint integration clone token, falling back to app token", "error", err)
		} else {
			cloneURL = git.BuildCloneURL(provider, token, username, dc.app.SourceRepo.String)
		}
	}
	if cloneURL == "" && len(dc.app.GitCredentialsEncrypted) > 0 {
		tokenBytes, decErr := h.Keyring.Decrypt(dc.app.GitCredentialsEncrypted)
		if decErr != nil {
			slog.Warn("failed to decrypt git credentials, cloning without token", "error", decErr)
		} else {
			cloneURL = git.BuildCloneURL("generic", string(tokenBytes), "", dc.app.SourceRepo.String)
		}
	}
	if cloneURL == "" {
		cloneURL = dc.app.SourceRepo.String
	}

	// Previews pin the clone to their branch; parents (branch unset) clone the
	// default ref. Token is empty here because cloneURL already embeds it when
	// credentials are present.
	branch := dc.appRow.Branch.String
	var cloneResult *git.CloneResult
	if dc.payload.CommitSHA != "" {
		// Rebuild: re-checkout the exact currently-deployed commit, not branch HEAD.
		slog.Info("cloning repository at pinned commit", "repo", dc.app.SourceRepo.String, "dest", tmpDir, "commit", dc.payload.CommitSHA)
		cloneResult, err = git.CloneCommit(buildCtx, cloneURL, tmpDir, dc.payload.CommitSHA, "")
	} else {
		slog.Info("cloning repository", "repo", dc.app.SourceRepo.String, "dest", tmpDir, "branch", branch)
		cloneResult, err = git.Clone(buildCtx, cloneURL, tmpDir, branch, "")
	}
	if err != nil {
		return fmt.Errorf("git clone: %w", err)
	}
	dc.commitSHA = cloneResult.CommitSHA
	slog.Info("cloned repository", "commit", dc.commitSHA)

	buildDir, err := resolveBuildDir(tmpDir, dc.app.RootDirectory.String)
	if err != nil {
		return err
	}

	// Record the built commit so Rebuild can re-checkout the deployed commit and
	// so deployment history shows it (webhook pushes set it at create time, but
	// manual/reload/rebuild deploys otherwise wouldn't).
	if dc.commitSHA != "" {
		if err := h.Queries.UpdateDeploymentCommitSha(ctx, generated.UpdateDeploymentCommitShaParams{
			ID:        dc.deploymentID,
			CommitSha: pgtype.Text{String: dc.commitSHA, Valid: true},
		}); err != nil {
			slog.Error("could not record the deployment commit", "error", err)
		}
	}

	var customBuildpacks []string
	if len(dc.app.CustomBuildpacks) > 0 {
		if err := json.Unmarshal(dc.app.CustomBuildpacks, &customBuildpacks); err != nil {
			slog.Debug("could not parse custom buildpacks, using defaults", "app_id", dc.payload.ApplicationID, "error", err)
		}
	}

	pub := buildlog.NewPublisher(h.RedisClient, dc.payload.DeploymentID)
	sink := buildlog.NewLogSink(pub, buildCtx)

	buildOpts := build.BuildOptions{
		SourceDir:      buildDir,
		ImageTag:       dc.imageName,
		DockerfilePath: dc.app.DockerfilePath.String,
		BuilderImage:   dc.app.BuilderImage.String,
		Buildpacks:     customBuildpacks,
		Env:            dc.env,
		BuildType:      dc.app.BuildType,
		StdoutWriter:   sink.Writer("stdout"),
		StderrWriter:   sink.Writer("stderr"),
		ApplicationID:  dc.payload.ApplicationID,
	}

	h.updateDeploymentStatus(ctx, dc.deploymentID, status.DeploymentPending, status.DeploymentBuilding)

	slog.Info("building image", "tag", dc.imageName)
	stopFlush := h.startBuildLogFlusher(ctx, dc.deploymentID, sink)
	result, err := h.Chain.Build(buildCtx, buildOpts)
	sink.Flush()
	stopFlush()
	pub.Close(ctx)

	// Persist the structured (NDJSON) build log built from the streamed lines,
	// so the stored log matches what live viewers received. Do this on failure
	// too, so a failed deploy surfaces the full build output in the log viewer
	// instead of only the (summarised) error message.
	if buildLogs := sink.NDJSON(); buildLogs != "" {
		if err := h.Queries.UpdateDeploymentBuildLogs(ctx, generated.UpdateDeploymentBuildLogsParams{
			ID:        dc.deploymentID,
			BuildLogs: pgtype.Text{String: buildLogs, Valid: true},
		}); err != nil {
			slog.Error("could not store the build logs", "error", err)
		}
	}

	if err != nil {
		return fmt.Errorf("build: %w", err)
	}

	dc.imageName = result.ImageTag
	slog.Info("build completed", "image", dc.imageName)
	h.updateDeploymentStatus(ctx, dc.deploymentID, status.DeploymentBuilding, status.DeploymentDeploying)
	return nil
}

// startBuildLogFlusher periodically persists the partial build log to
// deployments.build_logs while a build runs, so a page refresh mid-build can
// show progress. Live lines go over Redis pub/sub which has no replay, and
// build_logs is otherwise only written once the build ends — so without this a
// refresh shows nothing until completion. Returns a stop function that halts the
// flusher and waits for it to exit before the caller writes the final log.
func (h *TaskHandler) startBuildLogFlusher(ctx context.Context, deploymentID pgtype.UUID, sink *buildlog.LogSink) func() {
	done := make(chan struct{})
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		t := time.NewTicker(2 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-t.C:
				if s := sink.NDJSON(); s != "" {
					if err := h.Queries.UpdateDeploymentBuildLogs(ctx, generated.UpdateDeploymentBuildLogsParams{
						ID:        deploymentID,
						BuildLogs: pgtype.Text{String: s, Valid: true},
					}); err != nil {
						slog.Error("could not store the build logs", "error", err)
					}
				}
			}
		}
	}()
	return func() {
		close(done)
		<-stopped
	}
}

// writeFileMounts materialises the application's file/config mounts to managed
// host files under <FileMountsDir>/<app-id>/ and returns read-only bind specs.
// The per-app directory is cleared and rewritten every deploy so the on-disk
// set exactly matches the DB — removed mounts and stale (possibly secret)
// content never linger. Clearing is safe here: the previous container has
// already been removed by this deploy stage, so nothing holds a bind to these
// files. Returns nil when the app has no file mounts.
func (h *TaskHandler) writeFileMounts(ctx context.Context, dc *deployContext) ([]runtime.BindMount, error) {
	rows, err := h.Queries.ListApplicationFileMounts(ctx, dc.applicationID)
	if err != nil {
		return nil, fmt.Errorf("load file mounts: %w", err)
	}

	appDir := filepath.Join(h.Config.FileMountsDir, dc.payload.ApplicationID)
	if err := os.RemoveAll(appDir); err != nil {
		return nil, fmt.Errorf("clear file mounts dir: %w", err)
	}
	if len(rows) == 0 {
		return nil, nil
	}
	// 0700 on the per-app dir is the host-side protection for secret content:
	// other (non-root) host users cannot traverse into it, so the files' own
	// modes (0644 by default, needed so a non-root *container* uid can read the
	// bind-mounted inode) never expose secrets on the host. The container reads
	// the bind-mounted file directly and never traverses this dir, so 0700 does
	// not affect it.
	if err := os.MkdirAll(appDir, 0o700); err != nil {
		return nil, fmt.Errorf("create file mounts dir: %w", err)
	}

	binds := make([]runtime.BindMount, 0, len(rows))
	for _, fm := range rows {
		content, err := h.Keyring.Decrypt(fm.ContentEncrypted)
		if err != nil {
			return nil, fmt.Errorf("decrypt file mount %s: %w", fm.MountPath, err)
		}
		mode := parseFileMode(fm.FileMode)
		// Host filename is the mount id in hex — unique, filesystem-safe, and
		// irrelevant to the container (it sees the file at MountPath).
		hostFile := filepath.Join(appDir, fmt.Sprintf("%x", fm.ID.Bytes))
		if err := os.WriteFile(hostFile, content, mode); err != nil {
			return nil, fmt.Errorf("write file mount %s: %w", fm.MountPath, err)
		}
		binds = append(binds, runtime.BindMount{Source: hostFile, Target: fm.MountPath})
	}
	return binds, nil
}

// parseFileMode parses a stored octal mode string (e.g. "0644"), falling back to
// 0644 when empty or malformed. The value was validated on write, so the
// fallback is defence in depth.
func parseFileMode(mode string) os.FileMode {
	if mode == "" {
		return 0o644
	}
	parsed, err := strconv.ParseUint(mode, 8, 32)
	if err != nil {
		return 0o644
	}
	// Mask to the standard rwx bits: never honour setuid/setgid/sticky on a
	// user-supplied mount, even though validateFileMode already rejects them.
	return os.FileMode(parsed) & 0o777
}

// createAndStart creates the container, starts it, and connects it to belune-infra.
// On success it appends a compensator that stops and removes the container.
func (h *TaskHandler) createAndStart(ctx context.Context, dc *deployContext, t *deployTarget) error {
	// Load persistent volumes and ensure the backing Docker volumes exist
	// before creating the container. Volumes are keyed by application ID + name
	// (naming.AppVolumeName) so every redeploy reattaches the same data. The
	// CreateDataVolume label opts them out of PruneVolumes. Previews are
	// distinct application rows with no volume records, so they stay stateless
	// and never mount the parent's data.
	volRows, err := h.Queries.ListApplicationVolumes(ctx, dc.applicationID)
	if err != nil {
		return fmt.Errorf("load volumes: %w", err)
	}
	volumes := make(map[string]string, len(volRows))
	for _, v := range volRows {
		volName := naming.AppVolumeName(dc.payload.ApplicationID, v.Name)
		if err := dc.runtime.CreateDataVolume(ctx, volName); err != nil {
			return fmt.Errorf("ensure volume %s: %w", volName, err)
		}
		volumes[volName] = v.MountPath
	}

	// Materialise file/config mounts to managed host files and bind them
	// read-only. Content lives in the DB (source of truth); the host files are
	// rewritten every deploy.
	fileBinds, err := h.writeFileMounts(ctx, dc)
	if err != nil {
		return fmt.Errorf("write file mounts: %w", err)
	}

	// Runtime profile (v0.0.32, migration 000043). "Hardened" (read-only rootfs +
	// all caps dropped + tmpfs /tmp,/run) is the default for untrusted code — the
	// operator's own git builds and manually-added images. "Standard" (writable
	// rootfs + Docker's default capability set, no extra tmpfs) is the default for
	// curated template apps, whose stock images run a root entrypoint that chowns
	// and drops privileges. no-new-privileges stays on in both.
	var capDrop []string
	var tmpfs map[string]string
	if dc.app.ContainerCaps != "standard" {
		capDrop = []string{"ALL"}
	}
	if dc.app.ReadonlyRootfs {
		tmpfs = map[string]string{"/tmp": "", "/run": ""}
	}

	hc := healthCheckRuntimeConfig(dc.app, t.health)

	entrypoint, cmd := commandOverride(t.command)
	containerID, err := dc.runtime.CreateContainer(ctx, runtime.ContainerConfig{
		Name:       t.containerName,
		Image:      dc.imageName,
		Entrypoint: entrypoint,
		Cmd:        cmd,
		Env:        dc.env,
		Ports:      map[string]string{},
		Network:    naming.ProjectNetworkName(dc.appRow.ProjectSlug),
		// The routable type answers to the application's stable slug whatever its
		// container is called, so siblings addressing it by name (a documented
		// contract) keep working across a roll. For a legacy container named exactly
		// the slug the alias is a no-op. While two generations overlap they share it
		// and DNS returns both: a sibling reaching either is correct, and the overlap
		// lasts only until the old one is drained.
		NetworkAliases: routableAliases(dc, t),
		// application-id groups all of an app's logs; deployment-id further
		// separates them into per-run sessions so the viewer can isolate one
		// redeploy/rebuild/rollback from the next.
		Labels: map[string]string{
			runtime.LabelApplicationID: dc.payload.ApplicationID,
			runtime.LabelDeploymentID:  dc.payload.DeploymentID,
			// Which container of the application this is. Written on create only, so
			// an existing container has neither until it is redeployed -- readers
			// identify those by stored container name instead.
			runtime.LabelProcessType: t.typeName,
			runtime.LabelInstanceIdx: strconv.Itoa(int(t.instanceIdx)),
		},
		CPULimit:        t.cpuLimit,
		MemoryLimit:     t.memoryLimit,
		HealthCheckPath: dc.app.HealthCheckPath.String,
		// Native Docker HEALTHCHECK — set only for the command type; empty
		// command leaves the image's own HEALTHCHECK untouched.
		HealthCheckCommand:     hc.command,
		HealthCheckInterval:    hc.interval,
		HealthCheckTimeout:     hc.timeout,
		HealthCheckRetries:     hc.retries,
		HealthCheckStartPeriod: hc.startPeriod,
		// Persistent volumes mounted at their configured paths. These mounts
		// are writable even though ReadonlyRootfs is true — a volume mount
		// overrides the read-only rootfs at that path.
		Volumes: volumes,
		// Read-only file/config mounts materialised above.
		ReadOnlyBinds: fileBinds,
		// Security hardening (v0.0.9-alpha Phase 2, made per-app in v0.0.32): the
		// capability and rootfs posture come from the app's runtime profile
		// computed above; no-new-privileges is always on.
		CapDrop:        capDrop,
		SecurityOpt:    []string{"no-new-privileges"},
		ReadonlyRootfs: dc.app.ReadonlyRootfs,
		Tmpfs:          tmpfs,
	})
	if err != nil {
		return fmt.Errorf("create container: %w", err)
	}

	// Stamp the generation BEFORE the container starts, so its first events already
	// resolve against a row that knows which deployment it belongs to. Best-effort:
	// the column is nullable and D1 changes nothing for an operator if it stays so.
	// Not while rolling: the row still describes the OLD container, and stamping it
	// would make the stale-event guard drop the old container's events while it is
	// still serving. Cutover writes the deployment together with the new name.
	if t.instanceID.Valid && t.prevName == "" {
		if err := h.Queries.SetInstanceDeployment(ctx, generated.SetInstanceDeploymentParams{
			ID: t.instanceID, DeploymentID: dc.deploymentID,
		}); err != nil {
			slog.Warn("createAndStart: could not record the deployment on the instance", "container", t.containerName, "error", err)
		}
	}

	if err := dc.runtime.StartContainer(ctx, containerID); err != nil {
		// Container created but not started — remove it before returning. Use a
		// fresh bounded context: the parent ctx may already be cancelled or
		// near-deadline, and we don't want cleanup to be skipped because of it.
		cleanCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		if rmErr := dc.runtime.RemoveContainer(cleanCtx, containerID); rmErr != nil {
			slog.Warn("createAndStart: failed to remove container after start failure",
				"container", containerID, "error", rmErr)
		}
		cancel()
		return fmt.Errorf("start container: %w", err)
	}

	// No belune-infra cross-attach: project network isolation (v0.0.9 Phase 2).
	// Caddy was attached to the project network in ensureNetworks above so
	// it can already reach this container by hostname.

	t.containerID = containerID

	// Compensator: stop and remove this container if a later stage fails
	dc.addContainerCompensator(containerID)
	return nil
}

// addContainerCompensator registers the rollback for one created container.
//
// ⛔ The id is passed in and captured BY VALUE, never read from a shared field when
// the compensator runs. A field on the shared deployContext is one the next
// createAndStart overwrites, so a closure reading it would, once a deploy
// creates more than one container, stop and remove the LAST container twice and
// leave every earlier one running and unreferenced. The proxy compensator in
// wireProxy captures hostname and path by value for the same reason.
func (dc *deployContext) addContainerCompensator(containerID string) {
	rt := dc.runtime
	dc.compensators = append(dc.compensators, func() {
		cleanCtx := context.Background()
		if err := rt.StopContainer(cleanCtx, containerID); err != nil {
			slog.Warn("compensator: failed to stop container", "container", containerID, "error", err)
		}
		if err := rt.RemoveContainer(cleanCtx, containerID); err != nil {
			slog.Warn("compensator: failed to remove container", "container", containerID, "error", err)
		}
	})
}

// checkDeployQuota re-verifies project + owning-user quota right before a
// deploy proceeds. Skipped silently when no quota service is wired (tests).
// Quota errors are permanent for this deploy: retrying would not help, the
// admin must adjust limits or the user must shrink their fleet.
func (h *TaskHandler) checkDeployQuota(ctx context.Context, dc *deployContext) error {
	if h.QuotaService == nil {
		return nil
	}
	owner, err := h.Queries.GetApplicationOwnerUserID(ctx, dc.applicationID)
	if err != nil {
		// Owner lookup failure shouldn't block deploys — log and proceed.
		slog.Warn("quota: failed to resolve application owner, skipping check",
			"application_id", dc.payload.ApplicationID, "error", err)
		return nil
	}
	return h.QuotaService.CheckCurrentUsage(ctx, dc.app.ProjectID, owner.UserID)
}

// healthVerifyTimeout is the upper bound for post-deploy probing.
const healthVerifyTimeout = 2 * time.Minute

// Defaults for a command health check when the row leaves a field NULL. They
// mirror Docker's own HEALTHCHECK defaults so a check configured with only a
// command behaves the way its author would expect from a Dockerfile.
const (
	defaultHealthInterval    = 30 * time.Second
	defaultHealthTimeout     = 30 * time.Second
	defaultHealthRetries     = 3
	defaultHealthStartPeriod = 0
)

type healthRuntimeConfig struct {
	command     string
	interval    time.Duration
	timeout     time.Duration
	retries     int
	startPeriod time.Duration
}

// healthCheckRuntimeConfig resolves the stored per-app health fields into the
// Docker HEALTHCHECK values, filling NULLs with the defaults. It returns an
// empty command for any type other than "command", which is what tells the
// runtime to leave the image's own HEALTHCHECK alone.
//
// The type and command come from resolveHealthCheck, not from the application: the
// timing knobs stay application-level, the check itself is per process type.
func healthCheckRuntimeConfig(app generated.Application, health resolvedHealth) healthRuntimeConfig {
	if health.typ != "command" || !health.command.Valid || health.command.String == "" {
		return healthRuntimeConfig{}
	}
	cfg := healthRuntimeConfig{
		command:     health.command.String,
		interval:    defaultHealthInterval,
		timeout:     defaultHealthTimeout,
		retries:     defaultHealthRetries,
		startPeriod: defaultHealthStartPeriod,
	}
	if app.HealthCheckIntervalSeconds.Valid && app.HealthCheckIntervalSeconds.Int32 > 0 {
		cfg.interval = time.Duration(app.HealthCheckIntervalSeconds.Int32) * time.Second
	}
	if app.HealthCheckTimeoutSeconds.Valid && app.HealthCheckTimeoutSeconds.Int32 > 0 {
		cfg.timeout = time.Duration(app.HealthCheckTimeoutSeconds.Int32) * time.Second
	}
	if app.HealthCheckRetries.Valid && app.HealthCheckRetries.Int32 > 0 {
		cfg.retries = int(app.HealthCheckRetries.Int32)
	}
	if app.HealthCheckStartPeriodSeconds.Valid && app.HealthCheckStartPeriodSeconds.Int32 > 0 {
		cfg.startPeriod = time.Duration(app.HealthCheckStartPeriodSeconds.Int32) * time.Second
	}
	return cfg
}

// verifyCommandHealth is the deploy-time gate for a command health check. It
// asks Docker for the container's health until it settles on healthy (pass) or
// unhealthy (fail), giving up after a budget derived from the check's own
// timing — a check that never leaves "starting" is treated as a failure so a
// misconfigured command cannot let a broken deploy through.
func (h *TaskHandler) verifyCommandHealth(ctx context.Context, dc *deployContext, t *deployTarget) error {
	hc := healthCheckRuntimeConfig(dc.app, t.health)

	// Enough time for the start period plus a full run of retries, floored so a
	// fast check still gets a fair chance and capped so a hung deploy cannot
	// wait forever.
	budget := hc.startPeriod + hc.interval*time.Duration(hc.retries+1) + hc.timeout*time.Duration(hc.retries)
	if budget < 30*time.Second {
		budget = 30 * time.Second
	}
	if budget > healthVerifyTimeout {
		budget = healthVerifyTimeout
	}
	slog.Info("verifying container health (command)", "container", t.containerName, "budget", budget)

	deadline := time.Now().Add(budget)
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	for {
		switch st, err := dc.runtime.ContainerHealth(ctx, t.containerName); {
		case err != nil:
			// Transient inspect failure — keep trying until the budget runs out.
			slog.Debug("verifyCommandHealth: inspect failed", "container", t.containerName, "error", err)
		case st == "healthy":
			h.recordHealth(ctx, dc.deploymentID, healthStatusPassing, "")
			slog.Info("container health verified", "container", t.containerName)
			return nil
		case st == "unhealthy":
			msg := "container reported unhealthy"
			h.recordHealth(ctx, dc.deploymentID, healthStatusFailing, msg)
			return errors.New(msg)
		}

		if time.Now().After(deadline) {
			msg := "health check did not pass within the deploy window"
			h.recordHealth(ctx, dc.deploymentID, healthStatusFailing, msg)
			return errors.New(msg)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

// Health status values persisted on the deployments row.
const (
	healthStatusPassing = "passing"
	healthStatusFailing = "failing"
	healthStatusSkipped = "skipped"
)

// verifyHealth probes the container's health endpoint after the proxy is
// wired. The probe targets the container hostname directly (Docker network)
// rather than going through Caddy — Caddy's own active health checks are a
// pass-through observability layer wired separately into the route config.
//
// Result is persisted to deployments.health_status so the API/UI can render
// the latest probe outcome without re-running it. A failed probe is fatal:
// the caller invokes compensators so the bad container/route do not replace
// the working ones.
//
// This is the ROUTABLE type's gate only: it is the one with a port and domains, and
// the only one that records the deployment's health. Workers go through verifyWorker.
func (h *TaskHandler) verifyHealth(ctx context.Context, dc *deployContext) error {
	// Every replica must pass: a route with one broken upstream would serve it a share
	// of the traffic. The first failure fails the deploy (and is what gets recorded).
	for _, t := range dc.routables() {
		if err := h.verifyRoutableHealth(ctx, dc, t); err != nil {
			return err
		}
	}
	return nil
}

// verifyRoutableHealth is verifyHealth for one instance of the routable type.
func (h *TaskHandler) verifyRoutableHealth(ctx context.Context, dc *deployContext, t *deployTarget) error {
	// The command type does not probe over HTTP — Docker runs the check inside
	// the container. The gate here waits for Docker's own verdict so a broken
	// command fails the deploy, exactly as a failing HTTP probe does; after
	// that the check keeps running and the eventwatcher turns later verdicts
	// into status changes.
	if t.health.typ == "command" {
		return h.verifyCommandHealth(ctx, dc, t)
	}

	// 'none', or an inert http check (no path), is not an error — there is simply
	// nothing to verify. The inert test is the resolver's, shared with the API, so
	// what the operator is told and what this gate does cannot drift. ⚠️ With a
	// rolling deploy this gate is the whole safety mechanism: skipping it means the
	// new generation is never verified before the old one is removed.
	if t.health.typ == "none" || t.health.inert {
		h.recordHealth(ctx, dc.deploymentID, healthStatusSkipped, "no health check configured")
		return nil
	}

	// dc.domains is populated by wireProxy; it should always be set here,
	// but fall back to a fresh fetch in case the stage ordering changes.
	domains := dc.domains
	if domains == nil {
		fetched, err := h.Queries.ListDomainsByApplication(ctx, dc.applicationID)
		if err != nil {
			slog.Warn("verifyHealth: failed to list domains for port resolution",
				"application_id", dc.payload.ApplicationID, "error", err)
		}
		domains = fetched
	}

	timeout := healthVerifyTimeout
	if dc.app.HealthCheckTimeoutSeconds.Valid && dc.app.HealthCheckTimeoutSeconds.Int32 > 0 {
		timeout = time.Duration(dc.app.HealthCheckTimeoutSeconds.Int32) * time.Second
	}
	var expectStatus int
	if dc.app.HealthCheckExpectStatus.Valid {
		expectStatus = int(dc.app.HealthCheckExpectStatus.Int32)
	}

	healthURL := fmt.Sprintf("http://%s:%d%s", t.containerName, resolveContainerPort(dc.app, domains), dc.app.HealthCheckPath.String)
	slog.Info("verifying container health", "url", healthURL, "timeout", timeout, "expect_status", expectStatus)

	if err := pollHealthCheck(ctx, healthURL, timeout, expectStatus); err != nil {
		h.recordHealth(ctx, dc.deploymentID, healthStatusFailing, err.Error())
		return err
	}
	h.recordHealth(ctx, dc.deploymentID, healthStatusPassing, "")
	slog.Info("container health verified", "container", t.containerName)
	return nil
}

// verifyWorker is the deploy gate for a non-routable process type. A worker listens
// on nothing, so it must NEVER be probed over HTTP: only a command check, which
// Docker runs inside the container, gates it, and anything else (the resolved 'none',
// or an explicit type that makes no sense without a port) verifies nothing. It
// deliberately does not touch deployments.health_status, which describes the
// routable type; a worker whose command check fails still fails the deploy.
func (h *TaskHandler) verifyWorker(ctx context.Context, dc *deployContext, t *deployTarget) error {
	if t.health.typ != "command" {
		slog.Info("no health gate for worker", "process_type", t.typeName, "container", t.containerName)
		return nil
	}
	return h.verifyCommandHealth(ctx, dc, t)
}

// recordHealth writes the probe result to the deployments row. Failures are
// logged but never propagated — the deploy outcome is already represented by
// the caller's return value.
func (h *TaskHandler) recordHealth(ctx context.Context, deploymentID pgtype.UUID, status, message string) {
	if err := h.Queries.UpdateDeploymentHealth(ctx, generated.UpdateDeploymentHealthParams{
		ID:            deploymentID,
		HealthStatus:  pgtype.Text{String: status, Valid: true},
		HealthMessage: pgtype.Text{String: message, Valid: message != ""},
	}); err != nil {
		slog.Warn("verifyHealth: failed to persist health status", "status", status, "error", err)
	}
}

// wireProxy adds a Caddy route for each domain. On success it appends a compensator
// per route so they can be removed if a later stage fails.
func (h *TaskHandler) wireProxy(ctx context.Context, dc *deployContext) error {
	// Load domains if not already fetched by checkHealth
	if dc.domains == nil {
		domains, err := h.Queries.ListDomainsByApplication(ctx, dc.applicationID)
		if err != nil {
			slog.Warn("failed to list domains for application", "application_id", dc.payload.ApplicationID, "error", err)
		}
		dc.domains = domains
	}

	for _, domain := range dc.domains {
		cfg, err := proxy.BuildRouteConfigFromDB(ctx, h.Queries, h.Keyring, domain, dc.routableNames(), dc.app.HealthCheckPath.String)
		if err != nil {
			return fmt.Errorf("build route config for %s: %w", domain.Hostname, err)
		}
		if err := h.Proxy.AddRoute(ctx, cfg); err != nil {
			return fmt.Errorf("add route for %s: %w", domain.Hostname, err)
		}

		// Compensator: remove this route if a later stage fails. Captures the path
		// as well as the host — the route it added is identified by both, and
		// unwinding by host alone would take a sibling path's route down with it.
		hostname, path := domain.Hostname, domain.Path
		dc.compensators = append(dc.compensators, func() {
			if err := h.Proxy.RemoveRoute(context.Background(), hostname, path); err != nil {
				slog.Warn("compensator: failed to remove proxy route", "hostname", hostname, "path", path, "error", err)
			}
		})
	}
	return nil
}

// finalize atomically marks the deployment as succeeded and the application as running.
func (h *TaskHandler) finalize(ctx context.Context, dc *deployContext) error {
	return store.WithTx(ctx, h.DB, func(q *generated.Queries) error {
		if !status.ValidTransition(status.DeploymentDeploying, status.DeploymentSuccess) {
			slog.Warn("invalid deployment transition skipped", "from", status.DeploymentDeploying, "to", status.DeploymentSuccess)
			return nil
		}
		if _, err := q.UpdateDeploymentStatus(ctx, generated.UpdateDeploymentStatusParams{
			ID:     dc.deploymentID,
			Status: status.DeploymentSuccess,
		}); err != nil {
			return fmt.Errorf("mark the deployment succeeded: %w", err)
		}
		if _, err := q.UpdateApplicationStatus(ctx, generated.UpdateApplicationStatusParams{
			ID:     dc.applicationID,
			Status: status.ApplicationRunning,
		}); err != nil {
			return fmt.Errorf("mark the application running: %w", err)
		}
		// Alongside the application status, in the same transaction, not instead of
		// it: each instance records which container is serving. The application
		// status stays ONE write and is not derived from the instances.
		for _, t := range dc.all() {
			if !t.instanceID.Valid {
				continue
			}
			if _, err := q.UpdateInstanceContainer(ctx, generated.UpdateInstanceContainerParams{
				ID:            t.instanceID,
				ContainerName: t.containerName,
				DeploymentID:  dc.deploymentID,
				ContainerID:   pgtype.Text{String: t.containerID, Valid: t.containerID != ""},
				Status:        status.ApplicationRunning,
			}); err != nil {
				return fmt.Errorf("record the container of process type %q: %w", t.typeName, err)
			}
		}
		// Stamp last_activity_at for preview GC. Doing this on every deploy
		// (not just previews) keeps the column meaningful if we later extend
		// idle cleanup to parents too.
		q.TouchApplicationActivity(ctx, dc.applicationID)
		q.TouchApplicationDeployed(ctx, dc.applicationID)
		h.clearChangeMarkers(ctx, q, dc)
		return nil
	})
}

// clearChangeMarkers resolves the "your saved config is not what is running"
// indicator now that a deploy has succeeded.
//
// The branch is the same one prepareImage takes: a non-empty RollbackImageTag
// means no build and no pull happened, so the container was recreated from an
// image that already existed. That applies every config change (env, volumes,
// mounts, limits, runtime profile) but nothing about the source — change
// source_image and then roll back, and you still need a real deploy to get the
// new image, so source_changed_at has to survive.
//
// Both queries only clear a marker older than this deployment's started_at, so
// an edit saved while the deploy was in flight keeps its marker instead of
// being silently swallowed. Errors are logged, not returned: the deploy
// genuinely succeeded, and failing it over a stale indicator would be worse
// than the stale indicator.
func (h *TaskHandler) clearChangeMarkers(ctx context.Context, q *generated.Queries, dc *deployContext) {
	skippedBuild := dc.payload.RollbackImageTag != ""

	if err := q.ClearApplicationConfigChanged(ctx, generated.ClearApplicationConfigChangedParams{
		ID: dc.applicationID, ID_2: dc.deploymentID,
	}); err != nil {
		slog.Warn("could not clear config change marker", "error", err, "application_id", dc.payload.ApplicationID)
	}
	if skippedBuild {
		return
	}
	if err := q.ClearApplicationSourceChanged(ctx, generated.ClearApplicationSourceChangedParams{
		ID: dc.applicationID, ID_2: dc.deploymentID,
	}); err != nil {
		slog.Warn("could not clear source change marker", "error", err, "application_id", dc.payload.ApplicationID)
	}
}

// runCompensators runs all compensating cleanup functions in reverse order.
func (h *TaskHandler) runCompensators(dc *deployContext) {
	for i := len(dc.compensators) - 1; i >= 0; i-- {
		dc.compensators[i]()
	}
}

// deployFailureErrorsApp reports whether a terminal deploy failure should flag
// the application as errored.
//
// It must not when the application is still serving: a build-only task never
// touches the running container, a failure before the routable type is replaced
// leaves the previous container running the old version, and a ROLLING deploy
// only ever fails with the old generation intact -- it never tears anything down
// before the new one is healthy, so it never sets cleanedUp. cleanedUp is set only
// by the stop-then-create sequence (apps with persistent volumes), where once
// cleanup has run nothing is serving and a failure is a genuine error. A failure
// on an app that was not running to begin with (a first-ever deploy, or one on a
// stopped app) is also an error, which is how it stays visible on the row rather
// than only on the deployment.
func deployFailureErrorsApp(dc *deployContext) bool {
	if dc.buildOnly {
		return false
	}
	if dc.cleanedUp {
		return true
	}
	return dc.app.Status != status.ApplicationRunning &&
		dc.app.Status != status.ApplicationUnhealthy
}

func (h *TaskHandler) failDeployment(ctx context.Context, dc *deployContext, kind, errMsg string) {
	deploymentID := dc.deploymentID
	retried, _ := asynq.GetRetryCount(ctx)
	maxRetry, _ := asynq.GetMaxRetry(ctx)

	safeMsg := redact.Error(errMsg)

	slog.Error("deployment failed",
		"deployment_id", fmt.Sprintf("%v", deploymentID),
		"kind", kind,
		"error", safeMsg,
		"retry", retried,
		"max_retry", maxRetry,
	)

	if retried >= maxRetry {
		if _, err := h.Queries.UpdateDeploymentStatus(ctx, generated.UpdateDeploymentStatusParams{
			ID:           deploymentID,
			Status:       status.DeploymentFailed,
			ErrorMessage: pgtype.Text{String: safeMsg, Valid: true},
		}); err != nil {
			slog.Error("could not record the deployment status", "error", err)
		}

		if deployFailureErrorsApp(dc) {
			if _, err := h.Queries.UpdateApplicationStatus(ctx, generated.UpdateApplicationStatusParams{
				ID:     dc.applicationID,
				Status: status.ApplicationError,
			}); err != nil {
				slog.Warn("could not mark application errored", "error", err)
			} else if dc.routable != nil && dc.routable.instanceID.Valid {
				if _, err := h.Queries.UpdateInstanceStatus(ctx, generated.UpdateInstanceStatusParams{
					ID: dc.routable.instanceID, Status: status.ApplicationError,
				}); err != nil {
					slog.Warn("could not mark instance errored", "error", err)
				}
			}
		}

		h.maybeAlertDeploymentFailed(ctx, deploymentID, kind, safeMsg)
		h.notifyDeployment(ctx, deploymentID, status.DeploymentFailed, kind, safeMsg)
	}
}

// notifyDeployment emits a user-facing notification to the project owner for a
// terminal deployment outcome. Recipient resolution mirrors the email-alert
// path (project owner); the link deep-links into the app's Deployments tab.
// Best-effort and nil-safe — never blocks or fails the deploy path.
//
// Gated on the owner's alert preferences: deploy success honours
// deploy_success, deploy failure honours deploy_failures, and build failure
// honours build_failures. Defaults to enabled when no preferences row exists.
func (h *TaskHandler) notifyDeployment(ctx context.Context, deploymentID pgtype.UUID, outcome, kind, errMsg string) {
	if h.Notifier == nil && h.NotifyChannels == nil {
		return
	}

	info, err := h.Queries.GetDeploymentNotifyInfo(ctx, deploymentID)
	if err != nil {
		slog.Warn("notify: failed to fetch deployment info", "deployment_id", fmt.Sprintf("%v", deploymentID), "error", err)
		return
	}

	var notifType, title, body string
	if outcome == status.DeploymentFailed {
		notifType = "deployment.failed"
		title = fmt.Sprintf("Deployment failed — %s", info.AppName)
		body = errMsg
	} else {
		notifType = "deployment.succeeded"
		title = fmt.Sprintf("Deployment succeeded — %s", info.AppName)
		body = fmt.Sprintf("%s is now running.", info.AppName)
	}

	link := fmt.Sprintf("/projects/%s/applications/%s/deployments",
		formatUUID(info.ProjectID), formatUUID(info.ApplicationID))

	// Provider channels are global admin subscriptions: fire once per event,
	// independent of the owner's personal in-app alert preferences.
	h.dispatchToChannels(ctx, notify.Event{
		Type: notifType, Title: title, Body: body, Link: link, OccurredAt: time.Now(),
	})

	if h.Notifier == nil {
		return
	}

	prefs, prefErr := h.Queries.GetAlertPreferences(ctx, info.UserID)
	if prefErr != nil && !isNoRows(prefErr) {
		slog.Warn("notify: failed to fetch alert preferences", "user_id", formatUUID(info.UserID), "error", prefErr)
		return
	}
	if prefErr == nil {
		var enabled bool
		switch {
		case outcome == status.DeploymentFailed && kind == "build":
			enabled = prefs.BuildFailures
		case outcome == status.DeploymentFailed:
			enabled = prefs.DeployFailures
		default:
			enabled = prefs.DeploySuccess
		}
		if !enabled {
			return
		}
	}

	h.Notifier.Notify(formatUUID(info.UserID), notifType, title, body, link)
}

// maybeAlertDeploymentFailed fires a failure alert email if the deployment owner has alerts enabled.
func (h *TaskHandler) maybeAlertDeploymentFailed(ctx context.Context, deploymentID pgtype.UUID, kind, errMsg string) {
	if h.EmailService == nil || h.Enqueuer == nil {
		return
	}

	ownerInfo, err := h.Queries.GetDeploymentOwnerInfo(ctx, deploymentID)
	if err != nil {
		slog.Warn("alert: failed to fetch deployment owner info", "deployment_id", fmt.Sprintf("%v", deploymentID), "error", err)
		return
	}

	prefs, err := h.Queries.GetAlertPreferences(ctx, ownerInfo.UserID)
	if err != nil && !isNoRows(err) {
		slog.Warn("alert: failed to fetch alert preferences", "user_id", fmt.Sprintf("%v", ownerInfo.UserID), "error", err)
		return
	}

	// Apply defaults when no row: deploy_failures and build_failures both default to true.
	alertEnabled := true
	if err == nil {
		switch kind {
		case "build":
			alertEnabled = prefs.BuildFailures
		default:
			alertEnabled = prefs.DeployFailures
		}
	}
	if !alertEnabled {
		return
	}

	templateName := "alert_deploy_failed"
	if kind == "build" {
		templateName = "alert_build_failed"
	}

	vars := map[string]any{
		"AppName":      ownerInfo.AppName,
		"ProjectName":  ownerInfo.ProjectName,
		"DeploymentID": fmt.Sprintf("%v", deploymentID),
		"ErrorMessage": errMsg,
	}
	if kind == "build" {
		vars["BuildID"] = fmt.Sprintf("%v", deploymentID)
	}

	task, err := NewEmailSendTask(templateName, ownerInfo.Email, vars)
	if err != nil {
		slog.Warn("alert: failed to build email task", "error", err)
		return
	}
	if _, err := h.Enqueuer.Enqueue(task); err != nil {
		slog.Warn("alert: failed to enqueue failure alert email", "error", err)
	}
}

// updateDeploymentStatus validates the transition, updates the deployment status,
// and stamps the relevant timing column for the new state.
func (h *TaskHandler) updateDeploymentStatus(ctx context.Context, id pgtype.UUID, from, to string) {
	if !status.ValidTransition(from, to) {
		slog.Warn("invalid deployment transition skipped", "from", from, "to", to,
			"deployment_id", fmt.Sprintf("%v", id))
		return
	}
	if _, err := h.Queries.UpdateDeploymentStatus(ctx, generated.UpdateDeploymentStatusParams{
		ID:     id,
		Status: to,
	}); err != nil {
		slog.Error("could not record the deployment status", "error", err, "deployment_id", id, "status", to)
	}
	switch to {
	case status.DeploymentBuilding:
		if err := h.Queries.SetDeploymentBuildStarted(ctx, id); err != nil {
			slog.Error("could not stamp the build start", "error", err, "deployment_id", id)
		}
	case status.DeploymentDeploying:
		if from == status.DeploymentBuilding {
			if err := h.Queries.SetDeploymentBuildEnded(ctx, id); err != nil {
				slog.Error("could not stamp the build end", "error", err, "deployment_id", id)
			}
		}
		if err := h.Queries.SetDeploymentDeployStarted(ctx, id); err != nil {
			slog.Error("could not stamp the deploy start", "error", err, "deployment_id", id)
		}
	}
}

// parseUUID parses a UUID string into a pgtype.UUID, returning an error on invalid input.
func parseUUID(s string) (pgtype.UUID, error) {
	var u pgtype.UUID
	if err := u.Scan(s); err != nil {
		return pgtype.UUID{}, fmt.Errorf("invalid UUID %q: %w", s, err)
	}
	return u, nil
}

// resolveContainerPort returns the port the app's container listens on. The
// application's own container_port wins when set (image apps, notably templates
// deployed without a domain); otherwise it falls back to the first domain that
// declares one, then to 8080.
func resolveContainerPort(app generated.Application, domains []generated.Domain) int32 {
	if app.ContainerPort.Valid && app.ContainerPort.Int32 > 0 {
		return app.ContainerPort.Int32
	}
	for _, d := range domains {
		if d.ContainerPort.Valid && d.ContainerPort.Int32 > 0 {
			return d.ContainerPort.Int32
		}
	}
	return 8080
}

// pollHealthCheck repeatedly GETs url until a 2xx response is received or deadline expires.
// pollHealthCheck GETs url until it passes or timeout elapses. When expectStatus
// is non-zero the probe passes only on that exact status; otherwise any 2xx passes.
func pollHealthCheck(ctx context.Context, url string, timeout time.Duration, expectStatus int) error {
	pass := func(code int) bool {
		if expectStatus != 0 {
			return code == expectStatus
		}
		return code >= 200 && code < 300
	}
	deadline := time.Now().Add(timeout)
	client := &http.Client{Timeout: 5 * time.Second}
	// By default the client follows redirects and we accept the final 2xx (so an
	// app that 302s its root to a setup page still passes). But when the author
	// pins an exact 3xx status, following it would mean the redirect code is never
	// observed — so stop at the first response in that case.
	if expectStatus >= 300 && expectStatus < 400 {
		client.CheckRedirect = func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		}
	}
	for time.Now().Before(deadline) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return fmt.Errorf("build health check request: %w", err)
		}
		resp, err := client.Do(req)
		if err == nil {
			resp.Body.Close()
			if pass(resp.StatusCode) {
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
	want := "2xx"
	if expectStatus != 0 {
		want = strconv.Itoa(expectStatus)
	}
	return fmt.Errorf("health check at %s did not return %s within %s", url, want, timeout)
}
