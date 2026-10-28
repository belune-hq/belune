package caddy

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/belune-hq/belune/internal/proxy"
)

// setsClientIP reports whether a route's reverse proxy SETs the client-address
// header to Caddy's own answer, which is the only thing the API believes.
func setsClientIP(t *testing.T, raw json.RawMessage) bool {
	t.Helper()
	var r struct {
		Handle []struct {
			Handler string `json:"handler"`
			Headers struct {
				Request struct {
					Set map[string][]string `json:"set"`
				} `json:"request"`
			} `json:"headers"`
		} `json:"handle"`
	}
	require.NoError(t, json.Unmarshal(raw, &r))
	for _, h := range r.Handle {
		if h.Handler == "reverse_proxy" {
			v := h.Headers.Request.Set[proxy.ClientIPHeader]
			return len(v) == 1 && v[0] == "{http.vars.client_ip}"
		}
	}
	return false
}

func TestDashboardProxyHandle_SetsTheHeaderFromCaddysOwnClientAddress(t *testing.T) {
	b, err := json.Marshal(caddyRoute{ID: "x", Handle: []caddyHandle{newDashboardProxyHandle("belune:8080")}})
	require.NoError(t, err)
	assert.True(t, setsClientIP(t, b))
	// Pinned because the plausible-looking {http.request.client_ip} is not a
	// placeholder: Caddy sends it to the upstream as literal text.
	assert.NotContains(t, string(b), "http.request.client_ip")
}

func TestEveryDashboardPathSetsTheClientIPHeader(t *testing.T) {
	ctx := context.Background()

	t.Run("the dashboard's own hostname", func(t *testing.T) {
		c, a := newAdminStub(t, `{"@id":"route-catch-all","handle":[{"handler":"reverse_proxy"}]}`)
		require.NoError(t, c.SetDashboardRoute(ctx, "belune.example.com", proxy.SSLModeOff))
		r, idx := a.route(dashboardRouteID)
		require.GreaterOrEqual(t, idx, 0)
		assert.True(t, setsClientIP(t, r))
	})

	t.Run("the catch-all written at startup", func(t *testing.T) {
		c, a := newAdminStub(t, `{"handle":[{"handler":"reverse_proxy"}]}`)
		c.InitCatchAll(ctx)
		r, idx := a.route(catchAllRouteID)
		require.GreaterOrEqual(t, idx, 0)
		assert.True(t, setsClientIP(t, r))
	})

	t.Run("the catch-all synthesised by a reorder", func(t *testing.T) {
		c, a := newAdminStub(t, `{"@id":"route-app.example.com","match":[{"host":["app.example.com"]}],"handle":[{"handler":"reverse_proxy"}]}`)
		require.NoError(t, c.reorderRoutes(ctx))
		r, idx := a.route(catchAllRouteID)
		require.GreaterOrEqual(t, idx, 0)
		assert.True(t, setsClientIP(t, r))
	})
}

// A dashboard route written by a version that did not set the header has the right
// hostname, mode and upstream, so every other comparison calls it correct. If the
// header were not read back too, an upgrade would never rewrite it.
func TestSetDashboardRoute_RewritesARouteThatPredatesTheHeader(t *testing.T) {
	ctx := context.Background()
	old := `{"@id":"route-dashboard","match":[{"host":["belune.example.com"]}],"handle":[` +
		`{"handler":"reverse_proxy","upstreams":[{"dial":"belune:8080"}]}]}`
	c, a := newAdminStub(t, old, `{"@id":"route-catch-all","handle":[{"handler":"reverse_proxy"}]}`)

	require.NoError(t, c.SetDashboardRoute(ctx, "belune.example.com", proxy.SSLModeOff))
	assert.NotEmpty(t, a.writes(), "the old route was called correct and left as it was")
	r, _ := a.route(dashboardRouteID)
	assert.True(t, setsClientIP(t, r))

	// And once it is current, a pass is a read: a write is a config reload.
	a.calls = nil
	require.NoError(t, c.SetDashboardRoute(ctx, "belune.example.com", proxy.SSLModeOff))
	assert.Empty(t, a.writes())
}
