package worker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"go.opentelemetry.io/otel/trace"

	"github.com/belune-hq/belune/internal/proxy"
	"github.com/belune-hq/belune/internal/status"
	"github.com/belune-hq/belune/internal/store/generated"
)

const (
	// defaultRollDrain is how long the old generation keeps running after the route
	// stops sending it new requests, so in-flight ones finish. Caddy applies a route
	// change gracefully; this covers long-lived requests, not the proxy's own lag.
	defaultRollDrain = 5 * time.Second
	// rollRemoveTimeout bounds stopping one drained container. StopContainer's own
	// grace is 30s (SIGTERM, wait, SIGKILL), so this must stay above it.
	rollRemoveTimeout = 60 * time.Second
	// dockerNameMax is Docker's container-name limit, as in service.dockerNameLimit.
	dockerNameMax = 255
)

// shouldRoll reports whether this deploy may run the new generation beside the old.
//
// ⛔ Not when the application has persistent volumes. Two containers writing one
// volume is a data hazard (two databases on one data directory), and the stop-then-
// create sequence is what has always made that safe. A volume lookup that fails is
// treated as "has volumes": the conservative answer costs only availability.
func (h *TaskHandler) shouldRoll(ctx context.Context, dc *deployContext) bool {
	vols, err := h.Queries.ListApplicationVolumes(ctx, dc.applicationID)
	if err != nil {
		slog.Warn("deploy: could not list volumes; recreating in place rather than rolling",
			"application_id", dc.payload.ApplicationID, "error", err)
		return false
	}
	return len(vols) == 0
}

// routableAliases is the stable DNS name a routable container answers to.
func routableAliases(dc *deployContext, t *deployTarget) []string {
	if !t.routable || dc.appRow.Slug == "" {
		return nil
	}
	return []string{dc.appRow.Slug}
}

// rollName mints the new generation's container name. Unlike a row minted by the API
// (which has no deployment yet and uses a random token), a deploy HAS a unique id, and
// it makes a retried attempt of the same deployment land on the same name, which the
// roll then clears before creating.
func rollName(dc *deployContext, t *deployTarget) (string, error) {
	n := fmt.Sprintf("%s-%s-%d-%s", dc.appRow.Slug, t.typeName, t.instanceIdx, dc.payload.DeploymentID[:8])
	if len(n) > dockerNameMax {
		return "", fmt.Errorf("the container name %q would be %d characters, over Docker's %d limit", n, len(n), dockerNameMax)
	}
	return n, nil
}

// previousNames are the old generation's containers that are actually there, in idx
// order: the upstreams to keep while the new ones prove themselves.
func (dc *deployContext) previousNames() []string {
	var out []string
	for _, t := range dc.routables() {
		if t.prevExists {
			out = append(out, t.prevName)
		}
	}
	return out
}

// planRoll points every routable target at a NEW container name, remembering the old
// one. The instance rows are not touched: a row always describes the CURRENT
// container, and the roll repoints it at cutover (finalize). If the control plane
// dies in between, the new containers have no row and the old ones still do; whichever
// generation lacks one is an orphan the 24h reaper claims by name.
func (h *TaskHandler) planRoll(ctx context.Context, dc *deployContext) error {
	h.stampLegacyGeneration(ctx, dc)
	for _, t := range dc.routables() {
		name, err := rollName(dc, t)
		if err != nil {
			return err
		}
		t.prevName = t.containerName
		exists, err := dc.runtime.ContainerExists(ctx, t.prevName)
		// An unanswerable existence check is treated as "exists": the cost of being
		// wrong that way is a missing upstream for a moment, never an undrained one.
		t.prevExists = err != nil || exists
		t.containerName = name
	}
	return nil
}

