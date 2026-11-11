package caddy

import (
	"encoding/json"
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
		// upstreams overrides the single default one: the passive health check exists only
		// for a route with several.
		upstreams []string
	}{
		{"route_minimal", proxy.RouteConfig{Hostname: "app.example.com", SSLMode: proxy.SSLModeAutomatic}, nil},
		{"route_health_check_path", proxy.RouteConfig{Hostname: "app.example.com", SSLMode: proxy.SSLModeAutomatic, HealthCheckPath: "/healthz"}, nil},
		{"route_path_strip_internal", proxy.RouteConfig{
			Hostname: "app.example.com", Path: "/api", StripPath: true, InternalPath: "/v2", SSLMode: proxy.SSLModeAutomatic,
		}, nil},
		{"route_force_https", proxy.RouteConfig{Hostname: "app.example.com", ForceHTTPS: true, TLS: true, SSLMode: proxy.SSLModeAutomatic}, nil},
		{"route_ssl_off", proxy.RouteConfig{Hostname: "plain.example.com", SSLMode: proxy.SSLModeOff}, nil},
		// Replicas: passive only (the default, since an application is created with no
		// health path), and active + passive together.
		{"route_two_upstreams", proxy.RouteConfig{Hostname: "app.example.com", SSLMode: proxy.SSLModeAutomatic},
			[]string{"http://proj-app-1234abcd-web-1:8080", "http://proj-app-1234abcd-web-2:8080"}},
		{"route_two_upstreams_health_check_path", proxy.RouteConfig{Hostname: "app.example.com", SSLMode: proxy.SSLModeAutomatic, HealthCheckPath: "/healthz"},
			[]string{"http://proj-app-1234abcd-web-1:8080", "http://proj-app-1234abcd-web-2:8080"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := tc.cfg
			cfg.Upstreams = []string{"http://proj-app-1234abcd:8080"}
			if tc.upstreams != nil {
				cfg.Upstreams = tc.upstreams
			}
			got, err := buildRoute(cfg)
			if err != nil {
				t.Fatalf("buildRoute: %v", err)
			}
			checkGolden(t, tc.name, got)
		})
	}
}

// A route with several upstreams renders one dial per upstream, in the order given,
// and renders the same bytes every time. Vacuous at one replica today; it is the C2
// guard: the desired route is compared against Caddy's as JSON, so an unstable
// order would make every route look drifted on every pass.
func TestBuildRoute_UpstreamsKeepTheirOrderAndAreByteStable(t *testing.T) {
	cfg := proxy.RouteConfig{
		Hostname: "app.example.com", SSLMode: proxy.SSLModeAutomatic,
		Upstreams: []string{"http://app-1:8080", "http://app-2:8080", "http://app-3:8080"},
	}
	first, err := buildRoute(cfg)
	if err != nil {
		t.Fatalf("buildRoute: %v", err)
	}
	want, _ := json.Marshal(first)

	for i := 0; i < 50; i++ {
		again, err := buildRoute(cfg)
		if err != nil {
			t.Fatalf("buildRoute: %v", err)
		}
		got, _ := json.Marshal(again)
		if string(got) != string(want) {
			t.Fatalf("pass %d rendered different bytes:\n%s\n%s", i, want, got)
		}
	}

	var dials []string
	for _, h := range first.Handle {
		if h["handler"] != "reverse_proxy" {
			continue
		}
		for _, u := range h["upstreams"].([]caddyUpstream) {
			dials = append(dials, u.Dial)
		}
	}
	if len(dials) != 3 || dials[0] != "app-1:8080" || dials[1] != "app-2:8080" || dials[2] != "app-3:8080" {
		t.Fatalf("upstreams out of order or missing: %v", dials)
	}
}

// A route with nothing to proxy to is refused, not written. Caddy accepts
// "upstreams": [] and the route would 502 every request while the reconciler saw it
// as correct; the single empty target this replaced was rejected by Caddy, so the
// loudness is kept here.
func TestBuildRoute_RefusesAZeroUpstreamRoute(t *testing.T) {
	for _, ups := range [][]string{nil, {}} {
		if _, err := buildRoute(proxy.RouteConfig{Hostname: "app.example.com", Upstreams: ups}); err == nil {
			t.Fatalf("buildRoute accepted a route with upstreams %#v", ups)
		}
	}
}
