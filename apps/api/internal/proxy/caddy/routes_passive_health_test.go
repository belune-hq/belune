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
