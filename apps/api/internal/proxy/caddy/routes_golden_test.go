package caddy

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"testing"

	"github.com/belune-hq/belune/internal/proxy"
)

var updateGolden = flag.Bool("update", false, "update golden files under testdata/")

// TestBuildFeatureHandlers_Golden pins the JSON shape emitted by
// buildFeatureHandlers for each feature combination. Schema regressions
// (e.g. a Caddy module renaming) show up as golden diffs instead of
// runtime-only failures.
func TestBuildFeatureHandlers_Golden(t *testing.T) {
	cases := []struct {
		name     string
		features []proxy.RouteFeature
		advanced json.RawMessage
	}{
		{
			name: "basic_auth",
			features: []proxy.RouteFeature{{
				Type:    proxy.FeatureBasicAuth,
				Enabled: true,
				Config:  json.RawMessage(`{"username":"alice","hashed_password":"$2a$10$abcdef"}`),
			}},
		},
		{
			name: "headers_response_only",
			features: []proxy.RouteFeature{{
				Type:    proxy.FeatureHeaders,
				Enabled: true,
				Config:  json.RawMessage(`{"response":{"set":{"X-Frame-Options":["DENY"]}}}`),
			}},
		},
		{
			name: "headers_request_and_response",
			features: []proxy.RouteFeature{{
				Type:    proxy.FeatureHeaders,
				Enabled: true,
				Config: json.RawMessage(`{
					"request":{"set":{"X-Forwarded-Proto":["https"]}},
					"response":{"set":{"Strict-Transport-Security":["max-age=31536000"]},"delete":["Server"]}
				}`),
			}},
		},
		{
			name: "ip_allowlist",
			features: []proxy.RouteFeature{{
				Type:    proxy.FeatureIPAllowlist,
				Enabled: true,
				Config:  json.RawMessage(`{"ranges":["10.0.0.0/8","192.168.1.0/24"]}`),
			}},
		},
		{
			name: "redirect_default_status",
			features: []proxy.RouteFeature{{
				Type:    proxy.FeatureRedirect,
				Enabled: true,
				Config:  json.RawMessage(`{"from":"/old","to":"/new"}`),
			}},
		},
		{
			name: "redirect_302",
			features: []proxy.RouteFeature{{
				Type:    proxy.FeatureRedirect,
				Enabled: true,
				Config:  json.RawMessage(`{"from":"/temp","to":"https://example.com/moved","status_code":302}`),
			}},
		},
		{
			name: "combined_auth_and_headers",
			features: []proxy.RouteFeature{
				{
					Type:    proxy.FeatureBasicAuth,
					Enabled: true,
					Config:  json.RawMessage(`{"username":"admin","hashed_password":"$2a$10$xyz"}`),
				},
				{
					Type:    proxy.FeatureHeaders,
					Enabled: true,
					Config:  json.RawMessage(`{"response":{"set":{"X-Robots-Tag":["noindex"]}}}`),
				},
			},
		},
		{
			name: "disabled_feature_skipped",
			features: []proxy.RouteFeature{
				{
					Type:    proxy.FeatureBasicAuth,
					Enabled: false,
					Config:  json.RawMessage(`{"username":"alice","hashed_password":"x"}`),
				},
				{
					Type:    proxy.FeatureHeaders,
					Enabled: true,
					Config:  json.RawMessage(`{"response":{"set":{"X-Test":["yes"]}}}`),
				},
			},
		},
		{
			name:     "advanced_config_allowed",
			advanced: json.RawMessage(`[{"handler":"rewrite","uri":"/api{http.request.uri}"}]`),
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := proxy.RouteConfig{
				Hostname:       "example.com",
				TargetURL:      "http://upstream:8080",
				Features:       tc.features,
				AdvancedConfig: tc.advanced,
			}
			got, err := buildFeatureHandlers(cfg)
			if err != nil {
				t.Fatalf("buildFeatureHandlers: %v", err)
			}
			checkGolden(t, tc.name, got)
		})
	}
}

// TestBuildFeatureHandlers_InvalidFeature confirms that an invalid config is
// dropped silently (logged) rather than rendered as broken JSON.
func TestBuildFeatureHandlers_InvalidFeature(t *testing.T) {
	cfg := proxy.RouteConfig{
		Features: []proxy.RouteFeature{{
			Type:    proxy.FeatureBasicAuth,
			Enabled: true,
			// Missing hashed_password — ParseFeatureConfig rejects it.
			Config: json.RawMessage(`{"username":"alice"}`),
		}},
	}
	got, err := buildFeatureHandlers(cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("expected 0 handlers from invalid feature, got %d", len(got))
	}
}

// TestBuildFeatureHandlers_AdvancedConfigRejected ensures a disallowed handler
// in AdvancedConfig produces a hard error with a listing of every bad entry.
func TestBuildFeatureHandlers_AdvancedConfigRejected(t *testing.T) {
	cfg := proxy.RouteConfig{
		AdvancedConfig: json.RawMessage(`[
			{"handler":"reverse_proxy","upstreams":[{"dial":"evil:80"}]},
			{"handler":"exec","command":"rm"}
		]`),
	}
	_, err := buildFeatureHandlers(cfg)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	msg := err.Error()
	for _, want := range []string{"reverse_proxy", "exec"} {
		if !contains(msg, want) {
			t.Fatalf("error %q missing disallowed handler %q", msg, want)
		}
	}
}

// checkGolden compares got (marshalled to pretty JSON) against testdata/<name>.golden.json.
// Run `go test -update ./internal/proxy/caddy` to regenerate.
func checkGolden(t *testing.T, name string, got any) {
	t.Helper()
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetIndent("", "  ")
	if err := enc.Encode(got); err != nil {
		t.Fatalf("encode: %v", err)
	}
	path := filepath.Join("testdata", name+".golden.json")

	if *updateGolden {
		if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
			t.Fatalf("write golden: %v", err)
		}
		return
	}

	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden %s: %v (run `go test -update` to create)", path, err)
	}
	if !bytes.Equal(bytes.TrimSpace(want), bytes.TrimSpace(buf.Bytes())) {
		t.Fatalf("golden mismatch for %s\n-- want --\n%s\n-- got --\n%s", name, want, buf.String())
	}
}

func contains(s, sub string) bool {
	return len(sub) == 0 || (len(s) >= len(sub) && indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	n := len(s) - len(sub)
	for i := 0; i <= n; i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
