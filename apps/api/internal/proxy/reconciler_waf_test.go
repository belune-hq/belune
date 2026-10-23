package proxy_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/belune-hq/belune/internal/proxy"
	"github.com/belune-hq/belune/internal/proxy/capability"
)

func switchOf(on bool) func(context.Context) (bool, error) {
	return func(context.Context) (bool, error) { return on, nil }
}

func routes(hosts ...string) []proxy.RouteConfig {
	out := make([]proxy.RouteConfig, 0, len(hosts))
	for _, h := range hosts {
		out = append(out, proxy.RouteConfig{Hostname: h})
	}
	return out
}

var (
	caddyWithWAF    = capability.NewSet("http.handlers.reverse_proxy", capability.ModuleWAF)
	caddyWithoutWAF = capability.NewSet("http.handlers.reverse_proxy")
)

func wafReconciler(mp *mockProxy, set capability.Set) *proxy.Reconciler {
	return proxy.NewReconciler(nil, mp, nil, time.Second).WithCapabilities(&countingProvider{set: set})
}

func TestSyncWAF_ProtectsTheApplicationsAndNotTheDashboard(t *testing.T) {
	mp := &mockProxy{}
	r := wafReconciler(mp, caddyWithWAF)

	// Two routes for one host (a path split), and the dashboard's own.
	expected := routes("b.example.com", "a.example.com", "b.example.com", "dash.example.com")
	require.NoError(t, r.SyncWAF(context.Background(), expected, "dash.example.com", switchOf(true)))

	require.Len(t, mp.waf, 1)
	assert.True(t, mp.waf[0].Enabled)
	assert.Equal(t, []string{"a.example.com", "b.example.com"}, mp.waf[0].Hosts,
		"sorted and deduplicated, with the dashboard left out: its API takes commands and credentials as ordinary input")
	st := r.WAF()
	assert.True(t, st.Active)
	assert.Equal(t, 2, st.Hosts)
	assert.Empty(t, st.Reason)
}

func TestSyncWAF_SwitchedOffRemovesTheRoute(t *testing.T) {
	mp := &mockProxy{}
	r := wafReconciler(mp, caddyWithWAF)
	require.NoError(t, r.SyncWAF(context.Background(), routes("a.example.com"), "", switchOf(false)))

	require.Len(t, mp.waf, 1)
	assert.False(t, mp.waf[0].Enabled, "off is pushed, so a route left from before is taken down")
	assert.False(t, r.WAF().Active)
	assert.Empty(t, r.WAF().Reason, "an operator's choice needs no explaining")
}

// Three answers to "can this Caddy do it", and they are three different actions.
func TestSyncWAF_AbsentIsNotUnknown(t *testing.T) {
	ctx := context.Background()

	t.Run("module absent: the route is taken down", func(t *testing.T) {
		mp := &mockProxy{}
		r := wafReconciler(mp, caddyWithoutWAF)
		require.NoError(t, r.SyncWAF(ctx, routes("a.example.com"), "", switchOf(true)))
		require.Len(t, mp.waf, 1)
		assert.False(t, mp.waf[0].Enabled, "Caddy would refuse a route that names a module it lacks")
		assert.Equal(t, proxy.ReasonModuleAbsent, r.WAF().Reason)
		assert.False(t, r.WAF().Active)
	})

	t.Run("could not ask: nothing is touched", func(t *testing.T) {
		mp := &mockProxy{}
		cp := &countingProvider{set: caddyWithWAF}
		r := proxy.NewReconciler(nil, mp, nil, time.Second).WithCapabilities(cp)
		require.NoError(t, r.SyncWAF(ctx, routes("a.example.com"), "", switchOf(true)))
		require.True(t, r.WAF().Active, "the first pass puts it up")

		// The probe fails (Caddy restarting, or no exec access). A failed probe is not
		// evidence the module is gone: acting on it would take down a working WAF and
		// put it back on the next pass.
		cp.set = capability.Set{}
		require.NoError(t, r.SyncWAF(ctx, routes("a.example.com"), "", switchOf(true)))
		assert.Len(t, mp.waf, 1, "no second push")
		st := r.WAF()
		assert.True(t, st.Active, "it is still up, and the status says so")
		assert.Equal(t, proxy.ReasonModulesUnknown, st.Reason, "...while saying it could not be verified")
	})

	t.Run("no probe wired is also could-not-ask", func(t *testing.T) {
		mp := &mockProxy{}
		r := proxy.NewReconciler(nil, mp, nil, time.Second)
		require.NoError(t, r.SyncWAF(ctx, routes("a.example.com"), "", switchOf(true)))
		assert.Empty(t, mp.waf)
		assert.Equal(t, proxy.ReasonModulesUnknown, r.WAF().Reason)
	})
}

func TestSyncWAF_OnWithNothingToInspect(t *testing.T) {
	mp := &mockProxy{}
	r := wafReconciler(mp, caddyWithWAF)
	require.NoError(t, r.SyncWAF(context.Background(), nil, "", switchOf(true)))

	st := r.WAF()
	assert.False(t, st.Active)
	assert.Equal(t, proxy.ReasonNoApplications, st.Reason)
	require.Len(t, mp.waf, 1)
	assert.Empty(t, mp.waf[0].Hosts)
}

func TestSyncWAF_AFailureReadingTheSwitchTouchesNothing(t *testing.T) {
	mp := &mockProxy{}
	r := wafReconciler(mp, caddyWithWAF)
	require.NoError(t, r.SyncWAF(context.Background(), routes("a.example.com"), "", switchOf(true)))

	// A transient database error must not read as "off": that would take down a
	// working WAF.
	err := r.SyncWAF(context.Background(), routes("a.example.com"), "", func(context.Context) (bool, error) {
		return false, errors.New("connection refused")
	})
	require.Error(t, err)
	assert.Len(t, mp.waf, 1, "nothing was pushed")
	assert.True(t, r.WAF().Active)
}

func TestSyncWAF_ARefusalFromCaddyIsReportedAndTheOldStateKept(t *testing.T) {
	mp := &mockProxy{}
	r := wafReconciler(mp, caddyWithWAF)
	require.NoError(t, r.SyncWAF(context.Background(), routes("a.example.com"), "", switchOf(true)))

	mp.wafErr = errors.New("add WAF route: HTTP 500: unknown module")
	err := r.SyncWAF(context.Background(), routes("a.example.com", "b.example.com"), "", switchOf(true))
	require.Error(t, err)
	st := r.WAF()
	assert.Contains(t, st.Error, "unknown module")
	assert.True(t, st.Active, "the route that is up stays reported as up")
}
