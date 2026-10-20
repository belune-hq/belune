package crypto_test

import (
	"bytes"
	"encoding/hex"
	"strings"
	"testing"

	"github.com/belune-hq/belune/internal/pkg/crypto"
)

const (
	testKeyHexV1 = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	testKeyHexV2 = "fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210"
	testKeyHexV3 = "aaaabbbbccccddddeeeeffff0000111122223333444455556666777788889999"
)

func mustKeyring(t *testing.T, keysEnv, keyEnv, current string) *crypto.Keyring {
	t.Helper()
	k, err := crypto.ParseKeyringEnv(keysEnv, keyEnv, current)
	if err != nil {
		t.Fatalf("build keyring: %v", err)
	}
	return k
}

func TestKeyring_EncryptDecryptRoundtrip(t *testing.T) {
	k := mustKeyring(t, "", testKeyHexV1, "")

	cases := [][]byte{
		nil,
		{},
		[]byte("hello"),
		[]byte("a"),
		bytes.Repeat([]byte("x"), 10_000),
	}
	for _, pt := range cases {
		ct, err := k.Encrypt(pt)
		if err != nil {
			t.Fatalf("encrypt: %v", err)
		}
		got, err := k.Decrypt(ct)
		if err != nil {
			t.Fatalf("decrypt: %v", err)
		}
		if !bytes.Equal(got, pt) {
			t.Errorf("roundtrip mismatch: got %q, want %q", got, pt)
		}
	}
}

func TestKeyring_EnvelopeHasMagicAndVersion(t *testing.T) {
	k := mustKeyring(t, "v1:"+testKeyHexV1+",v2:"+testKeyHexV2, "", "v2")

	ct, err := k.Encrypt([]byte("hello"))
	if err != nil {
		t.Fatal(err)
	}
	wantPrefix := []byte{'P', 'a', 'a', 'S', 0x01, 0x02} // magic + kek v2
	if !bytes.HasPrefix(ct, wantPrefix) {
		t.Errorf("envelope prefix mismatch: %x", ct[:6])
	}
	if !k.IsCurrent(ct) {
		t.Error("freshly encrypted blob should be reported as current")
	}
}

func TestKeyring_LegacyDecrypt(t *testing.T) {
	// Encrypt using the pre-keyring legacy function, then decrypt via the keyring.
	legacyCT, err := crypto.Encrypt([]byte("legacy-secret"), testKeyHexV1)
	if err != nil {
		t.Fatalf("legacy encrypt: %v", err)
	}

	k := mustKeyring(t, "", testKeyHexV1, "")
	got, err := k.Decrypt(legacyCT)
	if err != nil {
		t.Fatalf("keyring decrypt legacy: %v", err)
	}
	if string(got) != "legacy-secret" {
		t.Errorf("got %q, want %q", got, "legacy-secret")
	}
	if k.IsCurrent(legacyCT) {
		t.Error("legacy ciphertext must not be reported as current")
	}
}

func TestKeyring_LegacyDecryptWhenV1NotPresent(t *testing.T) {
	legacyCT, err := crypto.Encrypt([]byte("x"), testKeyHexV1)
	if err != nil {
		t.Fatal(err)
	}
	k := mustKeyring(t, "v2:"+testKeyHexV2, "", "v2")
	_, err = k.Decrypt(legacyCT)
	if err == nil {
		t.Error("expected error when v1 missing for legacy decrypt")
	}
}

func TestKeyring_RewrapLegacyToEnvelope(t *testing.T) {
	legacyCT, err := crypto.Encrypt([]byte("rewrap-me"), testKeyHexV1)
	if err != nil {
		t.Fatal(err)
	}

	k := mustKeyring(t, "v1:"+testKeyHexV1+",v2:"+testKeyHexV2, "", "v2")
	upgraded, err := k.Rewrap(legacyCT)
	if err != nil {
		t.Fatalf("rewrap: %v", err)
	}
	if !k.IsCurrent(upgraded) {
		t.Error("rewrapped blob should be current (v2)")
	}

	got, err := k.Decrypt(upgraded)
	if err != nil {
		t.Fatalf("decrypt rewrapped: %v", err)
	}
	if string(got) != "rewrap-me" {
		t.Errorf("got %q, want %q", got, "rewrap-me")
	}
}

