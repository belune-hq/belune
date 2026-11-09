package proxy_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/belune-hq/belune/internal/proxy"
	"github.com/belune-hq/belune/internal/status"
	"github.com/belune-hq/belune/internal/store/generated"
	"github.com/belune-hq/belune/internal/testutil"
)

// A pass that cannot read what it needs must never delete a route on a guess.
// These tests fail one query at a time -- the way a transient database error
// would -- and read the verdict off the proxy mock: what was removed, what was
// ensured, and which hosts the TLS skip list still counts.

var errInjected = errors.New("injected: connection reset")

// failingDB wraps a real pool and fails any statement for which fail returns true.
// Everything else reaches Postgres, so the pass runs on real data.
type failingDB struct {
	generated.DBTX
	fail func(sql string, args []any) bool
}

type failedRow struct{}

func (failedRow) Scan(...any) error { return errInjected }

func (f failingDB) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	if f.fail(sql, args) {
		return nil, errInjected
	}
	return f.DBTX.Query(ctx, sql, args...)
}

func (f failingDB) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	if f.fail(sql, args) {
		return failedRow{}
	}
	return f.DBTX.QueryRow(ctx, sql, args...)
}

func (f failingDB) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	return f.DBTX.Exec(ctx, sql, args...)
}

// failOn matches a generated query by name and its first argument.
func failOn(name string, first any) func(string, []any) bool {
	return func(sql string, args []any) bool {
		return strings.Contains(sql, "-- name: "+name+" ") && len(args) > 0 && args[0] == first
	}
}

func addRunningApp(t *testing.T, ctx context.Context, q *generated.Queries, base generated.Application, slug string) generated.Application {
	t.Helper()
	app, err := q.CreateApplication(ctx, generated.CreateApplicationParams{
		ProjectID: base.ProjectID, Name: slug, Slug: slug,
		Type: "image", BuildType: "image", SourceImage: pgtype.Text{String: "nginx:alpine", Valid: true},
	})
	require.NoError(t, err)
	seedInstance(t, ctx, q, app)
	app, err = q.UpdateApplicationStatus(ctx, generated.UpdateApplicationStatusParams{ID: app.ID, Status: status.ApplicationRunning})
	require.NoError(t, err)
	return app
}

func addDomain(t *testing.T, ctx context.Context, q *generated.Queries, app generated.Application, host string) generated.Domain {
	t.Helper()
	d, err := q.CreateDomain(ctx, createDomainParams(app.ID, host, proxy.SSLModeOff))
	require.NoError(t, err)
	return d
}

func liveRoutes(hosts ...string) []proxy.RouteConfig {
	var out []proxy.RouteConfig
	for _, h := range hosts {
		out = append(out, proxy.RouteConfig{Hostname: h, Path: "/", Upstreams: []string{"x:8080"}})
	}
	return out
}

func ensuredHosts(m *mockProxy) []string {
	var out []string
	for _, c := range m.ensured {
		out = append(out, c.Hostname)
	}
	return out
}

func reconcileWith(t *testing.T, ctx context.Context, db generated.DBTX, mock *mockProxy) {
	t.Helper()
	// CrowdSec is switched on and ready, with its module present, so the bouncer
	// DOES push: without that, syncCrowdSec returns early and an assertion about it
	// would pass on any code.
	r := proxy.NewReconciler(generated.New(db), mock, nil, time.Second).
		WithCapabilities(&countingProvider{set: caddyWithCrowdSec}).
		WithCrowdSec(&fakeCrowdSec{desire: proxy.CrowdSecDesire{Enabled: true, Ready: true, APIURL: "http://crowdsec:8080", APIKey: "k"}})
	require.NoError(t, r.ReconcileNow(ctx))
}

// The slug query fails for a running application with domains: every one of its
// routes is held -- not deleted, and still counted in the TLS skip list.
func TestReconcile_SlugQueryFails_HoldsTheAppsRoutes(t *testing.T) {
	ctx := context.Background()
	pool, queries, teardown := testutil.SetupTestDB()
	defer teardown()
	t.Cleanup(func() { _ = testutil.TruncateAll(ctx, pool) })

	flaky := seedRunningApp(t, ctx, queries)
	addDomain(t, ctx, queries, flaky, "a.belune.local")
	addDomain(t, ctx, queries, flaky, "b.belune.local")
	healthy := addRunningApp(t, ctx, queries, flaky, "reconcile-project-healthy")
	addDomain(t, ctx, queries, healthy, "healthy.belune.local")

	mock := &mockProxy{routes: liveRoutes("a.belune.local", "b.belune.local", "healthy.belune.local")}
	reconcileWith(t, ctx, failingDB{pool, failOn("GetApplicationWithProjectSlug", flaky.ID)}, mock)

	assert.Empty(t, mock.removed, "a transient error must not delete a running application's routes")
	assert.Contains(t, mock.skip, "a.belune.local", "Caddy still serves it, so the skip list must count it")
	assert.Contains(t, mock.skip, "b.belune.local")
	assert.ElementsMatch(t, []string{"healthy.belune.local"}, ensuredHosts(mock),
		"nothing is pushed for a held route; the neighbour is unaffected")
}

