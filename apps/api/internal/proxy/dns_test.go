package proxy

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestDNSProviderNormalize(t *testing.T) {
	cf := DNSProviders["cloudflare"]
	r53 := DNSProviders["route53"]

	if got, err := cf.Normalize(map[string]string{"api_token": "  tok  "}); err != nil || got["api_token"] != "tok" {
		t.Errorf("trim: got %v, %v", got, err)
	}
	for name, tc := range map[string]struct {
		spec   DNSProviderSpec
		fields map[string]string
		want   string
	}{
		"missing required": {cf, map[string]string{}, "api_token is required"},
		"blank required":   {cf, map[string]string{"api_token": "  "}, "api_token is required"},
		"unknown field":    {cf, map[string]string{"api_token": "t", "apitoken": "x"}, "unknown credential field"},
		"newline":          {cf, map[string]string{"api_token": "a\nb"}, "line breaks"},
		"too long":         {cf, map[string]string{"api_token": strings.Repeat("a", maxDNSCredentialLen+1)}, "too long"},
		"half a pair":      {r53, map[string]string{"access_key_id": "AKIA"}, "secret_access_key is required"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := tc.spec.Normalize(tc.fields)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("got %v, want error containing %q", err, tc.want)
			}
		})
	}
}

// The module keys here are the caddy-dns modules' own JSON names (read from the
// libdns provider structs), so a rename on our side breaks issuance silently.
func TestCaddyPolicyShape(t *testing.T) {
	p := DNSPolicy{
		ProviderID: "abc", Provider: "cloudflare",
		Subjects:    []string{"b.example.com", "*.example.com"},
		Credentials: map[string]string{"api_token": "tok"},
	}
	raw, err := json.Marshal(p.CaddyPolicy())
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		ID       string   `json:"@id"`
		Subjects []string `json:"subjects"`
		Issuers  []struct {
			Module     string `json:"module"`
			Challenges struct {
				DNS struct {
					Provider map[string]string `json:"provider"`
				} `json:"dns"`
			} `json:"challenges"`
		} `json:"issuers"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if got.ID != PolicyIDPrefix+"abc" {
		t.Errorf("id = %q", got.ID)
	}
	if strings.Join(got.Subjects, ",") != "*.example.com,b.example.com" {
		t.Errorf("subjects not sorted: %v", got.Subjects)
	}
	if len(got.Issuers) != 1 || got.Issuers[0].Module != "acme" {
		t.Fatalf("issuers = %+v", got.Issuers)
	}
	prov := got.Issuers[0].Challenges.DNS.Provider
	if prov["name"] != "cloudflare" || prov["api_token"] != "tok" {
		t.Errorf("provider = %v", prov)
	}
}
