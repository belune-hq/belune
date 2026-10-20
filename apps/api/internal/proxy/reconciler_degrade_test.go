package proxy_test

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"strings"
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

// countingProvider fails the test if it is asked when no probe was warranted.
type countingProvider struct {
	set   capability.Set
	calls int
}

func (c *countingProvider) Capabilities(context.Context) capability.Set {
	c.calls++
	return c.set
}

func rateLimited(host string) proxy.RouteConfig {
	return proxy.RouteConfig{
		Hostname: host,
		Features: []proxy.RouteFeature{
			{Type: proxy.FeatureHeaders, Enabled: true, Config: json.RawMessage(`{"response":{"set":{"X-A":["1"]}}}`)},
			{Type: proxy.FeatureRateLimit, Enabled: true, Config: json.RawMessage(`{"window":"1s","max_events":10}`)},
			{Type: proxy.FeatureBasicAuth, Enabled: true},
		},
	}
}

func featureTypes(cfg proxy.RouteConfig) []string {
	var out []string
	for _, f := range cfg.Features {
		out = append(out, f.Type)
	}
	return out
}

func TestDegrade_DropsOnlyTheUnsatisfiableFeature(t *testing.T) {
	r := proxy.NewReconciler(nil, &mockProxy{}, nil, time.Second).
		WithCapabilities(&countingProvider{set: capability.NewSet()})

	in := []proxy.RouteConfig{rateLimited("a.test"), {Hostname: "plain.test"}}
	out, degraded := r.Degrade(context.Background(), in)

	require.Len(t, out, 2, "a route must degrade, never disappear")
	assert.Equal(t, []string{proxy.FeatureHeaders, proxy.FeatureBasicAuth}, featureTypes(out[0]),
		"every other feature must survive, in order")
	assert.Empty(t, out[1].Features)
	assert.Equal(t, []proxy.Degradation{{
		Hostname: "a.test", Path: "/", Feature: proxy.FeatureRateLimit, Module: capability.ModuleRateLimit,
		Reason: proxy.ReasonModulesUnknown,
	}}, degraded)

	// The caller's slice must be untouched: it is shared with the skip
	// computation and the stale-route sweep.
	assert.Equal(t, []string{proxy.FeatureHeaders, proxy.FeatureRateLimit, proxy.FeatureBasicAuth}, featureTypes(in[0]))
}

func TestDegrade_KeepsFeatureWhenModulePresent(t *testing.T) {
	r := proxy.NewReconciler(nil, &mockProxy{}, nil, time.Second).
		WithCapabilities(&countingProvider{set: capability.NewSet(capability.ModuleRateLimit)})

	out, degraded := r.Degrade(context.Background(), []proxy.RouteConfig{rateLimited("a.test")})

	assert.Empty(t, degraded)
	assert.Contains(t, featureTypes(out[0]), proxy.FeatureRateLimit)
}

func TestDegrade_IgnoresDisabledFeatures(t *testing.T) {
	cfg := rateLimited("a.test")
	cfg.Features[1].Enabled = false
	prov := &countingProvider{set: capability.NewSet()}
	r := proxy.NewReconciler(nil, &mockProxy{}, nil, time.Second).WithCapabilities(prov)

	_, degraded := r.Degrade(context.Background(), []proxy.RouteConfig{cfg})

	assert.Empty(t, degraded, "a disabled feature asks nothing of Caddy, so there is nothing to report")
	assert.Zero(t, prov.calls, "no enabled module-backed feature, no probe")
}

func TestDegrade_NoProviderLeavesRoutesAlone(t *testing.T) {
	r := proxy.NewReconciler(nil, &mockProxy{}, nil, time.Second)

	out, degraded := r.Degrade(context.Background(), []proxy.RouteConfig{rateLimited("a.test")})

	assert.Empty(t, degraded)
	assert.Contains(t, featureTypes(out[0]), proxy.FeatureRateLimit)
}

// The test that matters: one domain naming an absent module must not stop another
// reconciling, must itself still be pushed minus that feature, and an
// ssl_mode=off host must stay in the auto-HTTPS skip list — a hostname is in
// Caddy's route table iff it is in the skip computation.
func TestReconcile_DegradedRouteStillServesAndKeepsSkipEntry(t *testing.T) {
	if os.Getenv("BELUNE_CADDY_INTEGRATION") == "" {
		t.Skip("set BELUNE_CADDY_INTEGRATION=1 (needs Docker for Postgres)")
	}
	ctx := context.Background()
	pool, queries, teardown := testutil.SetupTestDB()
	defer teardown()
	t.Cleanup(func() { _ = testutil.TruncateAll(ctx, pool) })

	app := seedRunningApp(t, ctx, queries)
	limited, err := queries.CreateDomain(ctx, createDomainParams(app.ID, "limited.belune.local", proxy.SSLModeOff))
	require.NoError(t, err)
	_, err = queries.CreateDomain(ctx, createDomainParams(app.ID, "healthy.belune.local", proxy.SSLModeAutomatic))
	require.NoError(t, err)
	_, err = queries.UpsertRouteFeature(ctx, upsertFeatureParams(limited.ID, proxy.FeatureRateLimit))
	require.NoError(t, err)

	mock := &mockProxy{}
	r := proxy.NewReconciler(queries, mock, nil, time.Second).
		WithCapabilities(&countingProvider{set: capability.NewSet()})
	require.NoError(t, r.ReconcileNow(ctx))

	byHost := map[string]proxy.RouteConfig{}
	for _, c := range mock.ensured {
		byHost[c.Hostname] = c
	}
	require.Contains(t, byHost, "limited.belune.local", "the degraded domain must still be pushed")
	require.Contains(t, byHost, "healthy.belune.local", "one degraded domain must not stop another")
	for _, f := range byHost["limited.belune.local"].Features {
		assert.NotEqual(t, proxy.FeatureRateLimit, f.Type, "the absent module's feature must not be pushed")
	}
	assert.Contains(t, mock.skip, "limited.belune.local", "degrading must not desynchronise the skip list")

	require.Len(t, r.Status().Degraded, 1)
	assert.Equal(t, capability.ModuleRateLimit, r.Status().Degraded[0].Module)
}

