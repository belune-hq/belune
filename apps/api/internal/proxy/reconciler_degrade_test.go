package proxy_test

import (
	"context"
	"encoding/json"
	"os"
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
