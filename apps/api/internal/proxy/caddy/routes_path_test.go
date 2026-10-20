package caddy

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/belune-hq/belune/internal/proxy"
)

// A whole-host route must serialise exactly as it did before paths existed. If
// it does not, every route on every existing install looks drifted the moment
// this ships, and the reconciler rewrites all of them on its first pass.
func TestBuildRoute_RootPathIsUnchanged(t *testing.T) {
	route, err := buildRoute(proxy.RouteConfig{
		Hostname:  "app.example.com",
		Path:      "/",
		TargetURL: "http://app:3000",
	})
	require.NoError(t, err)

	assert.Equal(t, "route-app.example.com", route.ID, "root route id must not change")

	raw, err := json.Marshal(route.Match[0])
	require.NoError(t, err)
	assert.JSONEq(t, `{"host":["app.example.com"]}`, string(raw),
		"a whole-host route must carry no path matcher at all")
}

// The empty path is what a RouteConfig built by older code (or a test that omits
// the field) carries. It must mean "the whole host", never "a route that matches
// nothing" — the latter would be a silent 404 for a domain that looks fine.
func TestBuildRoute_EmptyPathMeansWholeHost(t *testing.T) {
	route, err := buildRoute(proxy.RouteConfig{Hostname: "a.com", TargetURL: "http://a:80"})
	require.NoError(t, err)
	assert.Empty(t, route.Match[0].Path)
	assert.Equal(t, "route-a.com", route.ID)
}

func TestBuildRoute_PathMatcherCoversPrefixAndExact(t *testing.T) {
	route, err := buildRoute(proxy.RouteConfig{
		Hostname:  "shop.com",
		Path:      "/api",
		TargetURL: "http://api:8080",
	})
	require.NoError(t, err)

	// Both entries are needed: "/api" alone misses "/api/users", and "/api/*"
	// alone misses "/api" itself, which would 404 the app's own root.
	assert.Equal(t, []string{"/api", "/api/*"}, route.Match[0].Path)
	assert.NotEqual(t, "route-shop.com", route.ID, "a path route must not take the root route's id")
}

// Two paths on one host must land on two different Caddy routes. Sharing an id
// would mean the second silently overwrote the first.
func TestRouteIDFor_DistinctPerPath(t *testing.T) {
	root := routeIDFor("shop.com", "/")
	api := routeIDFor("shop.com", "/api")
	v2 := routeIDFor("shop.com", "/api/v2")

	assert.Equal(t, "route-shop.com", root)
	assert.NotEqual(t, root, api)
	assert.NotEqual(t, api, v2)

	// The id is interpolated into the admin API URL "/id/<id>", so a literal
	// slash would read as another path segment and Caddy rejects it as traversal.
	for _, id := range []string{api, v2} {
		assert.NotContains(t, id, "/", "route id must be URL-path safe")
	}
}

// "/api/v2" and "/api-v2" slugify identically. Only the digest keeps them apart,
// and if it did not, one domain's route would quietly replace the other's.
func TestRouteIDFor_SlugCollisionsStayDistinct(t *testing.T) {
	assert.NotEqual(t, routeIDFor("shop.com", "/api/v2"), routeIDFor("shop.com", "/api-v2"))
}

func TestRouteIDFor_Deterministic(t *testing.T) {
	assert.Equal(t, routeIDFor("shop.com", "/api"), routeIDFor("shop.com", "/api"),
		"a non-deterministic id would make every reconcile pass look like drift")
}

// The strip handler must sit after the feature handlers: basic auth and IP
// allowlists are written against the public URL, so stripping first would make a
// rule guarding /api/admin stop matching.
func TestBuildRoute_StripRunsAfterFeaturesAndBeforeProxy(t *testing.T) {
	route, err := buildRoute(proxy.RouteConfig{
		Hostname:  "shop.com",
		Path:      "/api",
		StripPath: true,
		TargetURL: "http://api:8080",
		Features: []proxy.RouteFeature{{
			Type:    "basic_auth",
			Enabled: true,
			Config:  json.RawMessage(`{"username":"admin","hashed_password":"$2a$14$abcdefghijklmnopqrstuv"}`),
		}},
	})
	require.NoError(t, err)

	var order []string
	for _, h := range route.Handle {
		order = append(order, h["handler"].(string))
	}
	require.Equal(t, []string{"authentication", "rewrite", "reverse_proxy"}, order)

	rewrite := route.Handle[1]
	assert.Equal(t, "/api", rewrite["strip_path_prefix"])
}

