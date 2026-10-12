package handler_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/weiliang79/belune/internal/testutil"
)

type envRow struct {
	Encrypted []byte
	IsSecret  bool
	UpdatedAt time.Time
}

// envRows reads every stored row straight from the table, so assertions are on
// what was persisted, not on what a list endpoint chose to show.
func envRows(t *testing.T, appID string) map[string]envRow {
	t.Helper()
	rows, err := env.Pool.Query(context.Background(),
		`SELECT key, value_encrypted, is_secret, updated_at FROM env_vars WHERE application_id = $1`, appID)
	require.NoError(t, err)
	defer rows.Close()
	out := map[string]envRow{}
	for rows.Next() {
		var key string
		var r envRow
		require.NoError(t, rows.Scan(&key, &r.Encrypted, &r.IsSecret, &r.UpdatedAt))
		out[key] = r
	}
	require.NoError(t, rows.Err())
	return out
}

func envValue(t *testing.T, r envRow) string {
	t.Helper()
	plain, err := env.Config.Keyring.Decrypt(r.Encrypted)
	require.NoError(t, err)
	return string(plain)
}

// seedEnv writes through the real REST replace endpoint, so the fixture is
// whatever production would have stored.
func seedEnv(t *testing.T, token, projectID, appID string, vars ...map[string]any) {
	t.Helper()
	resp := env.DoRequest(t, "PUT", fmt.Sprintf("/api/projects/%s/applications/%s/env", projectID, appID),
		map[string]any{"vars": vars}, testutil.AuthHeader(token))
	require.Equal(t, 200, resp.StatusCode)
	resp.Body.Close()
}

func envApp(t *testing.T, adminToken string) (projectID, appID string) {
	t.Helper()
	project := env.CreateProject(t, adminToken, "P", "p")
	projectID = extractID(project["id"])
	appID = extractID(minimalApp(t, adminToken, projectID)["id"])
	seedEnv(t, adminToken, projectID, appID,
		map[string]any{"key": "DATABASE_URL", "value": "postgres://db/prod", "is_secret": false},
		map[string]any{"key": "API_KEY", "value": "sk-live-original", "is_secret": true},
		map[string]any{"key": "STRIPE_SECRET", "value": "stripe-original", "is_secret": true},
	)
	return projectID, appID
}

func setEnvArgs(appID string, vars ...map[string]any) map[string]any {
	return map[string]any{"application_id": appID, "variables": vars}
}

// The destructive trap: the REST endpoint this sits beside replaces the whole
// set. Setting ONE variable must leave every other row untouched — same
// ciphertext bytes (encryption is randomised, so identical bytes prove the row
// was never rewritten, not merely rewritten to the same value) and same
// updated_at — and must delete nothing.
func TestMCP_SetEnvVars_MergesAndNeverTouchesOrDeletesTheRest(t *testing.T) {
	resetDB(t)
	adminToken := env.SetupAdmin(t, "mcp-env@test.com", "password123")
	_, appID := envApp(t, adminToken)
	token := mintScoped(t, adminToken, []string{"write"})
	before := envRows(t, appID)
	require.Len(t, before, 3)

	var got struct {
		Variables []struct {
			Key      string `json:"key"`
			IsSecret bool   `json:"is_secret"`
			Created  bool   `json:"created"`
		} `json:"variables"`
	}
	decodeToolResult(t, callToolArgs(t, token, "set_application_env_vars", setEnvArgs(appID,
		map[string]any{"key": "DATABASE_URL", "value": "postgres://db/staging"})), &got)
	require.Len(t, got.Variables, 1)
	assert.False(t, got.Variables[0].Created)

	after := envRows(t, appID)
	require.Len(t, after, 3, "nothing may be deleted")
	assert.Equal(t, "postgres://db/staging", envValue(t, after["DATABASE_URL"]))
	for _, k := range []string{"API_KEY", "STRIPE_SECRET"} {
		assert.Equal(t, before[k], after[k], "%s was not listed and must be byte-identical", k)
	}
	assert.Equal(t, "sk-live-original", envValue(t, after["API_KEY"]))
	assert.False(t, after["DATABASE_URL"].IsSecret, "an existing variable keeps its secret setting when none is passed")
}

