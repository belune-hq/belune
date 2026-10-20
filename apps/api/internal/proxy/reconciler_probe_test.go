package proxy_test

import (
	"context"
	"errors"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/weiliang79/belune/internal/proxy"
	"github.com/weiliang79/belune/internal/testutil"
)

// Caddy's states as the probe can see them.
const (
	caddyUp int32 = iota
	caddyDown
	caddyEmpty
)

// scriptedCaddy answers ListRoutes from a state the test flips, so a transition
// happens exactly when the test says rather than when a call count says.
type scriptedCaddy struct {
	*mockProxy
	state atomic.Int32
}

func (s *scriptedCaddy) ListRoutes(context.Context) ([]proxy.RouteConfig, error) {
	switch s.state.Load() {
	case caddyDown:
		return nil, errors.New("connection refused")
	case caddyEmpty:
		return nil, nil
	}
	return []proxy.RouteConfig{{Hostname: "probe.belune.local"}}, nil
}

func (s *scriptedCaddy) passes() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.ensured)
}

// The interval is an hour, so a pass between the opening one and the end of the
// test can only have come from the probe. With the production 30s a passing test
// would prove nothing: the tick would have fixed it anyway.
func TestRun_ProbeReconcilesOnTransitionsOnly(t *testing.T) {
	if os.Getenv("BELUNE_CADDY_INTEGRATION") == "" {
		t.Skip("set BELUNE_CADDY_INTEGRATION=1 (needs Docker for Postgres)")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pool, queries, teardown := testutil.SetupTestDB()
	defer teardown()
	t.Cleanup(func() { _ = testutil.TruncateAll(context.Background(), pool) })

	app := seedRunningApp(t, ctx, queries)
	_, err := queries.CreateDomain(ctx, createDomainParams(app.ID, "probe.belune.local", proxy.SSLModeAutomatic))
	require.NoError(t, err)

	fake := &scriptedCaddy{mockProxy: &mockProxy{}}
	r := proxy.NewReconciler(queries, fake, nil, time.Hour)
	go r.Run(ctx)

	within := 2*proxy.CaddyProbeInterval + time.Second
	steady := 3 * proxy.CaddyProbeInterval

	require.Eventually(t, func() bool { return fake.passes() == 1 }, within, 100*time.Millisecond,
		"the opening pass")
	time.Sleep(steady)
	require.Equal(t, 1, fake.passes(), "a healthy Caddy must not be reconciled")

	// Caddy restarted: up, but none of our routes.
	fake.state.Store(caddyEmpty)
	require.Eventually(t, func() bool { return fake.passes() == 2 }, within, 100*time.Millisecond,
		"lost routes must be reconciled by the probe, not the hour-long tick")
	time.Sleep(steady)
	require.Equal(t, 2, fake.passes(), "a pass that cannot restore the routes must not retrigger on every probe")

	// Unreachable is recorded and nothing more.
	fake.state.Store(caddyDown)
	time.Sleep(steady)
	require.Equal(t, 2, fake.passes(), "an unreachable Caddy is not something a pass can fix")

	// ...but its coming back is.
	fake.state.Store(caddyUp)
	require.Eventually(t, func() bool { return fake.passes() == 3 }, within, 100*time.Millisecond,
		"Caddy becoming reachable again must reconcile")
}
