package handler_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/belune-hq/belune/internal/runtime"
	"github.com/belune-hq/belune/internal/server"
	"github.com/belune-hq/belune/internal/service"
	"github.com/belune-hq/belune/internal/terminal"
	"github.com/belune-hq/belune/internal/testutil"
)

// auditedServerURL serves the same database through a second server whose audit
// service is real. The shared harness wires h.auditSvc as nil on purpose, so
// this is the only way to see the rows a handler writes.
func auditedServerURL(t *testing.T) string {
	t.Helper()
	auditSvc := service.NewAuditService(env.Queries)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go auditSvc.Run(ctx)

	srv := server.New(env.Config, env.Pool, env.Queries, env.Asynq, env.Inspector,
		runtime.NewLocalRuntimes(env.Runtime), env.Proxy, env.Reconciler, env.Redis,
		nil, auditSvc, nil, terminal.NewManager(2), nil)
	ts := httptest.NewServer(srv.Router())
	t.Cleanup(ts.Close)
	return ts.URL
}

func putJSON(t *testing.T, url, token string, body any) {
	t.Helper()
	raw, err := json.Marshal(body)
	require.NoError(t, err)
	req, err := http.NewRequest("PUT", url, bytes.NewReader(raw))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	for k, v := range testutil.AuthHeader(token) {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
}

type envAudit struct {
	Created, Updated, Removed []string
	Raw                       string
}

func awaitEnvAudit(t *testing.T, resourceType, resourceID string) envAudit {
	t.Helper()
	var raw []byte
	require.Eventually(t, func() bool {
		return env.Pool.QueryRow(context.Background(),
			`SELECT details FROM audit_logs WHERE action = 'update_env_vars' AND resource_type = $1 AND resource_id = $2`,
			resourceType, resourceID).Scan(&raw) == nil
	}, 2*time.Second, 10*time.Millisecond, "replacing a variable set must write an audit row")
	var d struct {
		Created []string `json:"created"`
		Updated []string `json:"updated"`
		Removed []string `json:"removed"`
	}
	require.NoError(t, json.Unmarshal(raw, &d))
	return envAudit{Created: d.Created, Updated: d.Updated, Removed: d.Removed, Raw: string(raw)}
}

// Belune recorded that someone LOOKED at a secret and nothing when someone
// replaced the whole set. A replace is also the only way a variable is ever
// deleted, so the entry names what was removed.
func TestUpdateEnvVars_IsAuditedWithKeyNamesNeverValues(t *testing.T) {
	resetDB(t)
	token := env.SetupAdmin(t, "env-audit@test.com", "password123")
	projectID := extractID(env.CreateProject(t, token, "P", "p")["id"])
	appID := extractID(minimalApp(t, token, projectID)["id"])
	seed := env.DoRequest(t, "PUT", "/api/projects/"+projectID+"/applications/"+appID+"/env", map[string]any{
		"vars": []map[string]any{
			{"key": "KEEP", "value": "kept-plain", "is_secret": false},
			{"key": "ROTATE", "value": "old-secret", "is_secret": true},
			{"key": "DROP", "value": "dropped-secret", "is_secret": true},
		},
	}, testutil.AuthHeader(token))
	require.Equal(t, http.StatusOK, seed.StatusCode)
	seed.Body.Close()
	base := auditedServerURL(t)

	putJSON(t, base+"/api/projects/"+projectID+"/applications/"+appID+"/env", token, map[string]any{
		"vars": []map[string]any{
			{"key": "KEEP", "value": "ignored-mask", "is_secret": false, "unchanged": true},
			{"key": "ROTATE", "value": "brand-new-secret-value", "is_secret": true},
			{"key": "ADDED", "value": "added-secret-value", "is_secret": true},
		},
	})

	got := awaitEnvAudit(t, "env_var", appID)
	assert.Equal(t, []string{"ADDED"}, got.Created)
	assert.Equal(t, []string{"ROTATE"}, got.Updated)
	assert.Equal(t, []string{"DROP"}, got.Removed, "the deletion must be on the record")
	for _, secret := range []string{"brand-new-secret-value", "added-secret-value", "old-secret", "dropped-secret", "kept-plain", "ignored-mask"} {
		assert.False(t, strings.Contains(got.Raw, secret), "the audit row must not contain %q", secret)
	}
}

func TestUpdateProjectEnvVars_IsAuditedWithKeyNamesNeverValues(t *testing.T) {
	resetDB(t)
	token := env.SetupAdmin(t, "proj-env-audit@test.com", "password123")
	projectID := extractID(env.CreateProject(t, token, "P", "p")["id"])
	base := auditedServerURL(t)

	putJSON(t, base+"/api/projects/"+projectID+"/env", token, map[string]any{
		"vars": []map[string]any{{"key": "SHARED_TOKEN", "value": "project-secret-value", "is_secret": true}},
	})

	got := awaitEnvAudit(t, "project_env_var", projectID)
	assert.Equal(t, []string{"SHARED_TOKEN"}, got.Created)
	assert.Empty(t, got.Removed)
	assert.False(t, strings.Contains(got.Raw, "project-secret-value"))
}