// stampLegacyGeneration gives a row that has no recorded deployment (every row on an
// install that predates D1) the deployment that built the container now serving.
//
// Without it the new container's events, which arrive while the row still describes
// the old one, would resolve to that row by labels (NULL = "no generation to
// contradict") and a new container that fails its health gate would flag a healthy,
// serving application. With it the stale-event guard rejects them. Best-effort and
// only ever fills a NULL: the latest successful deployment is what created the
// container (deploys and scale-ups both run from it).
func (h *TaskHandler) stampLegacyGeneration(ctx context.Context, dc *deployContext) {
	var serving pgtype.UUID
	for _, t := range dc.routables() {
		if !t.instanceID.Valid {
			continue
		}
		inst, err := h.Queries.GetInstance(ctx, t.instanceID)
		if err != nil || inst.DeploymentID.Valid {
			continue
		}
		if !serving.Valid {
			dep, err := h.Queries.GetLatestSuccessfulDeployment(ctx, dc.applicationID)
			if err != nil {
				return // never deployed: there is no old container whose events to protect
			}
			serving = dep.ID
		}
		if err := h.Queries.SetInstanceDeployment(ctx, generated.SetInstanceDeploymentParams{ID: t.instanceID, DeploymentID: serving}); err != nil {
			slog.Warn("roll: could not record the serving generation on the instance", "error", err)
		}
	}
}

