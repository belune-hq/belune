package handler_test

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/belune-hq/belune/internal/testutil"
)

func TestGetStats_AdminAndMemberSplit(t *testing.T) {
	resetDB(t)
	adminToken := env.SetupAdmin(t, "admin@test.com", "password123")

	env.DoRequest(t, "POST", "/api/users", map[string]string{
		"email":    "member@test.com",
		"password": "password123",
		"role":     "member",
	}, testutil.AuthHeader(adminToken)).Body.Close()
	memberToken := env.LoginAs(t, "member@test.com", "password123")

	env.CreateProject(t, adminToken, "Project 1", "project-1")

	// Admin: exercises every aggregate query, sees host + is_admin true.
	resp := env.DoRequest(t, "GET", "/api/stats", nil, testutil.AuthHeader(adminToken))
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	admin := testutil.ReadJSON(t, resp)
	assert.Equal(t, true, admin["is_admin"])
	assert.NotNil(t, admin["host"])
	assert.Contains(t, admin, "app_health")
	assert.Contains(t, admin, "deploy_7d")
	assert.Contains(t, admin, "needs_attention")

	// Member: scoped view, no host snapshot.
	resp = env.DoRequest(t, "GET", "/api/stats", nil, testutil.AuthHeader(memberToken))
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	member := testutil.ReadJSON(t, resp)
	assert.Equal(t, false, member["is_admin"])
	assert.Nil(t, member["host"])
}

// TestGetStats_ConfigWarnings covers W4's delivery path: a long access-token
// TTL is reported to admins on the dashboard, beside needs_attention rather
// than inside it — that block counts failing workloads, and a configuration
// finding is a different kind of thing.
func TestGetStats_ConfigWarnings(t *testing.T) {
	resetDB(t)
	adminToken := env.SetupAdmin(t, "admin@test.com", "password123")

	// Default TTL: nothing to say.
	resp := env.DoRequest(t, "GET", "/api/stats", nil, testutil.AuthHeader(adminToken))
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Nil(t, testutil.ReadJSON(t, resp)["config_warnings"],
		"a sane configuration should report no warnings")

	// The value old installs still carry in their .env.
	original := env.Config.JWTExpiryHours
	env.Config.JWTExpiryHours = 24
	t.Cleanup(func() { env.Config.JWTExpiryHours = original })

	resp = env.DoRequest(t, "GET", "/api/stats", nil, testutil.AuthHeader(adminToken))
	require.Equal(t, http.StatusOK, resp.StatusCode)
	body := testutil.ReadJSON(t, resp)

	warnings, ok := body["config_warnings"].([]any)
	require.True(t, ok, "config_warnings should be present for an admin")
	require.Len(t, warnings, 1)
	first := warnings[0].(map[string]any)
	assert.Equal(t, "jwt_expiry_too_long", first["code"])
	assert.Contains(t, first["remedy"], "JWT_EXPIRY_HOURS",
		"the remedy must name the variable to remove, not just warn")

	// The count of broken workloads must not absorb a configuration finding.
	attention := body["needs_attention"].(map[string]any)
	assert.EqualValues(t, 0, attention["total"])

	// Members do not see host or configuration detail.
	env.DoRequest(t, "POST", "/api/users", map[string]string{
		"email": "member2@test.com", "password": "password123", "role": "member",
	}, testutil.AuthHeader(adminToken)).Body.Close()
	memberToken := env.LoginAs(t, "member2@test.com", "password123")
	resp = env.DoRequest(t, "GET", "/api/stats", nil, testutil.AuthHeader(memberToken))
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Nil(t, testutil.ReadJSON(t, resp)["config_warnings"])
}
