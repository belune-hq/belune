package proxy_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/belune-hq/belune/internal/proxy"
	"github.com/belune-hq/belune/internal/proxy/capability"
)

var (
	caddyWithCrowdSec    = capability.NewSet("http.handlers.reverse_proxy", capability.ModuleCrowdSec)
	caddyWithoutCrowdSec = capability.NewSet("http.handlers.reverse_proxy")
)

type fakeCrowdSec struct {
	desire proxy.CrowdSecDesire
	err    error
	calls  int
}

func (f *fakeCrowdSec) Prepare(context.Context) (proxy.CrowdSecDesire, error) {
	f.calls++
	return f.desire, f.err
}

func readyDesire() proxy.CrowdSecDesire {
	return proxy.CrowdSecDesire{Enabled: true, Ready: true, APIURL: "http://crowdsec:8080", APIKey: "s3cret-bouncer-key"}
}

func crowdsecReconciler(mp *mockProxy, set capability.Set, src *fakeCrowdSec) *proxy.Reconciler {
	return proxy.NewReconciler(nil, mp, nil, time.Second).
		WithCapabilities(&countingProvider{set: set}).
		WithCrowdSec(src)
}

func TestSyncCrowdSec_ProtectsTheApplicationsAndNotTheDashboard(t *testing.T) {
	mp := &mockProxy{}
	src := &fakeCrowdSec{desire: readyDesire()}
	r := crowdsecReconciler(mp, caddyWithCrowdSec, src)

	require.NoError(t, r.SyncCrowdSec(context.Background(),
		routes("b.example.com", "a.example.com", "b.example.com", "dash.example.com"), "dash.example.com"))

	require.Len(t, mp.crowdsec, 1)
	cfg := mp.crowdsec[0]
	assert.True(t, cfg.Enabled)
	assert.Equal(t, []string{"a.example.com", "b.example.com"}, cfg.Hosts,
		"the dashboard stays out: a ban lifted from it has to be reachable by the person it was placed on")
	assert.Equal(t, []string{"dash.example.com"}, cfg.Exempt,
		"named as an exemption too, because a wildcard application host would otherwise cover it")
	assert.Equal(t, "http://crowdsec:8080", cfg.APIURL)
	assert.Equal(t, "s3cret-bouncer-key", cfg.APIKey)
	st := r.CrowdSec()
	assert.True(t, st.Active)
	assert.Equal(t, 2, st.Hosts)
	assert.Empty(t, st.Reason)
}

func TestSyncCrowdSec_NotWiredDoesNothing(t *testing.T) {
	mp := &mockProxy{}
	r := proxy.NewReconciler(nil, mp, nil, time.Second).WithCapabilities(&countingProvider{set: caddyWithCrowdSec})
	require.NoError(t, r.SyncCrowdSec(context.Background(), routes("a.example.com"), ""))
	assert.Empty(t, mp.crowdsec)
}

func TestSyncCrowdSec_SwitchedOffRemovesIt(t *testing.T) {
	mp := &mockProxy{}
	r := crowdsecReconciler(mp, caddyWithCrowdSec, &fakeCrowdSec{desire: proxy.CrowdSecDesire{Enabled: false}})
	require.NoError(t, r.SyncCrowdSec(context.Background(), routes("a.example.com"), ""))

	require.Len(t, mp.crowdsec, 1)
	assert.False(t, mp.crowdsec[0].Enabled, "off is pushed, so a bouncer left from before is taken down")
	assert.False(t, r.CrowdSec().Active)
	assert.Empty(t, r.CrowdSec().Reason)
}

