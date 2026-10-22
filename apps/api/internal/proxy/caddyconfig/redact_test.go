package caddyconfig

import (
	"encoding/json"
	"strings"
	"testing"
)

const privateKeyPEM = "-----BEGIN PRIVATE KEY-----\\nMIIEvQIBADANBgkqhkiG9w0BAQEFAASC\\n-----END PRIVATE KEY-----\\n"

// The shape tls.go actually pushes.
const withCustomCert = `{
  "apps": {
    "tls": {"certificates": {"load_pem": [
      {"certificate": "-----BEGIN CERTIFICATE-----\\nAAA\\n-----END CERTIFICATE-----\\n",
       "key": "` + privateKeyPEM + `",
       "tags": ["example.com"]}
    ]}},
    "http": {"servers": {"srv0": {"routes": [{"handle": [
      {"handler": "authentication", "providers": {"http_basic": {"accounts": [
        {"username": "admin", "password": "$2a$14$abcdefghijklmnopqrstuv"}
      ]}}}
    ]}]}}}
  }
}`

func marshal(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// The assertion that stops a private key reaching a browser.
func TestRedactRemovesTLSPrivateKeys(t *testing.T) {
	cfg, n, err := Redact([]byte(withCustomCert))
	if err != nil {
		t.Fatal(err)
	}
	out := marshal(t, cfg)

	if strings.Contains(out, "PRIVATE KEY") || strings.Contains(out, "MIIEvQIBADANBgkqhkiG9w0BAQEFAASC") {
		t.Fatalf("private key material survived redaction:\n%s", out)
	}
	if strings.Contains(out, "$2a$14$") {
		t.Errorf("basic-auth hash survived redaction:\n%s", out)
	}
	if n != 2 {
		t.Errorf("redacted %d values, want 2 (key + password)", n)
	}
}

func TestRedactKeepsEverythingElse(t *testing.T) {
	cfg, _, _ := Redact([]byte(withCustomCert))
	out := marshal(t, cfg)
	for _, want := range []string{"BEGIN CERTIFICATE", `"tags":["example.com"]`, `"username":"admin"`, `"handler":"authentication"`} {
		if !strings.Contains(out, want) {
			t.Errorf("redaction removed too much: %s missing from\n%s", want, out)
		}
	}
	if !strings.Contains(out, Redacted) {
		t.Error("a redacted field should leave a marker, so the reader knows it existed")
	}
}

// A path list would miss this. A key-name match does not.
func TestRedactCatchesSecretsAtUnforeseenPaths(t *testing.T) {
	cfg, n, err := Redact([]byte(`{"a":{"b":[{"dns":{"provider":{"name":"cloudflare","API_Token":"sekret"}}}]}}`))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(marshal(t, cfg), "sekret") || n != 1 {
		t.Errorf("cfg = %s, n = %d", marshal(t, cfg), n)
	}
}

func TestRedactDoesNotMangleNumbers(t *testing.T) {
	cfg, _, _ := Redact([]byte(`{"id": 9007199254740993, "ok": true, "nothing": null}`))
	out := marshal(t, cfg)
	if !strings.Contains(out, "9007199254740993") {
		t.Errorf("large integer was rounded: %s", out)
	}
}

func TestRedactFailsClosedOnInvalidJSON(t *testing.T) {
	cfg, _, err := Redact([]byte(`{"key": "-----BEGIN PRIVATE KEY-----`))
	if err == nil || cfg != nil {
		t.Fatalf("invalid input must be an error and return nothing; got %v, %v", cfg, err)
	}
}
