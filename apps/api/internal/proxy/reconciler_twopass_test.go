package proxy_test

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/belune-hq/belune/internal/proxy"
	"github.com/belune-hq/belune/internal/proxy/capability"
	"github.com/belune-hq/belune/internal/testutil"
)

// storeProxy behaves like Caddy for drift: it keeps what it was asked to hold, and
// EnsureRoute reports a change only if the DESIRED route differs byte-for-byte from
// what is held -- the same comparison the real client makes (route JSON, not a
// read-back field).
type storeProxy struct {
	*mockProxy
	mu2  sync.Mutex
	held map[string][]byte
}

func (s *storeProxy) EnsureRoute(_ context.Context, cfg proxy.RouteConfig) (bool, error) {
	desired, err := json.Marshal(cfg)
	if err != nil {
		return false, err
	}
	key := cfg.Hostname + "|" + cfg.Path
	s.mu2.Lock()
	defer s.mu2.Unlock()
	if prev, ok := s.held[key]; ok && string(prev) == string(desired) {
		return false, nil
	}
	s.held[key] = desired
	return true, nil
}

func (s *storeProxy) ListRoutes(context.Context) ([]proxy.RouteConfig, error) {
	s.mu2.Lock()
	defer s.mu2.Unlock()
	var out []proxy.RouteConfig
	for _, raw := range s.held {
		var c proxy.RouteConfig
		if err := json.Unmarshal(raw, &c); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, nil
}

// ⛔ The forever-rewrite is invisible in one pass. EnsureRoute compares the FULL
// desired route against what Caddy holds, so any instability in the desired route
// between passes -- most plausibly the ORDER of the upstream list -- makes every
// route "drift" on every pass and the reconciler rewrites it for ever. Only comparing
// a second pass against the first can show it: against an unchanged database the
// second pass must issue no route write at all.
//
// Built while a route has one upstream so that replicas (C2) inherit a test that
// would catch it, rather than discovering it in a drill.
func TestReconcile_SecondPassAgainstAnUnchangedDatabaseWritesNoRoute(t *testing.T) {
	ctx := context.Background()
	pool, queries, teardown := testutil.SetupTestDB()
	defer teardown()
	t.Cleanup(func() { _ = testutil.TruncateAll(ctx, pool) })

	app := seedRunningApp(t, ctx, queries)
	addDomain(t, ctx, queries, app, "a.belune.local")
	addDomain(t, ctx, queries, app, "b.belune.local")

	sp := &storeProxy{mockProxy: &mockProxy{}, held: map[string][]byte{}}
	r := proxy.NewReconciler(queries, sp, nil, time.Second).
		WithCapabilities(&countingProvider{set: capability.NewSet()})

	require.NoError(t, r.ReconcileNow(ctx))
	first := r.Status()
	require.Equal(t, 2, first.LastAdded, "the first pass creates both routes")

	require.NoError(t, r.ReconcileNow(ctx))
	second := r.Status()
	assert.Equal(t, 0, second.LastAdded, "the second pass adds nothing")
	assert.Equal(t, 0, second.LastUpdated, "and rewrites nothing: the desired route is stable across passes")
	assert.Equal(t, 0, second.LastRemoved)
}
