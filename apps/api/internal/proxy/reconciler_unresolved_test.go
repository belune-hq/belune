package proxy_test

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/belune-hq/belune/internal/proxy"
	"github.com/belune-hq/belune/internal/proxy/capability"
	"github.com/belune-hq/belune/internal/store/generated"
	"github.com/belune-hq/belune/internal/testutil"
)

// ⛔ "Absent from expected" means "delete it": the reconciler removes any live
// route it did not build. That is right for an application that is not running
// and catastrophic for one that IS running but whose container could not be
// resolved -- a transient error would remove every domain of the app, and again
// every pass. The same absence must therefore mean opposite things.
//
// Not gated on BELUNE_CADDY_INTEGRATION: that variable is set nowhere in CI, and
// this needs only Postgres, which every other package's tests already require.
func TestReconcile_UnresolvableRunningAppKeepsItsRoutes_StoppedAppLosesThem(t *testing.T) {
	ctx := context.Background()
	pool, queries, teardown := testutil.SetupTestDB()
	defer teardown()
	t.Cleanup(func() { _ = testutil.TruncateAll(ctx, pool) })

	// Running, with domains, and NO instance row: the resolver fails for it.
	held := seedRunningApp(t, ctx, queries)
	_, err := pool.Exec(ctx, `DELETE FROM application_instances WHERE application_id = $1`, held.ID)
	require.NoError(t, err)
	_, err = queries.CreateDomain(ctx, createDomainParams(held.ID, "held.belune.local", proxy.SSLModeOff))
	require.NoError(t, err)

	// Not running, with a domain: intentionally excluded, so the sweep removes it.
	stopped, err := queries.CreateApplication(ctx, generated.CreateApplicationParams{
		ProjectID: held.ProjectID, Name: "Stopped", Slug: "reconcile-project-stopped",
		Type: "image", BuildType: "image", SourceImage: pgtype.Text{String: "nginx:alpine", Valid: true},
	})
	require.NoError(t, err)
	seedInstance(t, ctx, queries, stopped)
	_, err = queries.CreateDomain(ctx, createDomainParams(stopped.ID, "stopped.belune.local", proxy.SSLModeAutomatic))
	require.NoError(t, err)

	// A healthy running app alongside, so an unresolvable neighbour is shown not to
	// stop the rest of the pass.
	healthy, err := queries.CreateApplication(ctx, generated.CreateApplicationParams{
		ProjectID: held.ProjectID, Name: "Healthy", Slug: "reconcile-project-healthy",
		Type: "image", BuildType: "image", SourceImage: pgtype.Text{String: "nginx:alpine", Valid: true},
	})
	require.NoError(t, err)
	seedInstance(t, ctx, queries, healthy)
	_, err = queries.UpdateApplicationStatus(ctx, generated.UpdateApplicationStatusParams{ID: healthy.ID, Status: "running"})
	require.NoError(t, err)
	_, err = queries.CreateDomain(ctx, createDomainParams(healthy.ID, "healthy.belune.local", proxy.SSLModeAutomatic))
	require.NoError(t, err)

	mock := &mockProxy{routes: []proxy.RouteConfig{
		{Hostname: "held.belune.local", Path: "/", TargetURL: "reconcile-project-app:8080"},
		{Hostname: "stopped.belune.local", Path: "/", TargetURL: "reconcile-project-stopped:8080"},
	}}
	r := proxy.NewReconciler(queries, mock, nil, time.Second).
		WithCapabilities(&countingProvider{set: capability.NewSet()})
	require.NoError(t, r.ReconcileNow(ctx))

	assert.NotContains(t, mock.removed, "held.belune.local",
		"a running application whose desired state is unknown must keep its live route")
	assert.Contains(t, mock.removed, "stopped.belune.local",
		"an application that is not running is intentionally excluded and still swept")

	var ensured []string
	for _, c := range mock.ensured {
		ensured = append(ensured, c.Hostname)
	}
	assert.Contains(t, ensured, "healthy.belune.local", "an unresolvable neighbour must not stop the pass")
	assert.NotContains(t, ensured, "held.belune.local", "nothing is pushed for a route whose upstream is unknown")

	// Caddy is still serving the held host, so its TLS decision must still be in
	// force: a hostname is in Caddy's route table iff it is in the skip computation.
	assert.Contains(t, mock.skip, "held.belune.local")
}
