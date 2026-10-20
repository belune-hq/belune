package service

import (
	"testing"

	"github.com/belune-hq/belune/internal/pkg/crypto"
)

// 32-byte hex key for AES-256 (inlined to avoid importing testutil, which would
// create an import cycle for this internal-package test).
const testKeyHex = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

// TestDestinationCredentialsRoundTrip verifies that credentials encrypt and
// decrypt back to the same values via the keyring (no DB needed).
func TestDestinationCredentialsRoundTrip(t *testing.T) {
	keyring, err := crypto.ParseKeyringEnv("", testKeyHex, "")
	if err != nil {
		t.Fatalf("build keyring: %v", err)
	}
	svc := NewBackupDestinationService(nil, keyring)

	in := &DestinationCredentials{AccessKey: "AKIAEXAMPLE", SecretKey: "s3cr3t/value+with=chars"}
	enc, err := svc.encryptCredentials(in)
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	if len(enc) == 0 {
		t.Fatal("expected non-empty ciphertext")
	}

	out, err := svc.decryptCredentials(enc)
	if err != nil {
		t.Fatalf("decrypt: %v", err)
	}
	if out.AccessKey != in.AccessKey || out.SecretKey != in.SecretKey {
		t.Fatalf("round trip mismatch: got %+v want %+v", out, in)
	}
}

func TestDecryptCredentialsEmpty(t *testing.T) {
	keyring, err := crypto.ParseKeyringEnv("", testKeyHex, "")
	if err != nil {
		t.Fatalf("build keyring: %v", err)
	}
	svc := NewBackupDestinationService(nil, keyring)
	if _, err := svc.decryptCredentials(nil); err == nil {
		t.Fatal("expected error for empty credentials")
	}
}