func TestBuildRoute_NoStripWhenDisabled(t *testing.T) {
	route, err := buildRoute(proxy.RouteConfig{
		Hostname:  "shop.com",
		Path:      "/api",
		StripPath: false,
		TargetURL: "http://api:8080",
	})
	require.NoError(t, err)
	for _, h := range route.Handle {
		assert.NotEqual(t, "rewrite", h["handler"], "strip must not appear when strip_path is off")
	}
}

// Stripping "/" would rewrite every request on a whole-host route to the empty
// path, which is not what the operator asked for and breaks the app's root.
func TestBuildRoute_StripIgnoredOnRootPath(t *testing.T) {
	route, err := buildRoute(proxy.RouteConfig{
		Hostname:  "a.com",
		Path:      "/",
		StripPath: true,
		TargetURL: "http://a:80",
	})
	require.NoError(t, err)
	for _, h := range route.Handle {
		assert.NotEqual(t, "rewrite", h["handler"])
	}
}

// ListRoutes reads routes back to diff them against the database. If a path
// route round-trips as a root route, the reconciler sees drift that is not there
// and rewrites the route on every single pass, for ever.
func TestPathMatcherRoundTrip(t *testing.T) {
	for _, path := range []string{"/", "/api", "/api/v2"} {
		got := pathFromMatcher(pathMatcher(path))
		assert.Equal(t, path, got, "round trip must be exact for %q", path)
	}
}

// Strip then prepend, in that order. The composed result is what the container
// receives, and getting the order backwards would prepend to a path that still
// carries the public prefix.
func TestBuildRoute_StripThenInternalPath(t *testing.T) {
	route, err := buildRoute(proxy.RouteConfig{
		Hostname:     "shop.com",
		Path:         "/public",
		StripPath:    true,
		InternalPath: "/app/v2",
		TargetURL:    "http://api:8080",
	})
	require.NoError(t, err)

	var order []string
	for _, h := range route.Handle {
		order = append(order, h["handler"].(string))
	}
	require.Equal(t, []string{"rewrite", "rewrite", "reverse_proxy"}, order)

	assert.Equal(t, "/public", route.Handle[0]["strip_path_prefix"])
	// {http.request.uri}, not {http.request.uri.path}: the former carries the
	// query string, and rewriting to the bare path would drop ?page=2 from every
	// request the app ever sees.
	assert.Equal(t, "/app/v2{http.request.uri}", route.Handle[1]["uri"])
}

// The internal path stands alone: an app that insists on serving under /grafana
// while the operator publishes it at the root of a host.
func TestBuildRoute_InternalPathWithoutStrip(t *testing.T) {
	route, err := buildRoute(proxy.RouteConfig{
		Hostname:     "metrics.example.com",
		Path:         "/",
		InternalPath: "/grafana",
		TargetURL:    "http://grafana:3000",
	})
	require.NoError(t, err)

	var order []string
	for _, h := range route.Handle {
		order = append(order, h["handler"].(string))
	}
	require.Equal(t, []string{"rewrite", "reverse_proxy"}, order)
	assert.Equal(t, "/grafana{http.request.uri}", route.Handle[0]["uri"])
}

// Empty means prepend nothing, and must not emit a rewrite at all — an empty
// prefix would rewrite every request to itself and cost a handler for nothing.
func TestBuildRoute_NoInternalPathEmitsNoRewrite(t *testing.T) {
	route, err := buildRoute(proxy.RouteConfig{
		Hostname:  "a.com",
		Path:      "/",
		TargetURL: "http://a:80",
	})
	require.NoError(t, err)
	for _, h := range route.Handle {
		assert.NotEqual(t, "rewrite", h["handler"])
	}
}
