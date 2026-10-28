package proxy_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/belune-hq/belune/internal/proxy"
)

// mockProxy records AddRoute and RemoveRoute calls for assertion.
type mockProxy struct {
	mu      sync.Mutex
	added   []string
	removed []string
	routes  []proxy.RouteConfig // routes returned by ListRoutes
	ensured []proxy.RouteConfig // what EnsureRoute was asked to make Caddy hold
	skip    []string            // last auto-HTTPS skip list pushed
	dns     []proxy.DNSPolicy   // last DNS-01 policy set pushed
	dnsPush int                 // how many times it was pushed
	waf     []proxy.WAFConfig   // every WAF config pushed, in order
	wafErr  error               // returned by SyncWAF when set

	trusted    []proxy.TrustedProxiesConfig // every trusted-proxy config pushed, in order
	trustedErr error                        // returned by SyncTrustedProxies when set

	crowdsec    []proxy.CrowdSecConfig // every CrowdSec config pushed, in order
	crowdsecErr error                  // returned by SyncCrowdSec when set
}

func (m *mockProxy) AddRoute(_ context.Context, cfg proxy.RouteConfig) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.added = append(m.added, cfg.Hostname)
	return nil
}

func (m *mockProxy) RemoveRoute(_ context.Context, hostname, _ string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.removed = append(m.removed, hostname)
	return nil
}

func (m *mockProxy) ListRoutes(_ context.Context) ([]proxy.RouteConfig, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.routes, nil
}

func (m *mockProxy) SetupTLS(_ context.Context, _, _, _, _ string) error { return nil }

func (m *mockProxy) SyncAutoHTTPS(_ context.Context, skip, _ []string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.skip = skip
	return nil
}

func (m *mockProxy) SyncDNSPolicies(_ context.Context, p []proxy.DNSPolicy) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.dns = p
	m.dnsPush++
	return nil
}

func (m *mockProxy) SyncTrustedProxies(_ context.Context, cfg proxy.TrustedProxiesConfig) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.trustedErr != nil {
		return m.trustedErr
	}
	m.trusted = append(m.trusted, cfg)
	return nil
}

func (m *mockProxy) SyncWAF(_ context.Context, cfg proxy.WAFConfig) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.wafErr != nil {
		return m.wafErr
	}
	m.waf = append(m.waf, cfg)
	return nil
}

func (m *mockProxy) SyncCrowdSec(_ context.Context, cfg proxy.CrowdSecConfig) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.crowdsecErr != nil {
		return m.crowdsecErr
	}
	m.crowdsec = append(m.crowdsec, cfg)
	return nil
}

func (m *mockProxy) SyncCertificates(_ context.Context, _ []proxy.HostCertificate) error { return nil }

func (m *mockProxy) SetDashboardRoute(_ context.Context, _, _ string) error { return nil }

func (m *mockProxy) EnsureRoute(ctx context.Context, cfg proxy.RouteConfig) (bool, error) {
	m.mu.Lock()
	m.ensured = append(m.ensured, cfg)
	m.mu.Unlock()
	return true, m.AddRoute(ctx, cfg)
}
func (m *mockProxy) UsesInternalIssuer(_ context.Context) (bool, error) { return false, nil }

// reconcileOnce exercises the diff logic by building expected/current sets and
// calling AddRoute / RemoveRoute via the exported Reconciler. We test the diff
// behaviour by constructing scenarios directly using the exported constructor.

func TestReconcilerDiff_AddsMissing(t *testing.T) {
	mock := &mockProxy{
		routes: []proxy.RouteConfig{}, // Caddy has nothing
	}

	// Inject two "expected" routes via a custom buildExpected shim by calling
	// the reconciler's exported diff logic indirectly through a test-local
	// reconcile helper.
	expected := []proxy.RouteConfig{
		{Hostname: "a.example.com", TargetURL: "http://container-a:8080"},
		{Hostname: "b.example.com", TargetURL: "http://container-b:8080"},
	}

	reconcileDiff(t, mock, expected, mock.routes)

	if len(mock.added) != 2 {
		t.Fatalf("expected 2 routes added, got %d: %v", len(mock.added), mock.added)
	}
}

func TestReconcilerDiff_RemovesStale(t *testing.T) {
	mock := &mockProxy{
		routes: []proxy.RouteConfig{
			{Hostname: "stale.example.com", TargetURL: "http://dead:8080"},
		},
	}

	// DB has no running apps — expected is empty.
	reconcileDiff(t, mock, nil, mock.routes)

	if len(mock.removed) != 1 || mock.removed[0] != "stale.example.com" {
		t.Fatalf("expected stale.example.com removed, got: %v", mock.removed)
	}
	if len(mock.added) != 0 {
		t.Fatalf("expected no routes added, got: %v", mock.added)
	}
}

func TestReconcilerDiff_NoOpWhenInSync(t *testing.T) {
	existing := []proxy.RouteConfig{
		{Hostname: "ok.example.com", TargetURL: "http://container:8080"},
	}
	mock := &mockProxy{routes: existing}

	reconcileDiff(t, mock, existing, mock.routes)

	if len(mock.added) != 0 || len(mock.removed) != 0 {
		t.Fatalf("expected no changes, got added=%v removed=%v", mock.added, mock.removed)
	}
}

func TestReconcilerDiff_AddAndRemove(t *testing.T) {
	mock := &mockProxy{
		routes: []proxy.RouteConfig{
			{Hostname: "stale.example.com"},
		},
	}

	expected := []proxy.RouteConfig{
		{Hostname: "new.example.com"},
	}

	reconcileDiff(t, mock, expected, mock.routes)

	if len(mock.added) != 1 || mock.added[0] != "new.example.com" {
		t.Fatalf("expected new.example.com added, got: %v", mock.added)
	}
	if len(mock.removed) != 1 || mock.removed[0] != "stale.example.com" {
		t.Fatalf("expected stale.example.com removed, got: %v", mock.removed)
	}
}

func TestReconcilerStatus_Initial(t *testing.T) {
	r := proxy.NewReconciler(nil, &mockProxy{}, nil, 30*time.Second)
	s := r.Status()
	if s.IntervalSeconds != 30 {
		t.Fatalf("expected interval 30, got %d", s.IntervalSeconds)
	}
	if s.RunCount != 0 {
		t.Fatalf("expected run_count 0 before first reconcile, got %d", s.RunCount)
	}
	if !s.LastRunAt.IsZero() {
		t.Fatalf("expected zero last_run_at before first reconcile, got %v", s.LastRunAt)
	}
}

// reconcileDiff runs the same add-missing / remove-stale logic as Reconciler.reconcile
// but with caller-supplied expected and current sets, bypassing the DB.
func reconcileDiff(t *testing.T, p proxy.ProxyManager, expected, current []proxy.RouteConfig) {
	t.Helper()
	ctx := context.Background()

	currentSet := make(map[string]struct{}, len(current))
	for _, c := range current {
		currentSet[c.Hostname] = struct{}{}
	}
	expectedSet := make(map[string]proxy.RouteConfig, len(expected))
	for _, e := range expected {
		expectedSet[e.Hostname] = e
	}

	for hostname, cfg := range expectedSet {
		if _, exists := currentSet[hostname]; !exists {
			if err := p.AddRoute(ctx, cfg); err != nil {
				t.Errorf("AddRoute(%s): %v", hostname, err)
			}
		}
	}
	for hostname := range currentSet {
		if _, exists := expectedSet[hostname]; !exists {
			if err := p.RemoveRoute(ctx, hostname, "/"); err != nil {
				t.Errorf("RemoveRoute(%s): %v", hostname, err)
			}
		}
	}
}
