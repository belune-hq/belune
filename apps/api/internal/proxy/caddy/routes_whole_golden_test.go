package caddy

import (
	"testing"

	"github.com/belune-hq/belune/internal/proxy"
)

// TestBuildRoute_Golden pins the WHOLE route buildRoute emits -- match, handler
// chain and the reverse_proxy upstreams -- not just the feature handlers.
//
// It exists because the route table is where this product's worst defects have
// landed, and a change to how a route is ADDRESSED (the upstream list) must be
// provably invisible on the wire. The goldens were generated from the code before
// that change and must not move with it.
func TestBuildRoute_Golden(t *testing.T) {
	cases := []struct {
		name string
		cfg  proxy.RouteConfig
	}{
		{"route_minimal", proxy.RouteConfig{Hostname: "app.example.com", SSLMode: proxy.SSLModeAutomatic}},
		{"route_health_check_path", proxy.RouteConfig{Hostname: "app.example.com", SSLMode: proxy.SSLModeAutomatic, HealthCheckPath: "/healthz"}},
		{"route_path_strip_internal", proxy.RouteConfig{
			Hostname: "app.example.com", Path: "/api", StripPath: true, InternalPath: "/v2", SSLMode: proxy.SSLModeAutomatic,
		}},
		{"route_force_https", proxy.RouteConfig{Hostname: "app.example.com", ForceHTTPS: true, TLS: true, SSLMode: proxy.SSLModeAutomatic}},
		{"route_ssl_off", proxy.RouteConfig{Hostname: "plain.example.com", SSLMode: proxy.SSLModeOff}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := tc.cfg
			cfg.TargetURL = "http://proj-app-1234abcd:8080"
			got, err := buildRoute(cfg)
			if err != nil {
				t.Fatalf("buildRoute: %v", err)
			}
			checkGolden(t, tc.name, got)
		})
	}
}
