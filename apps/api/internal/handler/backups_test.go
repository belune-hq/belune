package handler_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/belune-hq/belune/internal/testutil"
)

func TestListBackupRuns_EmptyOnFreshDB(t *testing.T) {
	resetDB(t)
	token := env.SetupAdmin(t, "admin@test.com", "password123")

	resp := env.DoRequest(t, "GET", "/api/backups", nil, testutil.AuthHeader(token))
	require.Equal(t, http.StatusOK, resp.StatusCode)

	result := testutil.ReadJSONArray(t, resp)
	assert.Empty(t, result)
}

func TestGetBackupStatus_DefaultState(t *testing.T) {
	resetDB(t)
	token := env.SetupAdmin(t, "admin@test.com", "password123")

	resp := env.DoRequest(t, "GET", "/api/backups/status", nil, testutil.AuthHeader(token))
	require.Equal(t, http.StatusOK, resp.StatusCode)

	result := testutil.ReadJSON(t, resp)
	assert.Nil(t, result["last_succeeded_at"])
	assert.Nil(t, result["last_attempted_at"])
	assert.NotNil(t, result["remote_enabled"])
	assert.NotNil(t, result["retention"])
}

func TestTriggerBackupRun_ReturnsAccepted(t *testing.T) {
	resetDB(t)
	token := env.SetupAdmin(t, "admin@test.com", "password123")

	resp := env.DoRequest(t, "POST", "/api/backups/run", nil, testutil.AuthHeader(token))
	require.Equal(t, http.StatusAccepted, resp.StatusCode)

	result := testutil.ReadJSON(t, resp)
	assert.Equal(t, "queued", result["status"])
}

func TestBackupEndpoints_RequireAdmin(t *testing.T) {
	resetDB(t)
	adminToken := env.SetupAdmin(t, "admin@test.com", "password123")

	// Create a non-admin member
	env.DoRequest(t, "POST", "/api/users", map[string]string{
		"email":    "member@test.com",
		"password": "password123",
		"role":     "member",
	}, testutil.AuthHeader(adminToken))
	loginResp := env.DoRequest(t, "POST", "/api/auth/login", map[string]string{
		"email":    "member@test.com",
		"password": "password123",
	}, map[string]string{})
	require.Equal(t, http.StatusOK, loginResp.StatusCode)
	memberToken := testutil.ReadJSON(t, loginResp)["token"].(string)

	assert.Equal(t, http.StatusForbidden, env.DoRequest(t, "GET", "/api/backups", nil, testutil.AuthHeader(memberToken)).StatusCode)
	assert.Equal(t, http.StatusForbidden, env.DoRequest(t, "GET", "/api/backups/status", nil, testutil.AuthHeader(memberToken)).StatusCode)
	assert.Equal(t, http.StatusForbidden, env.DoRequest(t, "POST", "/api/backups/run", nil, testutil.AuthHeader(memberToken)).StatusCode)
}

func TestTriggerBackupRun_ConflictWhenRunning(t *testing.T) {
	resetDB(t)
	token := env.SetupAdmin(t, "admin@test.com", "password123")

	// First trigger queues successfully (no running row yet).
	resp := env.DoRequest(t, "POST", "/api/backups/run", nil, testutil.AuthHeader(token))
	require.Equal(t, http.StatusAccepted, resp.StatusCode)

	// Simulate a 'running' record by directly inserting one via the DB.
	// The asynq task starts async, so we insert the record manually here
	// to test the guard logic directly without waiting for the worker.
	_, err := env.Pool.Exec(t.Context(), `INSERT INTO backup_runs (status) VALUES ('running')`)
	require.NoError(t, err)

	// Second trigger while one is running → 409.
	resp = env.DoRequest(t, "POST", "/api/backups/run", nil, testutil.AuthHeader(token))
	assert.Equal(t, http.StatusConflict, resp.StatusCode)
}