func TestKeyring_RewrapAcrossVersions(t *testing.T) {
	// Start with v1 current, encrypt, then rotate to v2 current and rewrap.
	k1 := mustKeyring(t, "v1:"+testKeyHexV1, "", "v1")
	ct, err := k1.Encrypt([]byte("rotate-me"))
	if err != nil {
		t.Fatal(err)
	}

	k2 := mustKeyring(t, "v1:"+testKeyHexV1+",v2:"+testKeyHexV2, "", "v2")
	if k2.IsCurrent(ct) {
		t.Error("v1-encrypted blob should not be current under v2 keyring")
	}
	got, err := k2.Decrypt(ct)
	if err != nil {
		t.Fatalf("decrypt v1 blob with v2 keyring: %v", err)
	}
	if string(got) != "rotate-me" {
		t.Errorf("decrypt mismatch: %q", got)
	}

	upgraded, err := k2.Rewrap(ct)
	if err != nil {
		t.Fatalf("rewrap: %v", err)
	}
	if !k2.IsCurrent(upgraded) {
		t.Error("rewrapped blob should report as current")
	}
}

func TestKeyring_RewrapIdempotent(t *testing.T) {
	k := mustKeyring(t, "v1:"+testKeyHexV1+",v2:"+testKeyHexV2, "", "v2")
	ct, err := k.Encrypt([]byte("already-current"))
	if err != nil {
		t.Fatal(err)
	}
	ct2, err := k.Rewrap(ct)
	if err != nil {
		t.Fatalf("rewrap current: %v", err)
	}
	// Each rewrap produces fresh nonces/DEK, so bytes differ, but plaintext is preserved.
	if bytes.Equal(ct, ct2) {
		t.Error("rewrap should produce fresh ciphertext (new nonces)")
	}
	pt, err := k.Decrypt(ct2)
	if err != nil {
		t.Fatalf("decrypt rewrapped: %v", err)
	}
	if string(pt) != "already-current" {
		t.Errorf("got %q", pt)
	}
}

func TestKeyring_DecryptUnknownVersionFails(t *testing.T) {
	// Encrypt with v2-only keyring, then try to decrypt with v1-only.
	kEnc := mustKeyring(t, "v2:"+testKeyHexV2, "", "v2")
	ct, err := kEnc.Encrypt([]byte("x"))
	if err != nil {
		t.Fatal(err)
	}
	kDec := mustKeyring(t, "v1:"+testKeyHexV1, "", "v1")
	if _, err := kDec.Decrypt(ct); err == nil {
		t.Error("expected unknown-version error")
	}
}

func TestKeyring_TamperedCiphertextFails(t *testing.T) {
	k := mustKeyring(t, "", testKeyHexV1, "")
	ct, err := k.Encrypt([]byte("secret"))
	if err != nil {
		t.Fatal(err)
	}
	// Flip a byte deep in the data ciphertext region.
	tampered := make([]byte, len(ct))
	copy(tampered, ct)
	tampered[len(tampered)-1] ^= 0xFF
	if _, err := k.Decrypt(tampered); err == nil {
		t.Error("GCM should reject tampered ciphertext")
	}
}

func TestKeyring_ShortCiphertextTreatedAsLegacy(t *testing.T) {
	k := mustKeyring(t, "", testKeyHexV1, "")
	// A buffer shorter than envelopeMinLen cannot be envelope; it will try legacy and fail.
	if _, err := k.Decrypt([]byte("too-short")); err == nil {
		t.Error("expected error on short ciphertext")
	}
}

