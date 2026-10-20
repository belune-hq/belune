package handler_test

import (
	"encoding/json"
	"net/http"
	"net/url"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/belune-hq/belune/internal/testutil"
	"github.com/belune-hq/belune/internal/worker"
)

// extractInvitationToken extracts the plaintext token from the nth email task.
func extractInvitationToken(t *testing.T, taskIndex int) string {
	t.Helper()
	require.Greater(t, len(env.Asynq.Tasks), taskIndex, "expected email task at index %d", taskIndex)
	var payload worker.EmailSendPayload
	require.NoError(t, json.Unmarshal(env.Asynq.Tasks[taskIndex].Payload, &payload))
	vars, ok := payload.Vars.(map[string]any)
	require.True(t, ok, "payload Vars should decode as map[string]any")
	acceptURL, _ := vars["InviteURL"].(string)
	require.NotEmpty(t, acceptURL, "InviteURL must be present in email vars")
	parsed, err := url.Parse(acceptURL)
	require.NoError(t, err)
	token := parsed.Query().Get("token")
	require.NotEmpty(t, token, "token query param must be present in InviteURL")
	return token
}

func TestInviteUser_AdminCanInvite(t *testing.T) {
	resetDB(t)
	adminToken := env.SetupAdmin(t, "admin@test.com", "password123")

	resp := env.DoRequest(t, "POST", "/api/users/invite", map[string]string{
		"email": "newuser@test.com",
		"role":  "member",
	}, testutil.AuthHeader(adminToken))
	require.Equal(t, http.StatusCreated, resp.StatusCode)

	result := testutil.ReadJSON(t, resp)
	assert.Equal(t, "newuser@test.com", result["email"])
	assert.Equal(t, "member", result["role"])

	require.Len(t, env.Asynq.Tasks, 1, "expected one invitation email task")
	assert.Equal(t, "email:send", env.Asynq.Tasks[0].TypeName)
}

func TestInviteUser_RequiresAdmin(t *testing.T) {
	resetDB(t)
	adminToken := env.SetupAdmin(t, "admin@test.com", "password123")

	// Create a second non-admin user via the deprecated endpoint.
	resp := env.DoRequest(t, "POST", "/api/users", map[string]string{
		"email":    "member@test.com",
		"password": "password123",
		"role":     "member",
	}, testutil.AuthHeader(adminToken))
	require.Equal(t, http.StatusCreated, resp.StatusCode)
	resp.Body.Close()

	memberToken := env.LoginAs(t, "member@test.com", "password123")
	resp = env.DoRequest(t, "POST", "/api/users/invite", map[string]string{
		"email": "other@test.com",
		"role":  "member",
	}, testutil.AuthHeader(memberToken))
	assert.Equal(t, http.StatusForbidden, resp.StatusCode)
	resp.Body.Close()
}

func TestInviteUser_DuplicatePendingReplaced(t *testing.T) {
	resetDB(t)
	adminToken := env.SetupAdmin(t, "admin@test.com", "password123")

	// First invite.
	resp := env.DoRequest(t, "POST", "/api/users/invite", map[string]string{
		"email": "dup@test.com",
		"role":  "member",
	}, testutil.AuthHeader(adminToken))
	require.Equal(t, http.StatusCreated, resp.StatusCode)
	resp.Body.Close()

	// Second invite for the same email — should succeed (prior invalidated).
	resp = env.DoRequest(t, "POST", "/api/users/invite", map[string]string{
		"email": "dup@test.com",
		"role":  "member",
	}, testutil.AuthHeader(adminToken))
	assert.Equal(t, http.StatusCreated, resp.StatusCode)
	resp.Body.Close()

	// Only one pending invitation should remain.
	resp = env.DoRequest(t, "GET", "/api/users/invitations", nil, testutil.AuthHeader(adminToken))
	require.Equal(t, http.StatusOK, resp.StatusCode)
	list := testutil.ReadJSONArray(t, resp)
	assert.Len(t, list, 1, "only one pending invitation should exist after re-invite")
}

