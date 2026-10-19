package caddy

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/weiliang79/belune/internal/proxy"
)

func rateLimitedRoute(host, path string) proxy.RouteConfig {
	return proxy.RouteConfig{
		Hostname:  host,
		Path:      path,
		TargetURL: "http://127.0.0.1:9",
		Features: []proxy.RouteFeature{{
			Type:    proxy.FeatureRateLimit,
			Enabled: true,
			Config:  json.RawMessage(`{"window":"10s","max_events":2}`),
		}},
	}
}

// rateLimitHandler returns the rate_limit handler buildRoute emitted, and fails
// the test if there is not exactly one.
func rateLimitHandler(t *testing.T, cfg proxy.RouteConfig) map[string]any {
	t.Helper()
	route, err := buildRoute(cfg)
	require.NoError(t, err)

	var found []caddyHandle
	for _, h := range route.Handle {
		if h["handler"] == "rate_limit" {
			found = append(found, h)
		}
	}
	require.Len(t, found, 1)
	return found[0]
}

func TestRateLimitHandler_UsesTheModulesOwnShape(t *testing.T) {
	h := rateLimitHandler(t, rateLimitedRoute("shop.test", "/"))

	// The module rejects top-level rate/burst/zone/key (measured), and
	// "distributed" would silently enable distributed limiting. Pin the exact
	// keys so none of them can creep in.
	assert.ElementsMatch(t, []string{"handler", "rate_limits"}, keys(h))

	zones := h["rate_limits"].(map[string]any)
	require.Len(t, zones, 1)
	for _, z := range zones {
		zone := z.(map[string]any)
		assert.ElementsMatch(t, []string{"key", "window", "max_events"}, keys(zone))
		assert.Equal(t, "{http.request.remote.host}", zone["key"], "per client IP; omitted, the module uses one global bucket")
		assert.Equal(t, "10s", zone["window"])
		assert.Equal(t, 2, zone["max_events"])
	}
}

// Zones with the same name share one counter across domains (measured), so the
// zone must differ per route or two domains both set to the same limit would
// consume each other's budget.
func TestRateLimitHandler_ZoneIsPerRoute(t *testing.T) {
	zoneOf := func(cfg proxy.RouteConfig) string {
		for name := range rateLimitHandler(t, cfg)["rate_limits"].(map[string]any) {
			return name
		}
		return ""
	}

	a := zoneOf(rateLimitedRoute("a.test", "/"))
	b := zoneOf(rateLimitedRoute("b.test", "/"))
	apiPath := zoneOf(rateLimitedRoute("a.test", "/api"))

	assert.NotEmpty(t, a)
	assert.NotEqual(t, a, b, "two hosts, one shared budget")
	assert.NotEqual(t, a, apiPath, "two paths of one host, one shared budget")
}

func TestRateLimitHandler_RunsBeforeTheProxy(t *testing.T) {
	route, err := buildRoute(rateLimitedRoute("shop.test", "/"))
	require.NoError(t, err)

	limit, proxied := -1, -1
	for i, h := range route.Handle {
		switch h["handler"] {
		case "rate_limit":
			limit = i
		case "reverse_proxy":
			proxied = i
		}
	}
	require.GreaterOrEqual(t, limit, 0)
	require.GreaterOrEqual(t, proxied, 0)
	assert.Less(t, limit, proxied)
}

// A row stored in the old form must not take the route down: it is skipped and
// the route still serves.
func TestRateLimitHandler_LegacyRowIsSkippedNotFatal(t *testing.T) {
	cfg := rateLimitedRoute("shop.test", "/")
	cfg.Features[0].Config = json.RawMessage(`{"rate":"10/s","burst":5}`)

	route, err := buildRoute(cfg)
	require.NoError(t, err)
	for _, h := range route.Handle {
		assert.NotEqual(t, "rate_limit", h["handler"])
	}
}

func keys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// Real-Caddy proof that the handler is enforced, not merely accepted: the admin
// API takes a max_events of 0 with a 200 and the route then answers nothing, so
// "the POST succeeded" is no evidence at all. Drives real requests and asserts
// the 429.
//
// Needs a Caddy carrying the rate_limit module — the dev stack's stock
// caddy:2-alpine will skip, with a message — and the address of its :80:
//
//	CADDY_ADMIN_URL=http://localhost:12019 CADDY_HTTP_ADDR=localhost:18080 \
//	  BELUNE_CADDY_INTEGRATION=1 go test -p 1 ./internal/proxy/caddy/ -run RealCaddy_RateLimit -v
func TestRealCaddy_RateLimitIsEnforcedPerDomain(t *testing.T) {
	c := liveClient(t)
	ctx := context.Background()

	// As at startup: the Caddyfile's anonymous catch-all would otherwise sit ahead
	// of the route and answer every request itself.
	c.InitCatchAll(ctx)

	const hostA, hostB = "ratelimit-a.belune.local", "ratelimit-b.belune.local"
	t.Cleanup(func() {
		_ = c.RemoveRoute(ctx, hostA, "/")
		_ = c.RemoveRoute(ctx, hostB, "/")
	})

	for _, host := range []string{hostA, hostB} {
		cfg := rateLimitedRoute(host, "/")
		cfg.SSLMode = proxy.SSLModeOff
		if err := c.AddRoute(ctx, cfg); err != nil {
			if strings.Contains(err.Error(), "unknown module: http.handlers.rate_limit") {
				t.Skip("this Caddy has no rate_limit module (stock image); point CADDY_ADMIN_URL at a belune-caddy")
			}
			require.NoError(t, err)
		}
	}

	addr := os.Getenv("CADDY_HTTP_ADDR")
	if addr == "" {
		addr = "localhost:80"
	}
	hit := func(host string) *http.Response {
		t.Helper()
		req, err := http.NewRequest(http.MethodGet, "http://"+addr+"/", nil)
		require.NoError(t, err)
		req.Host = host
		resp, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
		require.NoError(t, err, "no reply at all is what max_events=0 looks like")
		resp.Body.Close()
		return resp
	}

	// max_events 2: the upstream is dead, so allowed requests are 502s.
	assert.NotEqual(t, http.StatusTooManyRequests, hit(hostA).StatusCode)
	assert.NotEqual(t, http.StatusTooManyRequests, hit(hostA).StatusCode)
	limited := hit(hostA)
	assert.Equal(t, http.StatusTooManyRequests, limited.StatusCode, "the third request in the window must be refused")
	assert.NotEmpty(t, limited.Header.Get("Retry-After"))

	// Negative control: a second domain with the same limit has its own budget.
	// Sharing a zone name would have refused this on arrival.
	assert.NotEqual(t, http.StatusTooManyRequests, hit(hostB).StatusCode,
		"domain B was refused after only domain A's requests: the two share a counter")
}
