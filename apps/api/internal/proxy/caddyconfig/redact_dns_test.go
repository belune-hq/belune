package caddyconfig

import (
	"strings"
	"testing"

	"github.com/belune-hq/belune/internal/proxy"
)

// identifiers and settings are credential fields that are NOT secrets and so are
// allowed to show in the Proxy page's config view. Everything else a provider
// takes must be redacted.
var nonSecretDNSFields = map[string]bool{
	"access_key_id":  true, // an AWS identifier, not a secret
	"region":         true,
	"hosted_zone_id": true,
}

// The failure this guards is silent: redaction fires on key NAMES, so a provider
// field it has never heard of renders in plain text in a browser, and nothing
// else notices. The field names come from proxy.DNSProviders, which are the
// modules' own JSON keys.
func TestRedactCoversEveryDNSProviderSecret(t *testing.T) {
	for name, spec := range proxy.DNSProviders {
		t.Run(name, func(t *testing.T) {
			creds := map[string]string{}
			for _, k := range append(append([]string{}, spec.Required...), spec.Optional...) {
				creds[k] = "SECRET-" + k
			}
			policy := proxy.DNSPolicy{ProviderID: "p1", Provider: name, Subjects: []string{"*.example.com"}, Credentials: creds}
			cfg := map[string]any{"apps": map[string]any{"tls": map[string]any{
				"automation": map[string]any{"policies": []any{policy.CaddyPolicy()}},
			}}}

			redacted, n, err := Redact([]byte(marshal(t, cfg)))
			if err != nil {
				t.Fatal(err)
			}
			out := marshal(t, redacted)

			wantRedacted := 0
			for k := range creds {
				if nonSecretDNSFields[k] {
					if !strings.Contains(out, "SECRET-"+k) {
						t.Errorf("%s is documented as non-secret but was redacted", k)
					}
					continue
				}
				wantRedacted++
				if strings.Contains(out, "SECRET-"+k) {
					t.Errorf("%s leaked into the rendered config: %s", k, out)
				}
			}
			if n != wantRedacted {
				t.Errorf("redacted %d values, want %d", n, wantRedacted)
			}
			if !strings.Contains(out, "*.example.com") {
				t.Errorf("subjects must stay visible: %s", out)
			}
		})
	}
}