func TestInviteUser_ConflictsWithExistingUser(t *testing.T) {
	resetDB(t)
	adminToken := env.SetupAdmin(t, "admin@test.com", "password123")

	resp := env.DoRequest(t, "POST", "/api/users/invite", map[string]string{
		"email": "admin@test.com",
		"role":  "member",
	}, testutil.AuthHeader(adminToken))
	assert.Equal(t, http.StatusConflict, resp.StatusCode)
	resp.Body.Close()
}

func TestListPendingInvitations(t *testing.T) {
	resetDB(t)
	adminToken := env.SetupAdmin(t, "admin@test.com", "password123")

	for _, email := range []string{"a@test.com", "b@test.com"} {
		resp := env.DoRequest(t, "POST", "/api/users/invite", map[string]string{
			"email": email,
			"role":  "member",
		}, testutil.AuthHeader(adminToken))
		require.Equal(t, http.StatusCreated, resp.StatusCode)
		resp.Body.Close()
	}

	resp := env.DoRequest(t, "GET", "/api/users/invitations", nil, testutil.AuthHeader(adminToken))
	require.Equal(t, http.StatusOK, resp.StatusCode)
	list := testutil.ReadJSONArray(t, resp)
	assert.Len(t, list, 2)
}

func TestRevokeInvitation(t *testing.T) {
	resetDB(t)
	adminToken := env.SetupAdmin(t, "admin@test.com", "password123")

	resp := env.DoRequest(t, "POST", "/api/users/invite", map[string]string{
		"email": "revoke@test.com",
		"role":  "member",
	}, testutil.AuthHeader(adminToken))
	require.Equal(t, http.StatusCreated, resp.StatusCode)
	inv := testutil.ReadJSON(t, resp)
	invID, _ := inv["id"].(string)
	require.NotEmpty(t, invID)

	resp = env.DoRequest(t, "DELETE", "/api/users/invitations/"+invID, nil, testutil.AuthHeader(adminToken))
	assert.Equal(t, http.StatusNoContent, resp.StatusCode)
	resp.Body.Close()

	// List should now be empty.
	resp = env.DoRequest(t, "GET", "/api/users/invitations", nil, testutil.AuthHeader(adminToken))
	require.Equal(t, http.StatusOK, resp.StatusCode)
	list := testutil.ReadJSONArray(t, resp)
	assert.Empty(t, list)
}

