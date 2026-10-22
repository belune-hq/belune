package proxy

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// DNSProviderSpec describes one DNS provider's Caddy module and the credential
// fields it takes. The field names are the module's JSON keys, taken from the
// libdns provider structs the caddy-dns modules embed — not guessed, because a
// key Caddy does not know is rejected for the whole policy write.
type DNSProviderSpec struct {
	// Name is both the provider label stored in dns_providers.provider and the
	// "name" key of the module in a policy's `provider` object.
	Name string
	// Module is the Caddy module ID, which the capability probe looks for.
	Module string
	// Required and Optional are credential keys. Anything else is refused.
	Required []string
	Optional []string
}

// DNSProviders is the closed set Belune can configure. It must stay in step with
// capability's dnsProviderModules and the dns_providers.provider CHECK.
var DNSProviders = map[string]DNSProviderSpec{
	"cloudflare": {
		Name: "cloudflare", Module: "dns.providers.cloudflare",
		Required: []string{"api_token"},
		// A Zone:Read token for setups whose api_token is scoped to one zone.
		Optional: []string{"zone_token"},
	},
	"digitalocean": {
		Name: "digitalocean", Module: "dns.providers.digitalocean",
		Required: []string{"auth_token"},
	},
	"route53": {
		Name: "route53", Module: "dns.providers.route53",
		Required: []string{"access_key_id", "secret_access_key"},
		Optional: []string{"session_token", "region", "hosted_zone_id"},
	},
}

// maxDNSCredentialLen bounds one credential value. Real tokens are well under
// 300 bytes; the cap only stops a pasted file from reaching the keyring.
const maxDNSCredentialLen = 2048

// Normalize trims and validates credential fields against the spec, returning
// the cleaned map. Unknown keys are an error rather than dropped: silently
// ignoring `apitoken` would store a provider that can never authenticate.
func (s DNSProviderSpec) Normalize(fields map[string]string) (map[string]string, error) {
	allowed := make(map[string]bool, len(s.Required)+len(s.Optional))
	for _, k := range s.Required {
		allowed[k] = true
	}
	for _, k := range s.Optional {
		allowed[k] = true
	}

	out := make(map[string]string, len(fields))
	for k, v := range fields {
		if !allowed[k] {
			return nil, fmt.Errorf("unknown credential field %q for %s", k, s.Name)
		}
		v = strings.TrimSpace(v)
		if v == "" {
			continue
		}
		if len(v) > maxDNSCredentialLen {
			return nil, fmt.Errorf("%s is too long", k)
		}
		// A newline in a token becomes a header injection or a broken request
		// the first time Caddy uses it, far from the form that took it.
		if strings.ContainsAny(v, "\r\n\x00") {
			return nil, fmt.Errorf("%s must not contain line breaks", k)
		}
		out[k] = v
	}
	for _, k := range s.Required {
		if out[k] == "" {
			return nil, fmt.Errorf("%s is required", k)
		}
	}
	return out, nil
}

// DNSPolicy is one Caddy TLS automation policy: the names that must be issued
// over DNS-01, and the provider credentials to do it with.
type DNSPolicy struct {
	// ProviderID is the dns_providers row; it names the policy so the reconciler
	// can tell its own policies from any others in the automation list.
	ProviderID string
	Provider   string
	Subjects   []string
	// Credentials are decrypted secrets. Never log a DNSPolicy with %v.
	Credentials map[string]string
}

// PolicyIDPrefix marks the automation policies Belune owns, via Caddy's "@id".
// Anything without it (local_certs' internal issuer in development) is left
// alone, so enabling DNS-01 cannot remove an unrelated issuer.
const PolicyIDPrefix = "belune-dns01-"

// CaddyPolicy renders the policy as Caddy JSON.
func (p DNSPolicy) CaddyPolicy() map[string]any {
	provider := map[string]any{"name": p.Provider}
	for k, v := range p.Credentials {
		provider[k] = v
	}
	subjects := append([]string(nil), p.Subjects...)
	sort.Strings(subjects)
	return map[string]any{
		"@id":      PolicyIDPrefix + p.ProviderID,
		"subjects": subjects,
		"issuers": []any{map[string]any{
			"module":     "acme",
			"challenges": map[string]any{"dns": map[string]any{"provider": provider}},
		}},
	}
}

// DecryptDNSCredentials unwraps a provider's stored credentials. It lives here,
// not in service, because the reconciler needs it and service imports proxy.
func DecryptDNSCredentials(dec Decryptor, encrypted []byte) (map[string]string, error) {
	raw, err := dec.Decrypt(encrypted)
	if err != nil {
		return nil, fmt.Errorf("decrypting dns credentials: %w", err)
	}
	var fields map[string]string
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil, fmt.Errorf("decoding dns credentials: %w", err)
	}
	return fields, nil
}
