package handler_test

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/belune-hq/belune/internal/testutil"
)

func putSettings(t *testing.T, token string, kv ...[2]string) *http.Response {
	t.Helper()
	body := make([]map[string]string, 0, len(kv))
	for _, pair := range kv {
		body = append(body, map[string]string{"key": pair[0], "value": pair[1]})
	}
	return env.DoRequest(t, "PUT", "/api/settings", body, testutil.AuthHeader(token))
}

func settingValue(t *testing.T, token, key string) (string, bool) {
	t.Helper()
	resp := env.DoRequest(t, "GET", "/api/settings", nil, testutil.AuthHeader(token))
	require.Equal(t, http.StatusOK, resp.StatusCode)
	defer resp.Body.Close()

	for _, row := range testutil.ReadJSONArray(t, resp) {
		m := row.(map[string]any)
		if m["key"] == key {
			return fmt.Sprint(m["value"]), true
		}
	}
	return "", false
}

// TestUpdateSettings_RejectsUnknownKeys is the actual bug. The write loop used
// to upsert whatever it was handed behind an empty-key check alone, so a typo
// returned 200, created a real row that read back correctly, and left the
// operator debugging a feature that never turned on.
func TestUpdateSettings_RejectsUnknownKeys(t *testing.T) {
	resetDB(t)
	adminToken := env.SetupAdmin(t, "admin@test.com", "password123")

	// The motivating typo: one character off the host-shell gate.
	resp := putSettings(t, adminToken, [2]string{"host_shel_enabled", "true"})
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
	body := testutil.ReadJSON(t, resp)
	assert.Contains(t, body["error"], "host_shel_enabled")
	assert.Contains(t, body["error"], "host_shell_enabled",
		"the error should name what IS accepted, so the typo is self-correcting")

	// It must not have been written on the way to the error.
	_, found := settingValue(t, adminToken, "host_shel_enabled")
	assert.False(t, found, "a rejected key must not reach the settings table")

	// An empty key is the same silence in a different place — it used to be
	// skipped by the write loop rather than reported.
	resp = putSettings(t, adminToken, [2]string{"", "whatever"})
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
	resp.Body.Close()

	// ⚠️ A valid key alongside an invalid one must not be written either: the
	// request is rejected whole, or a partial save is indistinguishable from a
	// full one to the caller.
	resp = putSettings(t, adminToken,
		[2]string{"instance_name", "Should Not Persist"},
		[2]string{"nonsense_key", "x"})
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
	resp.Body.Close()
	got, _ := settingValue(t, adminToken, "instance_name")
	assert.NotEqual(t, "Should Not Persist", got, "a rejected request must write nothing")
}

// TestUpdateSettings_RejectsUpdateCheckCacheKeys guards a security property,
// not just a typo: update_latest_* and update_last_checked_at are the
// update-check worker's own cache (worker/update_check_task.go), written
// directly via UpsertSetting, never through this endpoint. If one leaked into
// the allowlist, a write-scoped admin token could spoof "you are already
// current" by overwriting what the last manifest fetch actually found.
func TestUpdateSettings_RejectsUpdateCheckCacheKeys(t *testing.T) {
	resetDB(t)
	adminToken := env.SetupAdmin(t, "admin@test.com", "password123")

	cacheKeys := []string{
		"update_latest_version",
		"update_latest_breaking",
		"update_latest_requires_host_update",
		"update_latest_notes_url",
		"update_last_checked_at",
	}
	for _, key := range cacheKeys {
		resp := putSettings(t, adminToken, [2]string{key, "9.9.9"})
		assert.Equal(t, http.StatusBadRequest, resp.StatusCode, "%s must not be writable via PUT /api/settings", key)
		resp.Body.Close()
	}
}