// rollRoutable replaces the routable type's containers without a gap:
//
//	create new beside old -> health-gate new (directly) -> route old+new
//	  -> commit rows (finalize) -> route new only -> drain -> remove old
//
// Failure anywhere BEFORE the commit removes the new containers, restores the route to
// the old upstreams, and leaves the application serving its old generation. After the
// commit nothing aborts: the rows already describe the new generation, so the
// reconciler converges the route to it on its own.
//
// Time: each replica is health-gated for up to the app's probe timeout (2 minutes by
// default), one after another, so N replicas can take N times that. A roll trades
// time for availability; the gate is deliberately not parallelised, so a broken image
// fails on the first replica rather than after starting them all.
func (h *TaskHandler) rollRoutable(ctx context.Context, dc *deployContext, rootSpan trace.Span) error {
	fail := func(kind, msg string, err error) error {
		h.runCompensators(dc)
		h.failDeployment(ctx, dc, kind, msg)
		recordSpanErr(rootSpan, err)
		return err
	}

	if err := h.planRoll(ctx, dc); err != nil {
		return fail("deploy", fmt.Sprintf("create container: %v", err), fmt.Errorf("create container: %w", err))
	}

	if err := runStage(ctx, "deploy.create_and_start", func(ctx context.Context) error {
		for _, t := range dc.routables() {
			// A leftover from an earlier attempt of THIS deployment shares the name.
			if exists, err := dc.runtime.ContainerExists(ctx, t.containerName); err != nil || exists {
				removeContainer(ctx, dc, t.containerName)
			}
			if err := h.createAndStart(ctx, dc, t); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		return fail("deploy", fmt.Sprintf("create container: %v", err), fmt.Errorf("create container: %w", err))
	}

	// Gate the new generation BEFORE any traffic can reach it.
	if err := runStage(ctx, "deploy.verify_health", func(ctx context.Context) error {
		return h.verifyHealth(ctx, dc)
	}); err != nil {
		return fail("deploy", fmt.Sprintf("health check failed: %v", err), fmt.Errorf("health check: %w", err))
	}

	// Old and new together. With no old container (a first deploy) this is already
	// the final route.
	both := append(dc.previousNames(), dc.routableNames()...)
	if err := runStage(ctx, "deploy.wire_proxy", func(ctx context.Context) error {
		return h.setRoutes(ctx, dc, both, true)
	}); err != nil {
		return fail("deploy", fmt.Sprintf("wire proxy: %v", err), fmt.Errorf("wire proxy: %w", err))
	}

	// The commit. A failure here still has the old generation intact.
	if err := runStage(ctx, "deploy.finalize", func(ctx context.Context) error {
		return h.finalize(ctx, dc)
	}); err != nil {
		slog.Error("failed to commit deploy success status", "application_id", dc.payload.ApplicationID, "error", err)
		return fail("deploy", fmt.Sprintf("commit: %v", err), fmt.Errorf("finalize: %w", err))
	}
	h.notifyDeployment(ctx, dc.deploymentID, status.DeploymentSuccess, "deploy", "")

	h.cutOver(ctx, dc)
	return nil
}

// cutOver runs after the commit and cannot fail the deploy: the new generation is
// already recorded and healthy.
func (h *TaskHandler) cutOver(ctx context.Context, dc *deployContext) {
	_ = runStage(ctx, "deploy.cutover", func(ctx context.Context) error {
		if err := h.setRoutes(ctx, dc, dc.routableNames(), false); err != nil {
			// Not fatal: the rows say the new generation, and the reconciler writes the
			// same route on its next pass.
			slog.Warn("cutover: could not move the route to the new generation; the reconciler will", "error", err)
		}
		drain := h.RollDrainDelay
		if drain == 0 {
			drain = defaultRollDrain
		}
		select {
		case <-time.After(drain):
		case <-ctx.Done():
		}
		// Detached: a cancelled task context would turn the removal into a silent
		// no-op and strand the old generation.
		bg := context.WithoutCancel(ctx)
		for _, t := range dc.routables() {
			if !t.prevExists {
				continue
			}
			rmCtx, cancel := context.WithTimeout(bg, rollRemoveTimeout)
			removeContainer(rmCtx, dc, t.prevName)
			cancel()
		}
		h.cleanupLegacyNames(bg, dc)
		return nil
	})
}

// setRoutes writes one route per domain whose upstreams are names. With compensate it
// also registers the undo: RESTORE the previous upstreams if the old generation was
// serving, and only remove the route when nothing was.
//
// ⛔ The in-place sequence's compensator removes the route, which is right when nothing
// is serving. Here the old generation still is, so removing it would take down an
// application that a failed roll is supposed to leave untouched.
func (h *TaskHandler) setRoutes(ctx context.Context, dc *deployContext, names []string, compensate bool) error {
	if dc.domains == nil {
		domains, err := h.Queries.ListDomainsByApplication(ctx, dc.applicationID)
		if err != nil {
			slog.Warn("failed to list domains for application", "application_id", dc.payload.ApplicationID, "error", err)
		}
		dc.domains = domains
	}
	previous := dc.previousNames()
	for _, domain := range dc.domains {
		cfg, err := proxy.BuildRouteConfigFromDB(ctx, h.Queries, h.Keyring, domain, names, dc.app.HealthCheckPath.String)
		if err != nil {
			return fmt.Errorf("build route config for %s: %w", domain.Hostname, err)
		}
		if err := h.Proxy.AddRoute(ctx, cfg); err != nil {
			return fmt.Errorf("add route for %s: %w", domain.Hostname, err)
		}
		if !compensate {
			continue
		}
		d := domain // captured by value, as the in-place compensator does for host and path
		dc.compensators = append(dc.compensators, func() { h.restoreRoute(dc, d, previous) })
	}
	return nil
}

func (h *TaskHandler) restoreRoute(dc *deployContext, d generated.Domain, previous []string) {
	bg := context.Background()
	if len(previous) == 0 {
		if err := h.Proxy.RemoveRoute(bg, d.Hostname, d.Path); err != nil {
			slog.Warn("compensator: failed to remove proxy route", "hostname", d.Hostname, "path", d.Path, "error", err)
		}
		return
	}
	cfg, err := proxy.BuildRouteConfigFromDB(bg, h.Queries, h.Keyring, d, previous, dc.app.HealthCheckPath.String)
	if err == nil {
		err = h.Proxy.AddRoute(bg, cfg)
	}
	if err != nil {
		// The reconciler's next pass writes the old upstreams from the (unchanged) rows.
		slog.Warn("compensator: failed to restore the previous route", "hostname", d.Hostname, "error", errors.Join(err))
	}
}