// The domains query is the one failure that leaves the pass blind: with no rows
// there are no route keys, so a live route cannot be told from a stale one. No
// route may be removed anywhere in that pass -- an unrelated app's stale route
// included -- while create and repair carry on.
func TestReconcile_DomainsQueryFails_DisablesTheSweepForThePass(t *testing.T) {
	ctx := context.Background()
	pool, queries, teardown := testutil.SetupTestDB()
	defer teardown()
	t.Cleanup(func() { _ = testutil.TruncateAll(ctx, pool) })

	flaky := seedRunningApp(t, ctx, queries)
	addDomain(t, ctx, queries, flaky, "flaky.belune.local")
	healthy := addRunningApp(t, ctx, queries, flaky, "reconcile-project-healthy")
	addDomain(t, ctx, queries, healthy, "healthy.belune.local")

	mock := &mockProxy{routes: liveRoutes("flaky.belune.local", "stale.belune.local")}
	reconcileWith(t, ctx, failingDB{pool, failOn("ListDomainsByApplication", flaky.ID)}, mock)

	assert.Empty(t, mock.removed, "a pass that could not enumerate is not allowed to delete")
	assert.Contains(t, ensuredHosts(mock), "healthy.belune.local", "create and repair continue")
}

// One of two domains fails to build: only that one is held; the other is still
// built and served. Per-domain, not per-app.
func TestReconcile_BuildFailsForOneDomain_HoldsOnlyThatDomain(t *testing.T) {
	ctx := context.Background()
	pool, queries, teardown := testutil.SetupTestDB()
	defer teardown()
	t.Cleanup(func() { _ = testutil.TruncateAll(ctx, pool) })

	app := seedRunningApp(t, ctx, queries)
	bad := addDomain(t, ctx, queries, app, "bad.belune.local")
	addDomain(t, ctx, queries, app, "good.belune.local")

	mock := &mockProxy{routes: liveRoutes("bad.belune.local", "good.belune.local")}
	reconcileWith(t, ctx, failingDB{pool, failOn("ListRouteFeaturesByDomain", bad.ID)}, mock)

	assert.Empty(t, mock.removed)
	assert.Contains(t, mock.skip, "bad.belune.local")
	assert.ElementsMatch(t, []string{"good.belune.local"}, ensuredHosts(mock))
}

// The regression that matters most: with nothing failing, a stale route is still
// removed. The sweep must not become permanently timid.
func TestReconcile_NoFailure_StillRemovesAStaleRoute(t *testing.T) {
	ctx := context.Background()
	pool, queries, teardown := testutil.SetupTestDB()
	defer teardown()
	t.Cleanup(func() { _ = testutil.TruncateAll(ctx, pool) })

	app := seedRunningApp(t, ctx, queries)
	addDomain(t, ctx, queries, app, "live.belune.local")

	mock := &mockProxy{routes: liveRoutes("live.belune.local", "stale.belune.local")}
	reconcileWith(t, ctx, pool, mock)

	assert.Equal(t, []string{"stale.belune.local"}, mock.removed)
	assert.Equal(t, []string{"live.belune.local"}, ensuredHosts(mock))
}

// The same blind pass must not narrow any full-set sync either: the route is kept
// by the sweep guard, but a skip list, WAF list or CrowdSec list computed from the
// incomplete view would drop a host Caddy is still serving. Sentinels stand for
// what Caddy already holds; "not pushed" is asserted by their surviving intact.
func TestReconcile_DomainsQueryFails_DoesNotNarrowTheFullSetSyncs(t *testing.T) {
	ctx := context.Background()
	pool, queries, teardown := testutil.SetupTestDB()
	defer teardown()
	t.Cleanup(func() { _ = testutil.TruncateAll(ctx, pool) })

	flaky := seedRunningApp(t, ctx, queries)
	addDomain(t, ctx, queries, flaky, "flaky.belune.local") // ssl_mode off
	other := addRunningApp(t, ctx, queries, flaky, "reconcile-project-other")
	addDomain(t, ctx, queries, other, "other.belune.local") // ssl_mode off
	_, err := pool.Exec(ctx, `UPDATE applications SET waf_blocking = true WHERE id = ANY($1)`, []pgtype.UUID{flaky.ID, other.ID})
	require.NoError(t, err)

	previous := []string{"flaky.belune.local", "other.belune.local"}
	mock := &mockProxy{
		routes: liveRoutes("flaky.belune.local", "other.belune.local"),
		skip:   previous,
	}
	reconcileWith(t, ctx, failingDB{pool, failOn("ListDomainsByApplication", flaky.ID)}, mock)

	assert.Equal(t, previous, mock.skip, "the auto-HTTPS lists must not be replaced from an incomplete view")
	assert.Empty(t, mock.waf, "the WAF host list must not be replaced from an incomplete view")
	assert.Empty(t, mock.crowdsec, "the CrowdSec host list must not be replaced from an incomplete view")
	assert.Empty(t, mock.removed)
	assert.Contains(t, ensuredHosts(mock), "other.belune.local", "create and repair still run")

	// Control: the next pass, with the query healthy, pushes the full set again.
	reconcileWith(t, ctx, pool, mock)
	assert.ElementsMatch(t, previous, mock.skip)
	assert.NotEmpty(t, mock.waf, "a complete pass pushes the WAF set")
	require.NotEmpty(t, mock.crowdsec, "a complete pass pushes the CrowdSec set: the assertion above is not vacuous")
	assert.ElementsMatch(t, previous, mock.crowdsec[len(mock.crowdsec)-1].Hosts)
}