func createDomainParams(appID pgtype.UUID, host, sslMode string) generated.CreateDomainParams {
	return generated.CreateDomainParams{
		Path:          "/",
		ApplicationID: appID,
		Hostname:      host,
		SslEnabled:    sslMode != proxy.SSLModeOff,
		SslMode:       sslMode,
		ContainerPort: pgtype.Int4{Int32: 8080, Valid: true},
	}
}

func upsertFeatureParams(domainID pgtype.UUID, feature string) generated.UpsertRouteFeatureParams {
	return generated.UpsertRouteFeatureParams{
		DomainID:    domainID,
		FeatureType: feature,
		Config:      []byte(`{"window":"1s","max_events":10}`),
		Enabled:     true,
	}
}

func TestDegrade_ReasonSeparatesAbsentFromUnknown(t *testing.T) {
	cases := map[string]struct {
		set  capability.Set
		want string
	}{
		// A real Caddy always has modules, so a set that has some but not ours was SEEN.
		"module absent":   {capability.NewSet("http.handlers.reverse_proxy"), proxy.ReasonModuleAbsent},
		"probe read none": {capability.NewSet(), proxy.ReasonModulesUnknown},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			r := proxy.NewReconciler(nil, &mockProxy{}, nil, time.Second).
				WithCapabilities(&countingProvider{set: tc.set})
			_, degraded := r.Degrade(context.Background(), []proxy.RouteConfig{rateLimited("a.test")})
			require.Len(t, degraded, 1)
			assert.Equal(t, tc.want, degraded[0].Reason)
		})
	}
}

func TestCarrySince(t *testing.T) {
	t0 := time.Now()
	t1 := t0.Add(30 * time.Second)
	key := func(host, feat string) proxy.Degradation {
		return proxy.Degradation{Hostname: host, Path: "/", Feature: feat}
	}

	first := proxy.CarrySince(nil, []proxy.Degradation{key("a.test", "rate_limit")}, t0)
	assert.True(t, first[0].Since.Equal(t0), "a new degradation starts at this pass")

	second := proxy.CarrySince(first, []proxy.Degradation{key("a.test", "rate_limit"), key("b.test", "rate_limit")}, t1)
	assert.Equal(t, first[0].Since, second[0].Since, "an identical key keeps the EXACT earlier value, monotonic reading included")
	assert.True(t, second[1].Since.Equal(t1), "a newly-appearing key gets the current pass's time")

	other := proxy.Degradation{Hostname: "a.test", Path: "/api", Feature: "rate_limit"}
	third := proxy.CarrySince(second, []proxy.Degradation{other}, t1.Add(time.Minute))
	assert.True(t, third[0].Since.Equal(t1.Add(time.Minute)), "a different path is a different degradation")

	assert.Empty(t, proxy.CarrySince(third, nil, t1), "nothing degraded, nothing carried: the list clears")
}

// A standing degradation must keep its Since and log once, not once per pass; a
// cleared one must leave the status empty.
func TestReconcile_DegradedSinceStableAndLoggedOnce(t *testing.T) {
	if os.Getenv("BELUNE_CADDY_INTEGRATION") == "" {
		t.Skip("set BELUNE_CADDY_INTEGRATION=1 (needs Docker for Postgres)")
	}
	ctx := context.Background()
	pool, queries, teardown := testutil.SetupTestDB()
	defer teardown()
	t.Cleanup(func() { _ = testutil.TruncateAll(ctx, pool) })

	app := seedRunningApp(t, ctx, queries)
	d, err := queries.CreateDomain(ctx, createDomainParams(app.ID, "limited.belune.local", proxy.SSLModeAutomatic))
	require.NoError(t, err)
	_, err = queries.UpsertRouteFeature(ctx, upsertFeatureParams(d.ID, proxy.FeatureRateLimit))
	require.NoError(t, err)

	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	prov := &countingProvider{set: capability.NewSet()}
	r := proxy.NewReconciler(queries, &mockProxy{}, nil, time.Second).WithCapabilities(prov)
	require.NoError(t, r.ReconcileNow(ctx))
	require.Len(t, r.Status().Degraded, 1)
	since := r.Status().Degraded[0].Since
	require.False(t, since.IsZero())

	time.Sleep(10 * time.Millisecond)
	require.NoError(t, r.ReconcileNow(ctx))
	require.NoError(t, r.ReconcileNow(ctx))
	assert.Equal(t, since, r.Status().Degraded[0].Since, "Since must survive later passes unchanged")
	assert.Equal(t, 1, strings.Count(logs.String(), "serving route without a feature"),
		"a standing degradation must not re-log every pass")

	prov.set = capability.NewSet(capability.ModuleRateLimit)
	require.NoError(t, r.ReconcileNow(ctx))
	assert.Empty(t, r.Status().Degraded, "the list clears once the module is available")
}
