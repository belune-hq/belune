package handler_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/belune-hq/belune/internal/testutil"
)

var env *testutil.TestEnv

func TestMain(m *testing.M) {
	pool, queries, teardown := testutil.SetupTestDB()
	env = testutil.SetupTestServer(pool, queries)
	code := m.Run()
	env.Server.Close()
	teardown()
	os.Exit(code)
}

func resetDB(t *testing.T) {
	t.Helper()
	err := testutil.TruncateAll(context.Background(), env.Pool)
	require.NoError(t, err)
	// Flush in-memory Redis so JWT blacklists and invalidated-after flags
	// don't leak between tests.
	if env.RedisSrv != nil {
		env.RedisSrv.FlushAll()
	}
	// Reset mock state
	env.Runtime.StopCalls = nil
	env.Runtime.RemoveCalls = nil
	env.Runtime.StartCalls = nil
	env.Runtime.CreateCalls = nil
	env.Proxy.AddedRoutes = nil
	env.Proxy.RemovedRoutes = nil
	env.Asynq.Tasks = nil
}

func TestSetup(t *testing.T) {
	resetDB(t)

	// GET setup should indicate setup is required
	resp := env.DoRequest(t, "GET", "/api/auth/setup", nil, nil)
	result := testutil.ReadJSON(t, resp)
	assert.Equal(t, true, result["setup_required"])

	// POST setup creates admin
	resp = env.DoRequest(t, "POST", "/api/auth/setup", map[string]string{
		"email":    "admin@test.com",
		"password": "password123",
	}, nil)
	assert.Equal(t, http.StatusCreated, resp.StatusCode)
	result = testutil.ReadJSON(t, resp)
	assert.Equal(t, "admin@test.com", result["email"])
	assert.Equal(t, "admin", result["role"])

	// GET setup should now say not required
	resp = env.DoRequest(t, "GET", "/api/auth/setup", nil, nil)
	result = testutil.ReadJSON(t, resp)
	assert.Equal(t, false, result["setup_required"])

	// POST setup again should return 409
	resp = env.DoRequest(t, "POST", "/api/auth/setup", map[string]string{
		"email":    "admin2@test.com",
		"password": "password123",
	}, nil)
	assert.Equal(t, http.StatusConflict, resp.StatusCode)
	resp.Body.Close()
}

func TestLogin(t *testing.T) {
	resetDB(t)
	env.SetupAdmin(t, "admin@test.com", "password123")

	// Correct credentials
	resp := env.DoRequest(t, "POST", "/api/auth/login", map[string]string{
		"email":    "admin@test.com",
		"password": "password123",
	}, nil)
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	result := testutil.ReadJSON(t, resp)
	assert.NotEmpty(t, result["token"])
	assert.NotNil(t, result["user"])

	// Wrong password
	resp = env.DoRequest(t, "POST", "/api/auth/login", map[string]string{
		"email":    "admin@test.com",
		"password": "wrongpassword",
	}, nil)
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
	resp.Body.Close()

	// Missing fields
	resp = env.DoRequest(t, "POST", "/api/auth/login", map[string]string{
		"email": "admin@test.com",
	}, nil)
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
	resp.Body.Close()
}

func TestMe(t *testing.T) {
	resetDB(t)
	token := env.SetupAdmin(t, "admin@test.com", "password123")

	// With token
	resp := env.DoRequest(t, "GET", "/api/auth/me", nil, testutil.AuthHeader(token))
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	result := testutil.ReadJSON(t, resp)
	assert.Equal(t, "admin@test.com", result["email"])
	assert.Equal(t, "admin", result["role"])

	// Without token
	resp = env.DoRequest(t, "GET", "/api/auth/me", nil, nil)
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
	resp.Body.Close()
}

func TestChangePassword(t *testing.T) {
	resetDB(t)
	token := env.SetupAdmin(t, "admin@test.com", "password123")

	// Valid password change
	resp := env.DoRequest(t, "PUT", "/api/auth/password", map[string]string{
		"current_password": "password123",
		"new_password":     "newpassword123",
	}, testutil.AuthHeader(token))
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	resp.Body.Close()

	// Login with new password should work
	newToken := env.LoginAs(t, "admin@test.com", "newpassword123")
	assert.NotEmpty(t, newToken)

	// Wrong current password
	resp = env.DoRequest(t, "PUT", "/api/auth/password", map[string]string{
		"current_password": "wrongpassword",
		"new_password":     "anotherpassword",
	}, testutil.AuthHeader(newToken))
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
	resp.Body.Close()

	// Short new password
	resp = env.DoRequest(t, "PUT", "/api/auth/password", map[string]string{
		"current_password": "newpassword123",
		"new_password":     "short",
	}, testutil.AuthHeader(newToken))
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
	resp.Body.Close()
}

func TestJWTExpiryHoursConfigured(t *testing.T) {
	resetDB(t)
	// v0.0.9-alpha: access tokens default to 1h (refresh tokens cover the
	// gap). The test server passes JWTExpiryHours=0 so the service applies
	// its 1h fallback.
	token := env.SetupAdmin(t, "admin@test.com", "password123")
	require.NotEmpty(t, token)

	parts := strings.Split(token, ".")
	require.Len(t, parts, 3, "JWT must have 3 parts")

	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	require.NoError(t, err)

	var claims map[string]any
	require.NoError(t, json.Unmarshal(payload, &claims))

	exp, ok := claims["exp"].(float64)
	require.True(t, ok, "exp claim must be present")

	expTime := time.Unix(int64(exp), 0)
	expectedExpiry := time.Now().Add(1 * time.Hour)

	assert.WithinDuration(t, expectedExpiry, expTime, 60*time.Second,
		"token expiry should be approximately 1 hour from now")
}
