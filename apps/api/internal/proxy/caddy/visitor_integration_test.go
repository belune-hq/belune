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

	"github.com/belune-hq/belune/internal/proxy"
)

// Real-Caddy proof of the two features that used to see the CDN instead of the
// visitor: a domain's rate limit and its IP allowlist. A mock cannot prove either,
// because what matters is how the module resolves {http.vars.client_ip} and how
// Caddy's client_ip matcher reads X-Forwarded-For — and a placeholder that quietly
// resolved to empty would put every client in ONE bucket, which looks exactly like
// the limit working for a single client.
//
// Same environment as TestRealCaddy_RateLimitIsEnforcedPerDomain: a belune-caddy,
// CADDY_ADMIN_URL, and CADDY_HTTP_ADDR mapped to the container's port 80.

// trustThePeers lists the networks a test client can arrive from as trusted
// proxies — what an operator does for a CDN — and undoes it afterwards.
func trustThePeers(t *testing.T, c *Client) {
	t.Helper()
	ctx := context.Background()
	put := func(key string, v any) {
		path := serverPath + "/" + key
		if err := c.sendJSON(ctx, http.MethodPut, path, v); err != nil {
			// PUT only creates; an earlier run's leftover is replaced.
			require.NoError(t, c.sendJSON(ctx, http.MethodPatch, path, v))
		}
	}
	// Strict first: ranges without it read X-Forwarded-For left to right, which
	// lets a visitor choose their own address.
	put("trusted_proxies_strict", 1)
	put("trusted_proxies", map[string]any{
		"source": "static",
		"ranges": []string{"127.0.0.0/8", "10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16"},
	})
	t.Cleanup(func() {
		_ = c.sendJSON(ctx, http.MethodDelete, serverPath+"/trusted_proxies", nil)
		_ = c.sendJSON(ctx, http.MethodDelete, serverPath+"/trusted_proxies_strict", nil)
	})
}

func visitorHit(t *testing.T, host, xff string) int {
	t.Helper()
	addr := os.Getenv("CADDY_HTTP_ADDR")
	if addr == "" {
		addr = "localhost:80"
	}
	req, err := http.NewRequest(http.MethodGet, "http://"+addr+"/", nil)
	require.NoError(t, err)
	req.Host = host
	if xff != "" {
		req.Header.Set("X-Forwarded-For", xff)
	}
	resp, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
	require.NoError(t, err)
	_ = resp.Body.Close()
	return resp.StatusCode
}

func addOrSkip(t *testing.T, c *Client, cfg proxy.RouteConfig) {
	t.Helper()
	cfg.SSLMode = proxy.SSLModeOff
	if err := c.AddRoute(context.Background(), cfg); err != nil {
		if strings.Contains(err.Error(), "unknown module: http.handlers.rate_limit") {
			t.Skip("this Caddy has no rate_limit module (stock image); point CADDY_ADMIN_URL at a belune-caddy")
		}
		require.NoError(t, err)
	}
}

func TestRealCaddy_RateLimitKeysOnTheVisitorBehindATrustedProxy(t *testing.T) {
	c := liveClient(t)
	ctx := context.Background()
	c.InitCatchAll(ctx)
	const host = "ratelimit-visitor.belune.local"
	t.Cleanup(func() { _ = c.RemoveRoute(ctx, host, "/") })
	addOrSkip(t, c, rateLimitedRoute(host, "/")) // max_events 2

	// Without a trusted proxy the key is the peer, so a client that sends a
	// different X-Forwarded-For each time is still ONE client and is limited. This
	// is also the "it changes nothing until a CDN is listed" half of the claim.
	for i, v := range []string{"198.51.100.1", "198.51.100.2"} {
		assert.NotEqual(t, http.StatusTooManyRequests, visitorHit(t, host, v), "request %d", i)
	}
	assert.Equal(t, http.StatusTooManyRequests, visitorHit(t, host, "198.51.100.3"),
		"with no proxy trusted, a forged X-Forwarded-For must not buy a fresh bucket")

	// With the CDN trusted, distinct visitors have their own buckets. Re-adding the
	// route starts its counter empty.
	require.NoError(t, c.RemoveRoute(ctx, host, "/"))
	addOrSkip(t, c, rateLimitedRoute(host, "/"))
	trustThePeers(t, c)

	for i, v := range []string{"203.0.113.1", "203.0.113.2", "203.0.113.3", "203.0.113.4"} {
		assert.NotEqual(t, http.StatusTooManyRequests, visitorHit(t, host, v),
			"visitor %d shared a bucket with the others: the key collapsed to one value (%s)", i, v)
	}

	// …and one visitor is still limited, which a key that resolved to a distinct
	// value per request (an unset placeholder) would not do.
	one := "203.0.113.50"
	assert.NotEqual(t, http.StatusTooManyRequests, visitorHit(t, host, one))
	assert.NotEqual(t, http.StatusTooManyRequests, visitorHit(t, host, one))
	assert.Equal(t, http.StatusTooManyRequests, visitorHit(t, host, one))
}

func TestRealCaddy_IPAllowlistMatchesTheVisitorAndCannotBeForged(t *testing.T) {
	c := liveClient(t)
	ctx := context.Background()
	c.InitCatchAll(ctx)
	const host = "allowlist-visitor.belune.local"
	t.Cleanup(func() { _ = c.RemoveRoute(ctx, host, "/") })
	addOrSkip(t, c, proxy.RouteConfig{
		Hostname: host, Path: "/", TargetURL: "http://127.0.0.1:9",
		Features: []proxy.RouteFeature{{
			Type: proxy.FeatureIPAllowlist, Enabled: true,
			Config: json.RawMessage(`{"ranges":["203.0.113.0/24"]}`),
		}},
	})

	// No proxy trusted: the visitor IS the peer, which is not in the range, and a
	// forged X-Forwarded-For claiming to be inside it must not change that.
	assert.Equal(t, http.StatusForbidden, visitorHit(t, host, "203.0.113.7"),
		"a forged X-Forwarded-For got past the allowlist")

	trustThePeers(t, c)
	assert.NotEqual(t, http.StatusForbidden, visitorHit(t, host, "203.0.113.7"),
		"an allowlisted visitor behind a trusted CDN was refused: the matcher still sees the CDN")
	assert.Equal(t, http.StatusForbidden, visitorHit(t, host, "198.51.100.7"))
	// A visitor who pre-seeds the header in front of the CDN's own entry.
	assert.Equal(t, http.StatusForbidden, visitorHit(t, host, "203.0.113.7, 198.51.100.7"),
		"the leftmost entry is the visitor's to write and must not be believed")
}