func TestGetInvitation_ValidToken(t *testing.T) {
	resetDB(t)
	adminToken := env.SetupAdmin(t, "admin@test.com", "password123")

	resp := env.DoRequest(t, "POST", "/api/users/invite", map[string]string{
		"email": "peek@test.com",
		"role":  "member",
	}, testutil.AuthHeader(adminToken))
	require.Equal(t, http.StatusCreated, resp.StatusCode)
	resp.Body.Close()

	token := extractInvitationToken(t, 0)

	resp = env.DoRequest(t, "GET", "/api/auth/invitation?token="+token, nil, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	result := testutil.ReadJSON(t, resp)
	assert.Equal(t, "peek@test.com", result["email"])
	assert.Equal(t, "member", result["role"])
}

func TestGetInvitation_InvalidToken(t *testing.T) {
	resetDB(t)
	resp := env.DoRequest(t, "GET", "/api/auth/invitation?token=deadbeefdeadbeef", nil, nil)
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
	resp.Body.Close()
}

// TestAcceptInvitationFullFlow exercises the complete invite → accept → login path.
func TestAcceptInvitationFullFlow(t *testing.T) {
	resetDB(t)
	adminToken := env.SetupAdmin(t, "admin@test.com", "password123")

	// Admin invites a new user.
	resp := env.DoRequest(t, "POST", "/api/users/invite", map[string]string{
		"email": "invited@test.com",
		"role":  "member",
	}, testutil.AuthHeader(adminToken))
	require.Equal(t, http.StatusCreated, resp.StatusCode)
	resp.Body.Close()

	token := extractInvitationToken(t, 0)

	// Invited user accepts.
	resp = env.DoRequest(t, "POST", "/api/auth/accept-invitation", map[string]string{
		"token":      token,
		"password":   "newpassword123",
		"username":   "inviteduser",
		"first_name": "Invited",
		"last_name":  "User",
	}, nil)
	require.Equal(t, http.StatusCreated, resp.StatusCode)
	result := testutil.ReadJSON(t, resp)
	assert.NotEmpty(t, result["token"], "response should carry a JWT")

	// New user can log in with their password.
	newToken := env.LoginAs(t, "invited@test.com", "newpassword123")
	assert.NotEmpty(t, newToken)

	// Accepted invitation no longer appears in pending list.
	resp = env.DoRequest(t, "GET", "/api/users/invitations", nil, testutil.AuthHeader(adminToken))
	require.Equal(t, http.StatusOK, resp.StatusCode)
	list := testutil.ReadJSONArray(t, resp)
	assert.Empty(t, list, "accepted invitation should not appear in pending list")
}

func TestAcceptInvitation_DoubleAccept(t *testing.T) {
	resetDB(t)
	adminToken := env.SetupAdmin(t, "admin@test.com", "password123")

	resp := env.DoRequest(t, "POST", "/api/users/invite", map[string]string{
		"email": "once@test.com",
		"role":  "member",
	}, testutil.AuthHeader(adminToken))
	require.Equal(t, http.StatusCreated, resp.StatusCode)
	resp.Body.Close()

	token := extractInvitationToken(t, 0)

	accept := func() int {
		r := env.DoRequest(t, "POST", "/api/auth/accept-invitation", map[string]string{
			"token":    token,
			"password": "somepassword123",
		}, nil)
		defer r.Body.Close()
		return r.StatusCode
	}

	assert.Equal(t, http.StatusCreated, accept(), "first acceptance should succeed")
	assert.Equal(t, http.StatusBadRequest, accept(), "second acceptance should be rejected")
}

func TestAcceptInvitation_ExpiredToken(t *testing.T) {
	resetDB(t)
	adminToken := env.SetupAdmin(t, "admin@test.com", "password123")

	resp := env.DoRequest(t, "POST", "/api/users/invite", map[string]string{
		"email": "expired@test.com",
		"role":  "member",
	}, testutil.AuthHeader(adminToken))
	require.Equal(t, http.StatusCreated, resp.StatusCode)
	resp.Body.Close()

	token := extractInvitationToken(t, 0)

	// Force-expire the invitation via direct DB update.
	ctx := t.Context()
	_, err := env.Pool.Exec(ctx, `UPDATE invitations SET expires_at = NOW() - INTERVAL '1 hour' WHERE accepted_at IS NULL`)
	require.NoError(t, err)

	resp = env.DoRequest(t, "POST", "/api/auth/accept-invitation", map[string]string{
		"token":    token,
		"password": "somepassword123",
	}, nil)
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
	resp.Body.Close()
}

func TestAcceptInvitation_StaleAfterRevoke(t *testing.T) {
	resetDB(t)
	adminToken := env.SetupAdmin(t, "admin@test.com", "password123")

	resp := env.DoRequest(t, "POST", "/api/users/invite", map[string]string{
		"email": "stale@test.com",
		"role":  "member",
	}, testutil.AuthHeader(adminToken))
	require.Equal(t, http.StatusCreated, resp.StatusCode)
	inv := testutil.ReadJSON(t, resp)
	invID, _ := inv["id"].(string)

	token := extractInvitationToken(t, 0)

	// Admin revokes the invitation.
	resp = env.DoRequest(t, "DELETE", "/api/users/invitations/"+invID, nil, testutil.AuthHeader(adminToken))
	require.Equal(t, http.StatusNoContent, resp.StatusCode)
	resp.Body.Close()

	// Stale link is rejected.
	resp = env.DoRequest(t, "POST", "/api/auth/accept-invitation", map[string]string{
		"token":    token,
		"password": "somepassword123",
	}, nil)
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
	resp.Body.Close()
}
