package caddy

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/belune-hq/belune/internal/proxy"
)

// routeOrder renders the sorted routes as "host path" strings, in order.
func routeOrder(t *testing.T, cfgs []proxy.RouteConfig) []string {
	t.Helper()

	raws := make([]json.RawMessage, 0, len(cfgs))
	for _, cfg := range cfgs {
		route, err := buildRoute(cfg)
		require.NoError(t, err)
		raw, err := json.Marshal(route)
		require.NoError(t, err)
		raws = append(raws, raw)
	}

	sortRoutesBySpecificity(raws)

	out := make([]string, 0, len(raws))
	for _, raw := range raws {
		var probe struct {
			Match []caddyMatch `json:"match"`
		}
		require.NoError(t, json.Unmarshal(raw, &probe))
		out = append(out, probe.Match[0].Host[0]+" "+pathFromMatcher(probe.Match[0].Path))
	}
	return out
}

// The whole point of phase 3. Caddy is first-match-wins, so a whole-host route
// listed before a sibling /api route swallows every request meant for the API —
// and creation order is just "whichever domain the operator added first".
func TestSortRoutes_RootNeverShadowsPath(t *testing.T) {
	// Deliberately created worst-case: root first, exactly as an operator who
	// added their frontend before their API would produce.
	got := routeOrder(t, []proxy.RouteConfig{
		{Hostname: "shop.com", Path: "/", TargetURL: "http://web:3000"},
		{Hostname: "shop.com", Path: "/api", TargetURL: "http://api:8080"},
	})

	assert.Equal(t, []string{"shop.com /api", "shop.com /"}, got)
}

func TestSortRoutes_DeeperPrefixFirst(t *testing.T) {
	got := routeOrder(t, []proxy.RouteConfig{
		{Hostname: "shop.com", Path: "/", TargetURL: "http://web:3000"},
		{Hostname: "shop.com", Path: "/api", TargetURL: "http://api:8080"},
		{Hostname: "shop.com", Path: "/api/v2", TargetURL: "http://api2:8080"},
	})

	assert.Equal(t, []string{"shop.com /api/v2", "shop.com /api", "shop.com /"}, got)
}

// Depth is segment count, not string length. "/ab" is longer than "/a/b" as a
// string but less specific as a prefix, and sorting on length would put it first
// — which is only wrong when it matters.
func TestSortRoutes_DepthIsSegmentsNotLength(t *testing.T) {
	got := routeOrder(t, []proxy.RouteConfig{
		{Hostname: "shop.com", Path: "/abcdefgh", TargetURL: "http://a:80"},
		{Hostname: "shop.com", Path: "/a/b", TargetURL: "http://b:80"},
	})

	assert.Equal(t, []string{"shop.com /a/b", "shop.com /abcdefgh"}, got)
}

// A reconcile pass that reshuffled equal-ranked routes would rewrite Caddy's
// config every time and report drift for ever.
func TestSortRoutes_IsDeterministic(t *testing.T) {
	cfgs := []proxy.RouteConfig{
		{Hostname: "b.com", Path: "/api", TargetURL: "http://x:80"},
		{Hostname: "a.com", Path: "/api", TargetURL: "http://y:80"},
		{Hostname: "c.com", Path: "/", TargetURL: "http://z:80"},
		{Hostname: "a.com", Path: "/", TargetURL: "http://w:80"},
	}

	first := routeOrder(t, cfgs)
	for i := 0; i < 5; i++ {
		assert.Equal(t, first, routeOrder(t, cfgs), "ordering must not vary between passes")
	}

	// Ties broken by host, so the result is a total order rather than "sorted
	// enough" with fetch order deciding the rest.
	assert.Equal(t, []string{"a.com /api", "b.com /api", "a.com /", "c.com /"}, first)
}