// TestUpdateSettings_AcceptsEveryAllowlistedKey is the other half, and the one
// that matters for not shipping a regression: an allowlist that is too NARROW
// silently breaks a working feature. Every key here is read somewhere in the
// codebase, so each must still round-trip.
func TestUpdateSettings_AcceptsEveryAllowlistedKey(t *testing.T) {
	resetDB(t)
	adminToken := env.SetupAdmin(t, "admin@test.com", "password123")

	cases := [][2]string{
		{"host_shell_enabled", "true"},
		{"daily_cleanup_enabled", "false"},
		{"control_plane_backup_enabled", "false"},
		{"instance_name", "Acme Homelab"},
		{"public_ip", "203.0.113.7"},
		{"control_plane_backup_schedule", "0 3 * * *"},
		{"control_plane_backup_retain_days", "30"},
		{"control_plane_backup_retain_count", "10"},
		{"app_log_retention_days", "14"},
		{"request_log_retention_days", "7"},
		{"audit_log_retention_days", "365"},
		{"orphaned_backup_retention_days", "90"},
		{"host_metrics_retention_hours", "48"},
		{"update_check_enabled", "false"},
		{"update_skip_version", "0.1.8"},
	}

	resp := putSettings(t, adminToken, cases...)
	require.Equal(t, http.StatusOK, resp.StatusCode, "every allowlisted key must be accepted together")
	resp.Body.Close()

	for _, c := range cases {
		got, found := settingValue(t, adminToken, c[0])
		assert.True(t, found, "%s should have been written", c[0])
		assert.Equal(t, c[1], got, "%s round-trips unchanged", c[0])
	}
}

// TestUpdateSettings_ValidatesValueTypes covers the second silence: the readers
// accept a retention value only when it parses above zero and fall back to a
// default otherwise, so an unvalidated typo is indistinguishable from unset at
// every layer below.
func TestUpdateSettings_ValidatesValueTypes(t *testing.T) {
	resetDB(t)
	adminToken := env.SetupAdmin(t, "admin@test.com", "password123")

	rejected := [][2]string{
		{"host_shell_enabled", "ture"},
		{"daily_cleanup_enabled", "yes"},
		{"app_log_retention_days", "fourteen"},
		{"app_log_retention_days", "0"},
		{"app_log_retention_days", "-5"},
		{"host_metrics_retention_hours", "abc"},
		{"public_ip", "not-an-ip"},
		{"control_plane_backup_schedule", "every tuesday"},
		{"update_check_enabled", "yes"},
		{"update_skip_version", strings.Repeat("x", 51)},
	}
	for _, c := range rejected {
		resp := putSettings(t, adminToken, c)
		assert.Equal(t, http.StatusBadRequest, resp.StatusCode, "%s=%q must be rejected", c[0], c[1])
		resp.Body.Close()
	}

	// Blank clears a knob back to its default and must stay legal — it is how the
	// UI expresses "no override", and rejecting it would strand anyone who set one.
	for _, key := range []string{"app_log_retention_days", "public_ip", "control_plane_backup_schedule", "host_shell_enabled"} {
		resp := putSettings(t, adminToken, [2]string{key, ""})
		assert.Equal(t, http.StatusOK, resp.StatusCode, "%s must accept a blank value as 'clear'", key)
		resp.Body.Close()
	}
}

// TestUpdateSettings_BooleanDefaultsAreNotNormalised guards a trap specific to
// these flags: daily_cleanup_enabled and control_plane_backup_enabled are ON
// unless the value is exactly "false", while host_shell_enabled is OFF unless it
// is exactly "true". A validator that "helpfully" rewrote blank to a literal
// would flip one of the two groups.
func TestUpdateSettings_BooleanDefaultsAreNotNormalised(t *testing.T) {
	resetDB(t)
	adminToken := env.SetupAdmin(t, "admin@test.com", "password123")

	for _, key := range []string{"host_shell_enabled", "daily_cleanup_enabled", "control_plane_backup_enabled", "update_check_enabled"} {
		resp := putSettings(t, adminToken, [2]string{key, ""})
		require.Equal(t, http.StatusOK, resp.StatusCode)
		resp.Body.Close()

		got, found := settingValue(t, adminToken, key)
		require.True(t, found)
		assert.Equal(t, "", got,
			"%s must store blank verbatim — rewriting it to true or false flips one of the two default directions", key)
	}
}
