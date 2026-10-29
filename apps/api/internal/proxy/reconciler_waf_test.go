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

func TestSyncWAF_HandsTheExclusionsToTheProxy(t *testing.T) {
	mp := &mockProxy{}
	want := []proxy.WAFExclusion{{
		ID: 7, WAFExclusionSpec: proxy.WAFExclusionSpec{RuleID: 942290, Collection: "REQUEST_COOKIES", TargetName: "ph_x"}, Global: true,
	}}
	r := wafReconciler(mp, caddyWithWAF).WithWAFExclusions(func(context.Context) ([]proxy.WAFExclusion, error) { return want, nil })

	require.NoError(t, r.SyncWAF(context.Background(), routes("a.example.com"), "", switchOf(true)))
	require.Len(t, mp.waf, 1)
	assert.Equal(t, want, mp.waf[0].Exclusions)
}

// A route rendered without the exclusions would start detecting what the operator
// had silenced, then flip back on the next good read. Better to push nothing.
func TestSyncWAF_AnUnreadableExclusionListPushesNothing(t *testing.T) {
	mp := &mockProxy{}
	ok := true
	r := wafReconciler(mp, caddyWithWAF).WithWAFExclusions(func(context.Context) ([]proxy.WAFExclusion, error) {
		if ok {
			return []proxy.WAFExclusion{{ID: 1, WAFExclusionSpec: proxy.WAFExclusionSpec{RuleID: 941100}, Global: true}}, nil
		}
		return nil, errors.New("connection refused")
	})
	require.NoError(t, r.SyncWAF(context.Background(), routes("a.example.com"), "", switchOf(true)))
	require.Len(t, mp.waf, 1)

	ok = false
	require.Error(t, r.SyncWAF(context.Background(), routes("a.example.com"), "", switchOf(true)))
	assert.Len(t, mp.waf, 1, "nothing was pushed, so the exclusions stay in force")
}

func blockingOf(hosts ...string) func(context.Context) ([]string, error) {
	return func(context.Context) ([]string, error) { return hosts, nil }
}

func TestSyncWAF_PromotesOnlyHostsThatOptedIn(t *testing.T) {
	mp := &mockProxy{}
	r := wafReconciler(mp, caddyWithWAF).WithWAFBlockingHosts(blockingOf("a.example.com"))
	require.NoError(t, r.SyncWAF(context.Background(), routes("a.example.com", "b.example.com"), "", switchOf(true)))
	require.Len(t, mp.waf, 1)
	assert.Equal(t, []string{"a.example.com", "b.example.com"}, mp.waf[0].Hosts)
	assert.Equal(t, []string{"a.example.com"}, mp.waf[0].BlockingHosts)
}

// The dashboard never reaches Hosts, so it can never be enforced on, even if a
// domains row somehow named it.
func TestSyncWAF_CannotBlockTheDashboard(t *testing.T) {
	mp := &mockProxy{}
	r := wafReconciler(mp, caddyWithWAF).WithWAFBlockingHosts(blockingOf("dash.example.com", "a.example.com"))
	require.NoError(t, r.SyncWAF(context.Background(), routes("a.example.com", "dash.example.com"), "dash.example.com", switchOf(true)))
	assert.Equal(t, []string{"a.example.com"}, mp.waf[0].BlockingHosts)
}

// Reading the list wrong in the direction of empty would demote every blocking
// host and promote it again on the next good read; push nothing instead.
func TestSyncWAF_AnUnreadableBlockingListPushesNothing(t *testing.T) {
	mp := &mockProxy{}
	ok := true
	r := wafReconciler(mp, caddyWithWAF).WithWAFBlockingHosts(func(context.Context) ([]string, error) {
		if ok {
			return []string{"a.example.com"}, nil
		}
		return nil, errors.New("connection refused")
	})
	require.NoError(t, r.SyncWAF(context.Background(), routes("a.example.com"), "", switchOf(true)))
	ok = false
	require.Error(t, r.SyncWAF(context.Background(), routes("a.example.com"), "", switchOf(true)))
	assert.Len(t, mp.waf, 1)
}

func TestBlockingHosts_Narrowing(t *testing.T) {
	hosts := []string{"*.example.com", "a.example.com", "b.test", "shop.example.com"}

	assert.Equal(t, []string{"a.example.com"}, proxy.BlockingHosts(hosts, []string{"a.example.com", "gone.test"}, ""),
		"only hosts the WAF inspects")
	assert.Empty(t, proxy.BlockingHosts(hosts, nil, ""))

	// A wildcard that opted in would also refuse a sibling application's visitors.
	assert.Equal(t, []string{"a.example.com"}, proxy.BlockingHosts(hosts, []string{"*.example.com", "a.example.com"}, ""),
		"the wildcard overlaps shop.example.com, which did not opt in")
	// Dropping the sibling's non-opt-in is not needed when it opted in too.
	assert.Equal(t, []string{"*.example.com", "a.example.com", "shop.example.com"},
		proxy.BlockingHosts(hosts, []string{"*.example.com", "a.example.com", "shop.example.com"}, ""))
	// Different depth never overlaps.
	assert.Equal(t, []string{"*.example.com"}, proxy.BlockingHosts([]string{"*.example.com", "x.y.example.com"}, []string{"*.example.com"}, ""))
}

// ⛔ The dashboard is never named in Hosts, but a wildcard application domain
// covers it anyway: the WAF route matches by glob, so a pattern that is allowed to
// enforce would enforce on the one host whose recovery path is itself.
func TestBlockingHosts_NeverCoverTheDashboard(t *testing.T) {
	hosts := []string{"*.example.com", "a.example.com", "b.test"}
	cands := []string{"*.example.com", "a.example.com", "b.test"}

	got := proxy.BlockingHosts(hosts, cands, "panel.example.com")
	assert.Equal(t, []string{"a.example.com", "b.test"}, got, "the wildcard that would cover panel.example.com is left detecting")

	// Case does not hide it.
	assert.Equal(t, got, proxy.BlockingHosts(hosts, cands, "Panel.Example.COM"))
	// A dashboard elsewhere leaves the wildcard alone.
	assert.Contains(t, proxy.BlockingHosts(hosts, cands, "dash.other.test"), "*.example.com")
}

func TestSyncWAF_AWildcardCannotPromoteTheDashboard(t *testing.T) {
	mp := &mockProxy{}
	r := wafReconciler(mp, caddyWithWAF).WithWAFBlockingHosts(blockingOf("*.example.com"))
	require.NoError(t, r.SyncWAF(context.Background(), routes("*.example.com"), "panel.example.com", switchOf(true)))
	require.Len(t, mp.waf, 1)
	assert.Equal(t, []string{"*.example.com"}, mp.waf[0].Hosts, "it is inspected, which is detection only")
	assert.Empty(t, mp.waf[0].BlockingHosts)
}