func TestParseKeyringEnv_LegacyCompat(t *testing.T) {
	k, err := crypto.ParseKeyringEnv("", testKeyHexV1, "")
	if err != nil {
		t.Fatalf("legacy single key: %v", err)
	}
	if k.CurrentVersion() != 1 {
		t.Errorf("legacy single key should be v1, got v%d", k.CurrentVersion())
	}
}

func TestParseKeyringEnv_MultiKeyDefaultsToHighest(t *testing.T) {
	k, err := crypto.ParseKeyringEnv("v1:"+testKeyHexV1+",v3:"+testKeyHexV3+",v2:"+testKeyHexV2, "", "")
	if err != nil {
		t.Fatalf("%v", err)
	}
	if k.CurrentVersion() != 3 {
		t.Errorf("default current should be highest (v3), got v%d", k.CurrentVersion())
	}
	versions := k.Versions()
	if len(versions) != 3 || versions[0] != 1 || versions[1] != 2 || versions[2] != 3 {
		t.Errorf("Versions() = %v, want [1 2 3]", versions)
	}
}

func TestParseKeyringEnv_ExplicitCurrent(t *testing.T) {
	k, err := crypto.ParseKeyringEnv("v1:"+testKeyHexV1+",v2:"+testKeyHexV2, "", "v1")
	if err != nil {
		t.Fatalf("%v", err)
	}
	if k.CurrentVersion() != 1 {
		t.Errorf("current v1 override should apply, got v%d", k.CurrentVersion())
	}
}

func TestParseKeyringEnv_Errors(t *testing.T) {
	cases := []struct {
		name         string
		keys, key, c string
		wantContains string
	}{
		{"no keys at all", "", "", "", "no keys configured"},
		{"bad entry format", "v1" + testKeyHexV1, "", "", "invalid ENCRYPTION_KEYS entry"},
		{"bad version prefix", "vv1:" + testKeyHexV1, "", "", "invalid version"},
		{"short hex", "v1:abcd", "", "", "bytes"},
		{"duplicate version", "v1:" + testKeyHexV1 + ",v1:" + testKeyHexV2, "", "", "duplicate version"},
		{"current missing", "v1:" + testKeyHexV1, "", "v2", "current version 2 not present"},
		{"legacy key wrong length", "", "abcd", "", "ENCRYPTION_KEY"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := crypto.ParseKeyringEnv(tc.keys, tc.key, tc.c)
			if err == nil {
				t.Fatalf("expected error")
			}
			if !strings.Contains(err.Error(), tc.wantContains) {
				t.Errorf("error %q does not contain %q", err.Error(), tc.wantContains)
			}
		})
	}
}

func TestKeyring_EnvelopeSizeOverhead(t *testing.T) {
	k := mustKeyring(t, "", testKeyHexV1, "")
	plaintext := []byte("a")
	ct, err := k.Encrypt(plaintext)
	if err != nil {
		t.Fatal(err)
	}
	// magic(5) + kek_ver(1) + dek_nonce(12) + wrapped_dek(48) + data_nonce(12) + data_ct(1+16) = 95
	want := 5 + 1 + 12 + 48 + 12 + 1 + 16
	if len(ct) != want {
		t.Errorf("envelope length = %d, want %d", len(ct), want)
	}
}

func TestKeyring_MagicDoesNotCollideWithLegacyInPractice(t *testing.T) {
	// Sanity: the legacy crypto.Encrypt output shouldn't start with the envelope
	// magic bytes, for any of a batch of random plaintexts.
	k := mustKeyring(t, "", testKeyHexV1, "")
	_ = k
	for i := 0; i < 1000; i++ {
		ct, err := crypto.Encrypt([]byte("some-plaintext"), testKeyHexV1)
		if err != nil {
			t.Fatal(err)
		}
		if len(ct) >= 5 && bytes.Equal(ct[:5], []byte{'P', 'a', 'a', 'S', 0x01}) {
			t.Errorf("legacy ciphertext collided with envelope magic (iteration %d): %s",
				i, hex.EncodeToString(ct[:5]))
		}
	}
}