func TestBackupEndpoints_RequireAuth(t *testing.T) {
	resetDB(t)

	assert.Equal(t, http.StatusUnauthorized, env.DoRequest(t, "GET", "/api/backups", nil, map[string]string{}).StatusCode)
	assert.Equal(t, http.StatusUnauthorized, env.DoRequest(t, "GET", "/api/backups/status", nil, map[string]string{}).StatusCode)
	assert.Equal(t, http.StatusUnauthorized, env.DoRequest(t, "POST", "/api/backups/run", nil, map[string]string{}).StatusCode)
	assert.Equal(t, http.StatusUnauthorized, env.DoRequest(t, "POST", "/api/backups/remote/test", nil, map[string]string{}).StatusCode)
}

// stubS3Endpoint answers every request 404, which minio reads as "bucket does
// not exist" — a real, immediate S3 round trip with no retry backoff, so the
// test proves the handler dialled the POSTED endpoint without depending on a
// container or on how long a refused dial takes to give up.
func stubS3Endpoint(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)
	return strings.TrimPrefix(srv.URL, "http://")
}

func TestTestBackupRemoteParams_TestsUnsavedValuesWhileDisabled(t *testing.T) {
	resetDB(t)
	token := env.SetupAdmin(t, "admin@test.com", "password123")

	// Baseline: nothing is saved and remote storage is off, so the
	// saved-config diagnostic refuses outright — the bug this endpoint fixes.
	saved := env.DoRequest(t, "POST", "/api/backups/test", nil, testutil.AuthHeader(token))
	require.Equal(t, http.StatusBadRequest, saved.StatusCode)
	require.Contains(t, testutil.ReadJSON(t, saved)["error"], "remote storage is disabled")

	resp := env.DoRequest(t, "POST", "/api/backups/remote/test", map[string]any{
		"endpoint":   stubS3Endpoint(t),
		"region":     "us-east-1",
		"bucket":     "belune-test",
		"prefix":     "belune/",
		"use_ssl":    false,
		"access_key": "key",
		"secret_key": "secret",
	}, testutil.AuthHeader(token))
	require.Equal(t, http.StatusOK, resp.StatusCode)

	body := testutil.ReadJSON(t, resp)
	assert.Equal(t, false, body["ok"])
	assert.NotContains(t, body["error"], "remote storage is disabled")
	assert.Contains(t, body["error"], "does not exist or is not accessible")
}

func TestTestBackupRemoteParams_RequiresBucket(t *testing.T) {
	resetDB(t)
	token := env.SetupAdmin(t, "admin@test.com", "password123")

	resp := env.DoRequest(t, "POST", "/api/backups/remote/test", map[string]any{
		"endpoint": stubS3Endpoint(t),
		"bucket":   "   ",
	}, testutil.AuthHeader(token))
	require.Equal(t, http.StatusBadRequest, resp.StatusCode)
	assert.Contains(t, testutil.ReadJSON(t, resp)["error"], "bucket is required")
}

func TestTestBackupRemoteParams_RequiresAdmin(t *testing.T) {
	resetDB(t)
	adminToken := env.SetupAdmin(t, "admin@test.com", "password123")

	env.DoRequest(t, "POST", "/api/users", map[string]string{
		"email":    "member@test.com",
		"password": "password123",
		"role":     "member",
	}, testutil.AuthHeader(adminToken))
	loginResp := env.DoRequest(t, "POST", "/api/auth/login", map[string]string{
		"email":    "member@test.com",
		"password": "password123",
	}, map[string]string{})
	require.Equal(t, http.StatusOK, loginResp.StatusCode)
	memberToken := testutil.ReadJSON(t, loginResp)["token"].(string)

	resp := env.DoRequest(t, "POST", "/api/backups/remote/test", map[string]any{
		"bucket": "belune-test",
	}, testutil.AuthHeader(memberToken))
	assert.Equal(t, http.StatusForbidden, resp.StatusCode)
}
