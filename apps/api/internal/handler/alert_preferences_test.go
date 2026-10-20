package handler_test

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/belune-hq/belune/internal/testutil"
)

func TestGetAlertPreferences_DefaultsWhenNoRow(t *testing.T) {
	resetDB(t)
	token := env.SetupAdmin(t, "admin@test.com", "password123")

	resp := env.DoRequest(t, "GET", "/api/account/alert-preferences", nil, testutil.AuthHeader(token))
	require.Equal(t, http.StatusOK, resp.StatusCode)

	result := testutil.ReadJSON(t, resp)
	assert.Equal(t, true, result["deploy_failures"])
	assert.Equal(t, true, result["build_failures"])
	assert.Equal(t, true, result["quota_threshold"])
	assert.Equal(t, float64(80), result["quota_threshold_percent"])
}

func TestUpdateAlertPreferences_RoundTrip(t *testing.T) {
	resetDB(t)
	token := env.SetupAdmin(t, "admin@test.com", "password123")

	resp := env.DoRequest(t, "PUT", "/api/account/alert-preferences", map[string]any{
		"deploy_failures":         false,
		"build_failures":          true,
		"quota_threshold":         true,
		"quota_threshold_percent": 90,
	}, testutil.AuthHeader(token))
	require.Equal(t, http.StatusOK, resp.StatusCode)

	result := testutil.ReadJSON(t, resp)
	assert.Equal(t, false, result["deploy_failures"])
	assert.Equal(t, true, result["build_failures"])
	assert.Equal(t, true, result["quota_threshold"])
	assert.Equal(t, float64(90), result["quota_threshold_percent"])

	// GET must reflect the stored values.
	get := env.DoRequest(t, "GET", "/api/account/alert-preferences", nil, testutil.AuthHeader(token))
	require.Equal(t, http.StatusOK, get.StatusCode)
	persisted := testutil.ReadJSON(t, get)
	assert.Equal(t, false, persisted["deploy_failures"])
	assert.Equal(t, float64(90), persisted["quota_threshold_percent"])
}

func TestUpdateAlertPreferences_InvalidPercent(t *testing.T) {
	resetDB(t)
	token := env.SetupAdmin(t, "admin@test.com", "password123")

	resp := env.DoRequest(t, "PUT", "/api/account/alert-preferences", map[string]any{
		"deploy_failures":         true,
		"build_failures":          true,
		"quota_threshold":         true,
		"quota_threshold_percent": 0,
	}, testutil.AuthHeader(token))
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)

	resp = env.DoRequest(t, "PUT", "/api/account/alert-preferences", map[string]any{
		"deploy_failures":         true,
		"build_failures":          true,
		"quota_threshold":         true,
		"quota_threshold_percent": 101,
	}, testutil.AuthHeader(token))
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

func TestGetAlertPreferences_RequiresAuth(t *testing.T) {
	resetDB(t)

	resp := env.DoRequest(t, "GET", "/api/account/alert-preferences", nil, map[string]string{})
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
}

func TestUpdateAlertPreferences_IsolatedByUser(t *testing.T) {
	resetDB(t)
	adminToken := env.SetupAdmin(t, "admin@test.com", "password123")

	// Create a second user and get their token.
	env.DoRequest(t, "POST", "/api/users", map[string]string{
		"email":    "member@test.com",
		"password": "password123",
		"role":     "member",
	}, testutil.AuthHeader(adminToken))
	memberResp := env.DoRequest(t, "POST", "/api/auth/login", map[string]string{
		"email":    "member@test.com",
		"password": "password123",
	}, map[string]string{})
	require.Equal(t, http.StatusOK, memberResp.StatusCode)
	memberToken := testutil.ReadJSON(t, memberResp)["token"].(string)

	// Admin sets threshold to 60.
	env.DoRequest(t, "PUT", "/api/account/alert-preferences", map[string]any{
		"deploy_failures":         true,
		"build_failures":          true,
		"quota_threshold":         true,
		"quota_threshold_percent": 60,
	}, testutil.AuthHeader(adminToken))

	// Member still sees the default (80).
	resp := env.DoRequest(t, "GET", "/api/account/alert-preferences", nil, testutil.AuthHeader(memberToken))
	require.Equal(t, http.StatusOK, resp.StatusCode)
	result := testutil.ReadJSON(t, resp)
	assert.Equal(t, float64(80), result["quota_threshold_percent"])
}
