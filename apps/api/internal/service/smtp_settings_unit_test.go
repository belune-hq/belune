package service

import (
	"testing"

	"github.com/belune-hq/belune/internal/config"
	"github.com/belune-hq/belune/internal/pkg/crypto"
)

func newSMTPSvc(t *testing.T) *SMTPSettingsService {
	t.Helper()
	keyring, err := crypto.ParseKeyringEnv("", testKeyHex, "")
	if err != nil {
		t.Fatalf("build keyring: %v", err)
	}
	// nil queries: these tests exercise only the crypto helpers, no DB access.
	return NewSMTPSettingsService(nil, keyring, &config.Config{})
}

func TestSMTPPasswordRoundTrip(t *testing.T) {
	s := newSMTPSvc(t)
	in := "sup3r/secret+pw=="

	enc, err := s.encryptPassword(in)
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	if enc == "" || enc == in {
		t.Fatal("expected non-empty ciphertext distinct from plaintext")
	}
	out, err := s.decryptPassword(enc)
	if err != nil {
		t.Fatalf("decrypt: %v", err)
	}
	if out != in {
		t.Fatalf("round trip mismatch: got %q want %q", out, in)
	}
}

func TestSMTPDecryptRejectsGarbage(t *testing.T) {
	s := newSMTPSvc(t)
	// A plaintext value written directly to the setting (e.g. via the generic
	// settings endpoint) must not decrypt to something usable.
	if _, err := s.decryptPassword("not-base64-or-ciphertext"); err == nil {
		t.Fatal("expected error decrypting non-ciphertext value")
	}
}

func TestValidTLSModes(t *testing.T) {
	for _, m := range []string{"none", "starttls", "tls"} {
		if !validTLSModes[m] {
			t.Errorf("%q should be valid", m)
		}
	}
	if validTLSModes["ssl"] {
		t.Error("ssl should not be a valid mode")
	}
}
