// Package caddyconfig prepares Caddy's running configuration for display.
//
// The live config is not safe to show as it is. Belune pushes the unencrypted
// private key of every custom-TLS domain into it (proxy/caddy/tls.go builds
// load_pem entries with the key in the clear), and basic-auth password hashes
// ride along in the route handlers. Elsewhere in Belune those values are
// keyring-encrypted at rest and masked on read; showing them here would be a new
// way to read what everywhere else refuses to reveal.
package caddyconfig

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
)

// Redacted replaces every secret value, so a reader can see a field existed
// without being able to read it.
const Redacted = "[redacted]"

// secretKeys are redacted wherever they appear, by name, rather than at known
// paths. A path list is what a future module's credential (a DNS provider token,
// a CrowdSec API key) would silently fall outside; a key-name list errs toward
// hiding a harmless field, which costs a reader a little detail, over leaking a
// secret, which costs a certificate. Compared lower-cased.
//
// "key" is the private key inside tls.certificates.load_pem[]; "password" holds
// the bcrypt hash inside http_basic accounts.
var secretKeys = map[string]struct{}{
	"key":               {},
	"private_key":       {},
	"password":          {},
	"secret":            {},
	"secret_key":        {},
	"secret_access_key": {},
	"api_key":           {},
	"api_token":         {},
	"auth_token":        {},
	"token":             {},
	"credentials":       {},
}

// Redact returns the config with every secret value replaced, and how many were.
// It works on the decoded tree so redaction cannot be defeated by formatting, and
// it fails closed: input that is not valid JSON is an error, never passed through.
func Redact(raw []byte) (config any, redacted int, err error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	// Numbers stay as written; float64 would round-trip large integers lossily.
	dec.UseNumber()
	var tree any
	if err := dec.Decode(&tree); err != nil {
		return nil, 0, fmt.Errorf("decoding caddy config: %w", err)
	}
	return redact(tree, &redacted), redacted, nil
}

func redact(v any, n *int) any {
	switch t := v.(type) {
	case map[string]any:
		for k, child := range t {
			if _, secret := secretKeys[strings.ToLower(k)]; secret && child != nil {
				t[k] = Redacted
				*n++
				continue
			}
			t[k] = redact(child, n)
		}
		return t
	case []any:
		for i, child := range t {
			t[i] = redact(child, n)
		}
		return t
	}
	return v
}