func TestMCP_SetEnvVars_NewKeyDefaultsToSecretAndExplicitFalseIsHonoured(t *testing.T) {
	resetDB(t)
	adminToken := env.SetupAdmin(t, "mcp-env-new@test.com", "password123")
	_, appID := envApp(t, adminToken)
	token := mintScoped(t, adminToken, []string{"write"})

	decodeToolResult(t, callToolArgs(t, token, "set_application_env_vars", setEnvArgs(appID,
		map[string]any{"key": "NEW_TOKEN", "value": "abc"},
		map[string]any{"key": "LOG_LEVEL", "value": "debug", "is_secret": false},
	)), &struct{}{})

	rows := envRows(t, appID)
	assert.True(t, rows["NEW_TOKEN"].IsSecret, "an unlabelled new variable must fail closed to secret")
	assert.False(t, rows["LOG_LEVEL"].IsSecret)
	require.Len(t, rows, 5)

	// An explicit flag on an existing key is honoured over the stored one.
	decodeToolResult(t, callToolArgs(t, token, "set_application_env_vars", setEnvArgs(appID,
		map[string]any{"key": "DATABASE_URL", "value": "postgres://db/prod", "is_secret": true})), &struct{}{})
	assert.True(t, envRows(t, appID)["DATABASE_URL"].IsSecret)
}

func TestMCP_SetEnvVars_StampsTheConfigChangeMarker(t *testing.T) {
	resetDB(t)
	adminToken := env.SetupAdmin(t, "mcp-env-marker@test.com", "password123")
	_, appID := envApp(t, adminToken)
	token := mintScoped(t, adminToken, []string{"write"})
	_, err := env.Pool.Exec(context.Background(), `UPDATE applications SET config_changed_at = NULL WHERE id = $1`, appID)
	require.NoError(t, err)

	decodeToolResult(t, callToolArgs(t, token, "set_application_env_vars", setEnvArgs(appID,
		map[string]any{"key": "LOG_LEVEL", "value": "debug"})), &struct{}{})

	config, _ := markerState(t, appID)
	assert.True(t, config, "the running container is stale until redeployed, and the dashboard must say so")
}

// One bad entry among good ones writes NOTHING.
func TestMCP_SetEnvVars_InvalidInputWritesNothing(t *testing.T) {
	resetDB(t)
	adminToken := env.SetupAdmin(t, "mcp-env-invalid@test.com", "password123")
	_, appID := envApp(t, adminToken)
	token := mintScoped(t, adminToken, []string{"write"})
	before := envRows(t, appID)

	for name, tc := range map[string]struct {
		vars []map[string]any
		want string
	}{
		"bad key after a good one": {[]map[string]any{{"key": "GOOD", "value": "1"}, {"key": "1BAD", "value": "2"}}, "invalid env var key"},
		"duplicate key":            {[]map[string]any{{"key": "DUP", "value": "1"}, {"key": "DUP", "value": "2"}}, "duplicate env var key"},
		"empty key":                {[]map[string]any{{"key": "", "value": "1"}}, "invalid env var key"},
		"nothing given":            {[]map[string]any{}, "no variables given"},
	} {
		t.Run(name, func(t *testing.T) {
			assert.Contains(t, toolErrorText(t, callToolArgs(t, token, "set_application_env_vars", setEnvArgs(appID, tc.vars...))), tc.want)
			assert.Equal(t, before, envRows(t, appID), "a refused call must not touch any row")
		})
	}

	tooMany := make([]map[string]any, 101)
	for i := range tooMany {
		tooMany[i] = map[string]any{"key": fmt.Sprintf("K%d", i), "value": "v"}
	}
	assert.Contains(t, toolErrorText(t, callToolArgs(t, token, "set_application_env_vars", setEnvArgs(appID, tooMany...))), "too many")
	assert.Equal(t, before, envRows(t, appID))
}

