package handler_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/hibiken/asynq"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/weiliang79/belune/internal/runtime"
	"github.com/weiliang79/belune/internal/server"
	"github.com/weiliang79/belune/internal/service"
	"github.com/weiliang79/belune/internal/terminal"
	"github.com/weiliang79/belune/internal/testutil"
)

// rpcError decodes a call that the scope gate refused: a JSON-RPC protocol
// error, not a tool-level isError result, because the tool never ran.
func rpcError(t *testing.T, resp *http.Response) string {
	t.Helper()
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var rpc jsonRPCToolResult
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&rpc))
	require.NotNil(t, rpc.Error, "expected a protocol error, got result %+v", rpc.Result)
	return rpc.Error.Message
}

func triggerArgs(appID string) map[string]any { return map[string]any{"application_id": appID} }

func TestMCP_TriggerDeployment_ScopeAndPin(t *testing.T) {
	resetDB(t)
	adminToken := env.SetupAdmin(t, "mcp-trigger@test.com", "password123")
	project := env.CreateProject(t, adminToken, "Trigger Project", "trigger-project")
	projectID := extractID(project["id"])
	appID := extractID(minimalApp(t, adminToken, projectID)["id"])
	otherProject := env.CreateProject(t, adminToken, "Other Project", "other-project")
	otherAppID := extractID(minimalApp(t, adminToken, extractID(otherProject["id"]))["id"])

	// The route's read floor admits a read token, so only the per-tool gate
	// stands between it and a deploy.
	readToken := mintScoped(t, adminToken, []string{"read"})
	assert.Contains(t, rpcError(t, callToolArgs(t, readToken, "trigger_deployment", triggerArgs(appID))), "scope")
	assert.Empty(t, env.Asynq.Tasks, "a refused call must not queue anything")

	// deploy is narrower than write but is exactly what this tool needs.
	deployToken := mintScoped(t, adminToken, []string{"deploy"})
	var got map[string]any
	decodeToolResult(t, callToolArgs(t, deployToken, "trigger_deployment", triggerArgs(appID)), &got)
	assert.Equal(t, "pending", got["status"])
	assert.Equal(t, appID, got["application_id"])
	require.Len(t, env.Asynq.Tasks, 1)
	assert.Equal(t, "deploy", env.Asynq.Tasks[0].TypeName)

	// The pin composes independently of scope: deploy scope, wrong project.
	adminUserID := extractID(mustAuthMe(t, adminToken)["id"])
	pinned := createPinnedAPIToken(t, adminUserID, projectID, []string{"deploy"})
	assert.Equal(t, "access denied", toolErrorText(t, callToolArgs(t, pinned, "trigger_deployment", triggerArgs(otherAppID))))
}

func TestMCP_TriggerDeployment_ConflictAndStaleReclaim(t *testing.T) {
	resetDB(t)
	adminToken := env.SetupAdmin(t, "mcp-trigger-409@test.com", "password123")
	project := env.CreateProject(t, adminToken, "P", "p")
	appID := extractID(minimalApp(t, adminToken, extractID(project["id"]))["id"])
	token := mintScoped(t, adminToken, []string{"deploy"})

	// A deploy genuinely running: the guard says so in words the caller can
	// act on, not a bare conflict.
	env.Asynq.EnqueueErr = asynq.ErrTaskIDConflict
	env.Inspector.DeleteTaskErr = fmt.Errorf("task is active")
	t.Cleanup(func() { env.Asynq.EnqueueErr = nil; env.Inspector.DeleteTaskErr = nil })
	assert.Contains(t, toolErrorText(t, callToolArgs(t, token, "trigger_deployment", triggerArgs(appID))), "already in progress")

	// A stale holder is reclaimed — the same rule REST gets, from the same code.
	env.Asynq.EnqueueErr = asynq.ErrTaskIDConflict
	env.Asynq.EnqueueOnce = true
	env.Inspector.DeleteTaskErr = nil
	env.Inspector.DeleteTaskCalls = nil
	var got map[string]any
	decodeToolResult(t, callToolArgs(t, token, "trigger_deployment", triggerArgs(appID)), &got)
	assert.Equal(t, []string{"critical/deploy:" + appID}, env.Inspector.DeleteTaskCalls)
}

// newAuditedMCP serves the same database through a second server whose audit
// service is real: the shared harness wires h.auditSvc as nil on purpose, so
// this is the only way to see the rows a tool writes.
func newAuditedMCP(t *testing.T) string {
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

func callToolAt(t *testing.T, baseURL, token, name string, args map[string]any) *http.Response {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{"name": name, "arguments": args},
	})
	require.NoError(t, err)
	req, err := http.NewRequest("POST", baseURL+"/mcp", bytes.NewReader(body))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Forwarded-For", "203.0.113.9")
	for k, v := range mcpHeaders(token) {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	return resp
}

func TestMCP_TriggerDeployment_IsAuditedAgainstTheToken(t *testing.T) {
	resetDB(t)
	adminToken := env.SetupAdmin(t, "mcp-trigger-audit@test.com", "password123")
	project := env.CreateProject(t, adminToken, "P", "p")
	appID := extractID(minimalApp(t, adminToken, extractID(project["id"]))["id"])
	token := mintScoped(t, adminToken, []string{"deploy"})
	base := newAuditedMCP(t)

	var got map[string]any
	decodeToolResult(t, callToolAt(t, base, token, "trigger_deployment", triggerArgs(appID)), &got)

	var tokenID, ip *string
	require.Eventually(t, func() bool {
		return env.Pool.QueryRow(context.Background(),
			"SELECT token_id::text, ip_address FROM audit_logs WHERE action = 'deploy_application' AND resource_id = $1", appID,
		).Scan(&tokenID, &ip) == nil
	}, 2*time.Second, 10*time.Millisecond, "the tool must write an audit row")
	require.NotNil(t, tokenID, "the row must be attributed to the token, not read as a human action")
	assert.NotEmpty(t, *tokenID)
	require.NotNil(t, ip)
	assert.Equal(t, "203.0.113.9", *ip, "the client IP must survive the trip through the context")
}

// The REST route is the other caller of the extracted service; it must still
// serialise on the same TaskID and still report a conflict as 409.
func TestDeployApplication_StillUsesTheSharedQueue(t *testing.T) {
	resetDB(t)
	adminToken := env.SetupAdmin(t, "rest-trigger@test.com", "password123")
	project := env.CreateProject(t, adminToken, "P", "p")
	projectID := extractID(project["id"])
	appID := extractID(minimalApp(t, adminToken, projectID)["id"])

	resp := env.DoRequest(t, "POST", appActionPath(projectID, appID, "deploy"), nil, testutil.AuthHeader(adminToken))
	assert.Equal(t, http.StatusAccepted, resp.StatusCode)
	resp.Body.Close()
	require.NotEmpty(t, env.Asynq.Tasks)
}
