package proxy_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/belune-hq/belune/internal/pkg/crypto"
	"github.com/belune-hq/belune/internal/proxy"
	"github.com/belune-hq/belune/internal/proxy/caddy"
	"github.com/belune-hq/belune/internal/proxy/capability"
	"github.com/belune-hq/belune/internal/store/generated"
	"github.com/belune-hq/belune/internal/testutil"
)

// The whole chain for the WAF: the operator's switch in the database, through the
// real reconciler (which reads it with the real query), into a real Caddy that has
// the module. The unit tests cover each decision with the switch injected; this is
// the seam they cannot reach.
//
//	BELUNE_CADDY_INTEGRATION=1 go test -p 1 ./internal/proxy/ -run RealStack_WAF -v
func TestRealStack_WAF_TheSwitchReachesCaddy(t *testing.T) {
	if os.Getenv("BELUNE_CADDY_INTEGRATION") == "" {
		t.Skip("set BELUNE_CADDY_INTEGRATION=1 to run against a real Caddy (with the WAF module) + Postgres")
	}
	ctx := context.Background()
	pool, queries, teardown := testutil.SetupTestDB()
	defer teardown()
	t.Cleanup(func() { _ = testutil.TruncateAll(ctx, pool) })

	keyring, err := crypto.ParseKeyringEnv("", testutil.TestEncryptionKey, "")
	require.NoError(t, err)

	adminURL := os.Getenv("CADDY_ADMIN_URL")
	if adminURL == "" {
		adminURL = "http://localhost:2019"
	}
	client := caddy.New(adminURL)
	require.NoError(t, client.Ping(ctx), "caddy admin API not reachable")
	client.InitCatchAll(ctx)

	const (
		hostA     = "waf-a.belune.local"
		hostB     = "waf-b.belune.local"
		dashboard = "waf-dash.belune.local"
	)
	t.Cleanup(func() {
		_ = client.SyncWAF(ctx, proxy.WAFConfig{})
		_ = client.RemoveRoute(ctx, hostA, "/")
		_ = client.RemoveRoute(ctx, hostB, "/")
		_ = client.SetDashboardRoute(ctx, "", proxy.SSLModeOff)
	})

	app := seedRunningApp(t, ctx, queries)
	addDomain := func(host string) {
		_, err := queries.CreateDomain(ctx, generated.CreateDomainParams{
			Path: "/", ApplicationID: app.ID, Hostname: host, SslMode: proxy.SSLModeOff,
			ContainerPort: pgtype.Int4{Int32: 8080, Valid: true},
		})
		require.NoError(t, err)
	}
	set := func(key, value string) {
		_, err := queries.UpsertSetting(ctx, generated.UpsertSettingParams{Key: key, Value: value})
		require.NoError(t, err)
	}
	caps := &countingProvider{set: capability.NewSet("http.handlers.reverse_proxy", capability.ModuleWAF)}
	r := proxy.NewReconciler(queries, client, keyring, 30*time.Second).WithCapabilities(caps)
	pass := func() { require.NoError(t, r.ReconcileNow(ctx)) }

	// The dashboard has a hostname of its own and no domains row; it must not be
	// among the hosts the WAF inspects.
	set(proxy.SettingDashboardDomain, dashboard)
	set(proxy.SettingDashboardSSLMode, proxy.SSLModeOff)
	addDomain(hostA)

	// Never switched on: no setting at all means off, and nothing is in Caddy.
	pass()
	_, present := wafRouteHosts(t, adminURL)
	assert.False(t, present, "no switch, no route")
	assert.False(t, r.WAF().Active)

	set(proxy.SettingWAFEnabled, "true")
	pass()
	hosts, present := wafRouteHosts(t, adminURL)
	require.True(t, present, "switched on: the route is in Caddy")
	assert.Equal(t, []string{hostA}, hosts, "the application's host, and not the dashboard's")
	assert.True(t, r.WAF().Active)
	assert.Equal(t, 1, r.WAF().Hosts)

	// A new domain joins the host list on the next pass.
	addDomain(hostB)
	pass()
	hosts, _ = wafRouteHosts(t, adminURL)
	assert.Equal(t, []string{hostA, hostB}, hosts)

	// An application's opt-in reaches Caddy as a host-scoped promotion, through the
	// real query; turning it off takes it away again within one pass. The dashboard
	// is never among the hosts, so it cannot be named.
	_, err = pool.Exec(ctx, `UPDATE applications SET waf_blocking = TRUE WHERE id = $1`, app.ID)
	require.NoError(t, err)
	pass()
	blocking := wafRouteJSON(t, adminURL)
	assert.Contains(t, blocking, "ctl:ruleEngine=On")
	assert.Contains(t, blocking, `waf-a\\.belune\\.local`)
	assert.NotContains(t, blocking, "waf-dash")
	_, err = pool.Exec(ctx, `UPDATE applications SET waf_blocking = FALSE WHERE id = $1`, app.ID)
	require.NoError(t, err)
	pass()
	assert.NotContains(t, wafRouteJSON(t, adminURL), "ruleEngine", "back to detection-only")

	// Nothing changed: a further pass must not rewrite the route (a write is a reload).
	before := wafRouteJSON(t, adminURL)
	pass()
	assert.JSONEq(t, before, wafRouteJSON(t, adminURL))

	// A probe that cannot answer leaves a running WAF alone, rather than taking it
	// down on evidence that is not evidence.
	caps.set = capability.Set{}
	pass()
	_, present = wafRouteHosts(t, adminURL)
	assert.True(t, present, "could not ask is not the module being gone")
	assert.True(t, r.WAF().Active)
	assert.Equal(t, proxy.ReasonModulesUnknown, r.WAF().Reason)
	caps.set = capability.NewSet("http.handlers.reverse_proxy", capability.ModuleWAF)

	// Switched off: the route is taken down.
	set(proxy.SettingWAFEnabled, "false")
	pass()
	_, present = wafRouteHosts(t, adminURL)
	assert.False(t, present)
	assert.False(t, r.WAF().Active)
}

func wafRouteJSON(t *testing.T, adminURL string) string {
	t.Helper()
	resp, err := http.Get(adminURL + "/id/route-waf")
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return string(b)
}

// wafRouteHosts reads the WAF route's host matcher back from Caddy.
func wafRouteHosts(t *testing.T, adminURL string) ([]string, bool) {
	t.Helper()
	resp, err := http.Get(adminURL + "/id/route-waf")
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, false // Caddy answers an unknown @id with an error
	}
	var route struct {
		Match []struct {
			Host []string `json:"host"`
		} `json:"match"`
	}
	require.NoError(t, json.Unmarshal(body, &route), string(body))
	require.Len(t, route.Match, 1)
	return route.Match[0].Host, true
}
