package deploy_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/belune-hq/belune/internal/deploy"
)

func TestKey_Deterministic(t *testing.T) {
	a := deploy.Key("app-1", "push", "abc123")
	b := deploy.Key("app-1", "push", "abc123")
	assert.Equal(t, a, b, "same inputs must produce the same key")
}

func TestKey_DifferentApplicationIDs(t *testing.T) {
	a := deploy.Key("app-1", "push", "abc123")
	b := deploy.Key("app-2", "push", "abc123")
	assert.NotEqual(t, a, b, "different application IDs must produce different keys")
}

func TestKey_DifferentTriggers(t *testing.T) {
	a := deploy.Key("app-1", "push", "abc123")
	b := deploy.Key("app-1", "manual", "abc123")
	assert.NotEqual(t, a, b, "different triggers must produce different keys")
}

func TestKey_DifferentDiscriminators(t *testing.T) {
	a := deploy.Key("app-1", "push", "abc123")
	b := deploy.Key("app-1", "push", "def456")
	assert.NotEqual(t, a, b, "different discriminators must produce different keys")
}

func TestKey_EmptyDiscriminatorTriggerStillDiscriminates(t *testing.T) {
	// manual trigger uses an empty discriminator. The trigger field must still
	// contribute to the key so two different triggers with an empty discriminator
	// do not collide.
	push := deploy.Key("app-1", "push", "")
	manual := deploy.Key("app-1", "manual", "")
	assert.NotEqual(t, push, manual,
		"trigger must still discriminate when the discriminator is empty")
	// Stability: same inputs always produce the same key.
	assert.Equal(t, manual, deploy.Key("app-1", "manual", ""),
		"empty discriminator must produce a stable key")
}

func TestKey_SeparatorCollision(t *testing.T) {
	// Key concatenates with "|" as the separator. Verify that inputs which
	// contain the separator character cannot produce an equal key by shifting
	// field boundaries — i.e. ("app|1","push","abc") ≠ ("app","1|push","abc").
	a := deploy.Key("app|1", "push", "abc")
	b := deploy.Key("app", "1|push", "abc")
	assert.NotEqual(t, a, b,
		"pipe characters in inputs must not cause field-boundary collisions")
}

func TestKey_Format(t *testing.T) {
	k := deploy.Key("app-1", "push", "abc123")
	// SHA-256 hex digest → always 64 lowercase hex characters.
	require.Len(t, k, 64, "key must be 64 hex characters")
	for _, ch := range k {
		assert.True(t, (ch >= '0' && ch <= '9') || (ch >= 'a' && ch <= 'f'),
			"key must contain only lowercase hex characters, got %c", ch)
	}
}