// Absent, unknown and "the local API is down" are three different things and three
// different actions.
func TestSyncCrowdSec_AbsentIsNotUnknownIsNotUnavailable(t *testing.T) {
	ctx := context.Background()

	t.Run("module absent removes the route", func(t *testing.T) {
		mp := &mockProxy{}
		r := crowdsecReconciler(mp, caddyWithoutCrowdSec, &fakeCrowdSec{desire: readyDesire()})
		require.NoError(t, r.SyncCrowdSec(ctx, routes("a.example.com"), ""))
		require.Len(t, mp.crowdsec, 1)
		assert.False(t, mp.crowdsec[0].Enabled, "Caddy would refuse a route naming a handler it does not have")
		assert.Equal(t, proxy.ReasonModuleAbsent, r.CrowdSec().Reason)
	})

	t.Run("module unknown touches nothing and keeps what was true", func(t *testing.T) {
		mp := &mockProxy{}
		src := &fakeCrowdSec{desire: readyDesire()}
		good := proxy.NewReconciler(nil, mp, nil, time.Second).WithCapabilities(&countingProvider{set: caddyWithCrowdSec}).WithCrowdSec(src)
		require.NoError(t, good.SyncCrowdSec(ctx, routes("a.example.com"), ""))
		require.True(t, good.CrowdSec().Active)
		before := len(mp.crowdsec)

		// A failed probe returns an empty set. Same reconciler, probe now failing.
		blind := &countingProvider{set: capability.Set{}}
		r := proxy.NewReconciler(nil, mp, nil, time.Second).WithCapabilities(blind).WithCrowdSec(src)
		require.NoError(t, r.SyncCrowdSec(ctx, routes("a.example.com"), ""))
		assert.Len(t, mp.crowdsec, before, "a failed probe is not evidence the module is gone")
		assert.Equal(t, proxy.ReasonModulesUnknown, r.CrowdSec().Reason)
	})

	t.Run("local API unavailable leaves a working route alone", func(t *testing.T) {
		mp := &mockProxy{}
		src := &fakeCrowdSec{desire: readyDesire()}
		r := crowdsecReconciler(mp, caddyWithCrowdSec, src)
		require.NoError(t, r.SyncCrowdSec(ctx, routes("a.example.com"), ""))
		require.True(t, r.CrowdSec().Active)
		before := len(mp.crowdsec)

		src.desire = proxy.CrowdSecDesire{Enabled: true, Ready: false, Reason: proxy.ReasonCrowdSecUnavailable}
		require.NoError(t, r.SyncCrowdSec(ctx, routes("a.example.com"), ""))
		assert.Len(t, mp.crowdsec, before,
			"the module goes on enforcing the bans it holds through a restart; removing the route would end that")
		st := r.CrowdSec()
		assert.True(t, st.Active, "still in Caddy")
		assert.Equal(t, proxy.ReasonCrowdSecUnavailable, st.Reason)
	})

	t.Run("local API unavailable never writes a route that has no bouncer behind it", func(t *testing.T) {
		mp := &mockProxy{}
		r := crowdsecReconciler(mp, caddyWithCrowdSec, &fakeCrowdSec{desire: proxy.CrowdSecDesire{Enabled: true, Reason: proxy.ReasonCrowdSecUnavailable}})
		require.NoError(t, r.SyncCrowdSec(ctx, routes("a.example.com"), ""))
		assert.Empty(t, mp.crowdsec)
		assert.False(t, r.CrowdSec().Active)
		assert.Equal(t, proxy.ReasonCrowdSecUnavailable, r.CrowdSec().Reason)
	})
}

func TestSyncCrowdSec_OnWithNothingToCover(t *testing.T) {
	mp := &mockProxy{}
	r := crowdsecReconciler(mp, caddyWithCrowdSec, &fakeCrowdSec{desire: readyDesire()})
	require.NoError(t, r.SyncCrowdSec(context.Background(), nil, ""))
	require.Len(t, mp.crowdsec, 1)
	assert.Empty(t, mp.crowdsec[0].Hosts)
	assert.False(t, r.CrowdSec().Active)
	assert.Equal(t, proxy.ReasonNoApplications, r.CrowdSec().Reason)
}

func TestSyncCrowdSec_AFailureToPrepareTouchesNothing(t *testing.T) {
	mp := &mockProxy{}
	src := &fakeCrowdSec{desire: readyDesire()}
	r := crowdsecReconciler(mp, caddyWithCrowdSec, src)
	require.NoError(t, r.SyncCrowdSec(context.Background(), routes("a.example.com"), ""))
	before := len(mp.crowdsec)

	src.err = errors.New("database is down")
	err := r.SyncCrowdSec(context.Background(), routes("a.example.com"), "")
	require.Error(t, err)
	assert.Len(t, mp.crowdsec, before, "reading the switch wrong in the direction of off would remove a working bouncer")
	assert.True(t, r.CrowdSec().Active)
}

func TestSyncCrowdSec_ARefusalFromCaddyIsReportedAndTheOldStateKept(t *testing.T) {
	mp := &mockProxy{}
	r := crowdsecReconciler(mp, caddyWithCrowdSec, &fakeCrowdSec{desire: readyDesire()})
	require.NoError(t, r.SyncCrowdSec(context.Background(), routes("a.example.com"), ""))
	require.True(t, r.CrowdSec().Active)

	mp.crowdsecErr = errors.New("HTTP 500: loading new config: module not registered")
	err := r.SyncCrowdSec(context.Background(), routes("a.example.com", "b.example.com"), "")
	require.Error(t, err)
	st := r.CrowdSec()
	assert.True(t, st.Active, "the route that is in Caddy is still there")
	assert.Contains(t, st.Error, "module not registered")
}

func TestCrowdSecDesireAndConfigNeverPrintTheKey(t *testing.T) {
	d := readyDesire()
	cfg := proxy.CrowdSecConfig{Enabled: true, APIURL: "http://crowdsec:8080", APIKey: d.APIKey}
	for _, s := range []string{d.String(), cfg.String(), cfg.LogValue().String()} {
		assert.False(t, strings.Contains(s, d.APIKey), "%q leaks the bouncer key", s)
	}
}
