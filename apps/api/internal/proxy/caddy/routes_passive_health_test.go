package caddy

import (
	"testing"

	"github.com/belune-hq/belune/internal/proxy"
)

// reverseProxyHealth returns the reverse_proxy handler's health_checks of a built route.
func reverseProxyHealth(t *testing.T, cfg proxy.RouteConfig) (health map[string]any, present bool) {
	t.Helper()
	route, err := buildRoute(cfg)
	if err != nil {
		t.Fatalf("buildRoute: %v", err)
	}
	for _, h := range route.Handle {
		if h["handler"] != "reverse_proxy" {
			continue
		}
		hc, ok := h["health_checks"]
		if !ok {
			return nil, false
		}
		return hc.(map[string]any), true
	}
	t.Fatal("no reverse_proxy handler")
	return nil, false
}

func twoReplicas() proxy.RouteConfig {
	return proxy.RouteConfig{
		Hostname: "app.example.com", SSLMode: proxy.SSLModeAutomatic,
		Upstreams: []string{"http://app-web-1:8080", "http://app-web-2:8080"},
	}
}

// A route with several upstreams gets a PASSIVE check with no health path at all: that is
// the case this exists for, since applications are created with none.
func TestReverseProxy_SeveralUpstreamsGetAPassiveCheckWithNoHealthPath(t *testing.T) {
	health, ok := reverseProxyHealth(t, twoReplicas())
	if !ok {
		t.Fatal("a multi-upstream route must carry a health_checks block")
	}
	passive, ok := health["passive"].(map[string]any)
	if !ok {
		t.Fatalf("no passive check: %v", health)
	}
	if passive["fail_duration"] != "10s" || passive["max_fails"] != 3 {
		t.Fatalf("passive settings drifted: %v", passive)
	}
	if _, has := health["active"]; has {
		t.Fatalf("no health path was set, so there must be no active probe: %v", health)
	}
}

// ⛔ unhealthy_status is deliberately NOT set. A dead container is ejected on
// fail_duration + max_fails alone (a dial failure counts), and setting it would also eject
// a replica because the APPLICATION answered 502/503 (maintenance mode), which is not "this
// replica is dead".
func TestReverseProxy_PassiveNeverSetsUnhealthyStatus(t *testing.T) {
	health, _ := reverseProxyHealth(t, twoReplicas())
	if _, has := health["passive"].(map[string]any)["unhealthy_status"]; has {
		t.Fatal("unhealthy_status must not be set: an app's own 503 must not remove it from rotation")
	}
}

// Active and passive are complementary: a health path adds the active probe and keeps the
// passive check.
func TestReverseProxy_ActiveAndPassiveCoexist(t *testing.T) {
	cfg := twoReplicas()
	cfg.HealthCheckPath = "/healthz"
	health, _ := reverseProxyHealth(t, cfg)
	if _, ok := health["active"]; !ok {
		t.Fatalf("a health path must still configure the active check: %v", health)
	}
	if _, ok := health["passive"]; !ok {
		t.Fatalf("and the passive check stays: %v", health)
	}
}

// ⛔ Not for a single upstream. With nothing to fail over to, ejecting it turns a blip into
// a 503 that outlasts the recovery (measured: the container was back in seconds and the route
// answered 503 for the whole fail_duration), which would be a regression for every
// single-replica application.
func TestReverseProxy_ASingleUpstreamHasNoPassiveCheck(t *testing.T) {
	cfg := proxy.RouteConfig{Hostname: "app.example.com", SSLMode: proxy.SSLModeAutomatic, Upstreams: []string{"http://app:8080"}}
	if health, ok := reverseProxyHealth(t, cfg); ok {
		t.Fatalf("a single-upstream route must carry no health_checks without a path: %v", health)
	}
	cfg.HealthCheckPath = "/healthz"
	health, _ := reverseProxyHealth(t, cfg)
	if _, has := health["passive"]; has {
		t.Fatalf("a single upstream with a path gets the active probe only: %v", health)
	}
}

// reverseProxyHandle returns the reverse_proxy handler of a built route.
func reverseProxyHandle(t *testing.T, cfg proxy.RouteConfig) caddyHandle {
	t.Helper()
	route, err := buildRoute(cfg)
	if err != nil {
		t.Fatalf("buildRoute: %v", err)
	}
	for _, h := range route.Handle {
		if h["handler"] == "reverse_proxy" {
			return h
		}
	}
	t.Fatal("no reverse_proxy handler")
	return nil
}

// A route with several upstreams retries a failed dial for a short window, so a request
// that lands on a dead replica is served by a live one instead of failing.
func TestReverseProxy_SeveralUpstreamsRetryAFailedDial(t *testing.T) {
	h := reverseProxyHandle(t, twoReplicas())
	lb, ok := h["load_balancing"].(map[string]any)
	if !ok {
		t.Fatalf("no load_balancing block: %v", h)
	}
	if lb["try_duration"] != "2s" || lb["try_interval"] != "250ms" {
		t.Fatalf("retry window drifted: %v", lb)
	}
}

// ⛔ Not for a single upstream. Measured: for an app that takes 6s to boot a 2s window rescued
// 41 of 128 failed requests and turned the other 87 from instant failures into failures 2s
// later, and every single-replica application would hold every request for the window
// whenever it is down. See the comment on lbTryDuration before widening this.
func TestReverseProxy_ASingleUpstreamDoesNotRetry(t *testing.T) {
	cfg := proxy.RouteConfig{Hostname: "app.example.com", SSLMode: proxy.SSLModeAutomatic, Upstreams: []string{"http://app:8080"}}
	if _, has := reverseProxyHandle(t, cfg)["load_balancing"]; has {
		t.Fatal("a single upstream must not carry load_balancing")
	}
}

// The window is the held-connection cost knob (requests/s x try_duration while upstreams are
// down), so it is pinned: widening it is a decision, not a drive-by.
func TestReverseProxy_TheRetryWindowIsShort(t *testing.T) {
	if lbTryDuration != "2s" {
		t.Fatalf("lbTryDuration changed to %s: held connections scale with it (about request-rate x this), see the comment", lbTryDuration)
	}
}

// The dashboard's route has one upstream, the control plane, and so no retry.
func TestReverseProxy_TheDashboardProxyDoesNotRetry(t *testing.T) {
	if _, has := newDashboardProxyHandle("belune:8080")["load_balancing"]; has {
		t.Fatal("the dashboard proxy must not carry load_balancing")
	}
}
