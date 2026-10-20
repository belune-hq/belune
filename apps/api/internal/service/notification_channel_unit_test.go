package service

import (
	"encoding/json"
	"testing"

	"github.com/belune-hq/belune/internal/notify"
	"github.com/belune-hq/belune/internal/pkg/crypto"
)

func newChannelSvc(t *testing.T) *NotificationChannelService {
	t.Helper()
	keyring, err := crypto.ParseKeyringEnv("", testKeyHex, "")
	if err != nil {
		t.Fatalf("build keyring: %v", err)
	}
	// nil queries is fine: these tests exercise only crypto + validation, which
	// never touch the DB (mirrors backup_destination_unit_test).
	return NewNotificationChannelService(nil, keyring, notify.NewRegistry(nil), "https://belune.example")
}

func TestChannelConfigRoundTrip(t *testing.T) {
	svc := newChannelSvc(t)
	in := json.RawMessage(`{"webhook_url":"https://discord.example/webhooks/1/abc"}`)

	enc, err := svc.encryptConfig(in)
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	if len(enc) == 0 {
		t.Fatal("expected non-empty ciphertext")
	}
	out, err := svc.decryptConfig(enc)
	if err != nil {
		t.Fatalf("decrypt: %v", err)
	}
	if string(out) != string(in) {
		t.Fatalf("round trip mismatch: got %s want %s", out, in)
	}
}

func TestChannelValidate(t *testing.T) {
	svc := newChannelSvc(t)
	cases := []struct {
		name    string
		typ     string
		events  []string
		config  string
		wantErr bool
	}{
		{"valid discord", "discord", []string{notify.EventDeploymentFailed}, `{"webhook_url":"https://x"}`, false},
		{"unknown type", "carrierpigeon", nil, `{}`, true},
		{"unknown event", "discord", []string{"volcano.erupted"}, `{"webhook_url":"https://x"}`, true},
		{"bad config", "discord", nil, `{}`, true},
		{"valid multi-event", "ntfy", []string{notify.EventTLSExpiring, notify.EventTLSExpired}, `{"topic":"alerts"}`, false},
	}
	for _, c := range cases {
		err := svc.validate(c.typ, c.events, json.RawMessage(c.config))
		if (err != nil) != c.wantErr {
			t.Errorf("%s: err = %v, wantErr = %v", c.name, err, c.wantErr)
		}
	}
}

func TestChannelDecryptEmpty(t *testing.T) {
	svc := newChannelSvc(t)
	if _, err := svc.decryptConfig(nil); err == nil {
		t.Fatal("expected error decrypting empty config")
	}
}