func TestMCP_SetEnvVars_ScopeAndPin(t *testing.T) {
	resetDB(t)
	adminToken := env.SetupAdmin(t, "mcp-env-scope@test.com", "password123")
	projectID, appID := envApp(t, adminToken)
	otherProject := env.CreateProject(t, adminToken, "Other", "other")
	otherApp := extractID(minimalApp(t, adminToken, extractID(otherProject["id"]))["id"])
	before := envRows(t, appID)

	for _, scope := range []string{"read", "deploy"} {
		tok := mintScoped(t, adminToken, []string{scope})
		assert.Equal(t, `the set_application_env_vars tool requires the "write" scope; this token has: `+scope,
			rpcError(t, callToolArgs(t, tok, "set_application_env_vars", setEnvArgs(appID, map[string]any{"key": "X", "value": "1"}))))
	}
	assert.Equal(t, before, envRows(t, appID))

	adminUserID := extractID(mustAuthMe(t, adminToken)["id"])
	pinned := createPinnedAPIToken(t, adminUserID, projectID, []string{"write"})
	assert.Equal(t, "access denied", toolErrorText(t, callToolArgs(t, pinned, "set_application_env_vars",
		setEnvArgs(otherApp, map[string]any{"key": "X", "value": "1"}))))
	assert.Empty(t, envRows(t, otherApp))
}

// The audit log is the operator's record of what the assistant did, so it names
// the keys — and must not become a second copy of the secrets.
func TestMCP_SetEnvVars_AuditNamesKeysNeverValues(t *testing.T) {
	resetDB(t)
	adminToken := env.SetupAdmin(t, "mcp-env-audit@test.com", "password123")
	_, appID := envApp(t, adminToken)
	token := mintScoped(t, adminToken, []string{"write"})
	base := newAuditedMCP(t)
	const secretValue = "sk-live-VERY-SECRET-9f8e7d"

	decodeToolResult(t, callToolAt(t, base, token, "set_application_env_vars", setEnvArgs(appID,
		map[string]any{"key": "PAYMENT_KEY", "value": secretValue},
		map[string]any{"key": "DATABASE_URL", "value": "postgres://db/other"})), &struct{}{})

	var tokenID *string
	var details string
	require.Eventually(t, func() bool {
		return env.Pool.QueryRow(context.Background(),
			"SELECT token_id::text, details::text FROM audit_logs WHERE action = 'update_env_vars' AND resource_type = 'env_var' AND resource_id = $1", appID,
		).Scan(&tokenID, &details) == nil
	}, 2*time.Second, 10*time.Millisecond, "the tool must write an audit row")
	require.NotNil(t, tokenID, "attributed to the token")
	var d struct {
		Created, Updated, Removed []string
	}
	require.NoError(t, json.Unmarshal([]byte(details), &d))
	assert.Equal(t, []string{"PAYMENT_KEY"}, d.Created)
	assert.Equal(t, []string{"DATABASE_URL"}, d.Updated)
	assert.Empty(t, d.Removed, "a merge never removes")
	assert.False(t, strings.Contains(details, secretValue), "the audit row must not contain the value")
}

// The tool's result echoes keys and flags, never a value.
func TestMCP_SetEnvVars_ResultNeverEchoesValues(t *testing.T) {
	resetDB(t)
	adminToken := env.SetupAdmin(t, "mcp-env-echo@test.com", "password123")
	_, appID := envApp(t, adminToken)
	token := mintScoped(t, adminToken, []string{"write"})

	resp := callToolArgs(t, token, "set_application_env_vars", setEnvArgs(appID,
		map[string]any{"key": "PAYMENT_KEY", "value": "sk-live-echo-check", "is_secret": false}))
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	assert.Contains(t, string(body), "PAYMENT_KEY", "the key is reported back")
	assert.NotContains(t, string(body), "sk-live-echo-check")
}
