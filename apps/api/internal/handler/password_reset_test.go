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

func TestForgotPassword_AlwaysReturns200(t *testing.T) {
	resetDB(t)
	env.SetupAdmin(t, "admin@test.com", "password123")

	// Known email — must return 200 with no enumeration.
	resp := env.DoRequest(t, "POST", "/api/auth/forgot-password", map[string]string{
		"email": "admin@test.com",
	}, nil)
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	result := testutil.ReadJSON(t, resp)
	assert.Contains(t, result["status"], "if the email exists")

	// Unknown email — must also return 200.
	resp = env.DoRequest(t, "POST", "/api/auth/forgot-password", map[string]string{
		"email": "nonexistent@test.com",
	}, nil)
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	result = testutil.ReadJSON(t, resp)
	assert.Contains(t, result["status"], "if the email exists")
}

func TestForgotPassword_MissingEmail(t *testing.T) {
	resetDB(t)
	resp := env.DoRequest(t, "POST", "/api/auth/forgot-password", map[string]string{}, nil)
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
	resp.Body.Close()
}

func TestForgotPassword_EnqueuesEmailTask(t *testing.T) {
	resetDB(t)
	env.SetupAdmin(t, "admin@test.com", "password123")

	resp := env.DoRequest(t, "POST", "/api/auth/forgot-password", map[string]string{
		"email": "admin@test.com",
	}, nil)
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	resp.Body.Close()

	require.Len(t, env.Asynq.Tasks, 1, "expected one email task to be enqueued")
	assert.Equal(t, "email:send", env.Asynq.Tasks[0].TypeName)
}

func TestForgotPassword_InvalidatesPriorTokens(t *testing.T) {
	resetDB(t)
	env.SetupAdmin(t, "admin@test.com", "password123")

	// First request — creates a token.
	resp := env.DoRequest(t, "POST", "/api/auth/forgot-password", map[string]string{
		"email": "admin@test.com",
	}, nil)
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	resp.Body.Close()

	firstTaskCount := len(env.Asynq.Tasks)

	// Second request — invalidates prior token, creates a new one.
	resp = env.DoRequest(t, "POST", "/api/auth/forgot-password", map[string]string{
		"email": "admin@test.com",
	}, nil)
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	resp.Body.Close()

	assert.Greater(t, len(env.Asynq.Tasks), firstTaskCount, "second request should enqueue a second email task")
}

func TestResetPassword_InvalidToken(t *testing.T) {
	resetDB(t)

	resp := env.DoRequest(t, "POST", "/api/auth/reset-password", map[string]string{
		"token":        "deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef",
		"new_password": "newpassword123",
	}, nil)
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
	resp.Body.Close()
}

func TestResetPassword_MissingFields(t *testing.T) {
	resetDB(t)

	// Missing new_password.
	resp := env.DoRequest(t, "POST", "/api/auth/reset-password", map[string]string{
		"token": "sometoken",
	}, nil)
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
	resp.Body.Close()

	// Missing token.
	resp = env.DoRequest(t, "POST", "/api/auth/reset-password", map[string]string{
		"new_password": "newpassword123",
	}, nil)
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
	resp.Body.Close()
}

func TestResetPassword_ShortPassword(t *testing.T) {
	resetDB(t)

	resp := env.DoRequest(t, "POST", "/api/auth/reset-password", map[string]string{
		"token":        "sometoken",
		"new_password": "short",
	}, nil)
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
	resp.Body.Close()
}

// extractTokenFromTask parses the plaintext reset token from the email task payload.
func extractTokenFromTask(t *testing.T, taskIndex int) string {
	t.Helper()
	require.Greater(t, len(env.Asynq.Tasks), taskIndex, "expected email task at index %d", taskIndex)
	var payload worker.EmailSendPayload
	require.NoError(t, json.Unmarshal(env.Asynq.Tasks[taskIndex].Payload, &payload))
	vars, ok := payload.Vars.(map[string]any)
	require.True(t, ok, "payload Vars should decode as map[string]any")
	resetURL, _ := vars["ResetURL"].(string)
	require.NotEmpty(t, resetURL, "ResetURL must be present in email vars")
	parsed, err := url.Parse(resetURL)
	require.NoError(t, err)
	token := parsed.Query().Get("token")
	require.NotEmpty(t, token, "token query param must be present in ResetURL")
	return token
}

// TestPasswordResetFullFlow exercises the complete forgot → reset → login path.
func TestPasswordResetFullFlow(t *testing.T) {
	resetDB(t)
	env.SetupAdmin(t, "user@test.com", "oldpassword123")

	// Step 1: request reset.
	resp := env.DoRequest(t, "POST", "/api/auth/forgot-password", map[string]string{
		"email": "user@test.com",
	}, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	resp.Body.Close()

	// Step 2: extract plaintext token from enqueued email task.
	token := extractTokenFromTask(t, 0)

	// Step 3: reset to a new password.
	resp = env.DoRequest(t, "POST", "/api/auth/reset-password", map[string]string{
		"token":        token,
		"new_password": "newpassword123",
	}, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	result := testutil.ReadJSON(t, resp)
	assert.Contains(t, result["status"], "reset successful")

	// Step 4: old password is rejected.
	loginResp := env.DoRequest(t, "POST", "/api/auth/login", map[string]string{
		"email":    "user@test.com",
		"password": "oldpassword123",
	}, nil)
	assert.Equal(t, http.StatusUnauthorized, loginResp.StatusCode)
	loginResp.Body.Close()

	// Step 5: new password works.
	newJWT := env.LoginAs(t, "user@test.com", "newpassword123")
	assert.NotEmpty(t, newJWT)
}

// TestResetPassword_DoubleUse verifies that a reset token can only be used once.
func TestResetPassword_DoubleUse(t *testing.T) {
	resetDB(t)
	env.SetupAdmin(t, "user2@test.com", "oldpassword123")

	resp := env.DoRequest(t, "POST", "/api/auth/forgot-password", map[string]string{
		"email": "user2@test.com",
	}, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	resp.Body.Close()

	token := extractTokenFromTask(t, 0)

	// First use: succeeds.
	resp = env.DoRequest(t, "POST", "/api/auth/reset-password", map[string]string{
		"token":        token,
		"new_password": "newpassword456",
	}, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	resp.Body.Close()

	// Second use: rejected.
	resp = env.DoRequest(t, "POST", "/api/auth/reset-password", map[string]string{
		"token":        token,
		"new_password": "anotherpassword",
	}, nil)
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
	resp.Body.Close()
}

// TestResetPassword_ExpiredToken verifies that an expired token is rejected.
func TestResetPassword_ExpiredToken(t *testing.T) {
	resetDB(t)
	env.SetupAdmin(t, "user3@test.com", "oldpassword123")

	resp := env.DoRequest(t, "POST", "/api/auth/forgot-password", map[string]string{
		"email": "user3@test.com",
	}, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	resp.Body.Close()

	token := extractTokenFromTask(t, 0)

	// Force-expire the token directly in the DB.
	ctx := t.Context()
	_, err := env.Pool.Exec(ctx, `UPDATE password_reset_tokens SET expires_at = NOW() - INTERVAL '1 hour' WHERE used_at IS NULL`)
	require.NoError(t, err)

	resp = env.DoRequest(t, "POST", "/api/auth/reset-password", map[string]string{
		"token":        token,
		"new_password": "newpassword789",
	}, nil)
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
	resp.Body.Close()
}
