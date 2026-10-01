package handler_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func lifecycleApp(t *testing.T, adminToken string) (projectID, appID string) {
	t.Helper()
	project := env.CreateProject(t, adminToken, "P", "p")
	projectID = extractID(project["id"])
	appID = extractID(minimalApp(t, adminToken, projectID)["id"])
	env.Runtime.StopCalls, env.Runtime.StartCalls = nil, nil
	return projectID, appID
}

func TestMCP_StopAndStartApplication(t *testing.T) {
	resetDB(t)
	adminToken := env.SetupAdmin(t, "mcp-lifecycle@test.com", "password123")
	_, appID := lifecycleApp(t, adminToken)
	token := mintScoped(t, adminToken, []string{"write"})

	var got struct {
		ApplicationID string `json:"application_id"`
		Status        string `json:"status"`
	}
	decodeToolResult(t, callToolArgs(t, token, "stop_application", triggerArgs(appID)), &got)
	assert.Equal(t, "stopped", got.Status)
	assert.Equal(t, appID, got.ApplicationID)
	require.Len(t, env.Runtime.StopCalls, 1)
	assert.Empty(t, env.Runtime.StartCalls, "stop must not start anything")
	assert.Equal(t, "stopped", getApp(t, appID).Status)

	decodeToolResult(t, callToolArgs(t, token, "start_application", triggerArgs(appID)), &got)
	assert.Equal(t, "running", got.Status)
	require.Len(t, env.Runtime.StartCalls, 1)
	assert.Equal(t, "running", getApp(t, appID).Status)

	// Neither queues a build: operating on what exists is not a deploy.
	assert.Empty(t, env.Asynq.Tasks)
}

// start/stop need write, NOT deploy — a user decision. deploy stays narrowly
// "cause a deployment", so a CI token that can deploy cannot take an app
// offline.
func TestMCP_StartStop_RequireWriteNotDeploy(t *testing.T) {
	resetDB(t)
	adminToken := env.SetupAdmin(t, "mcp-lifecycle-scope@test.com", "password123")
	projectID, appID := lifecycleApp(t, adminToken)
	otherProject := env.CreateProject(t, adminToken, "Other", "other")
	otherApp := extractID(minimalApp(t, adminToken, extractID(otherProject["id"]))["id"])
	statusBefore := getApp(t, appID).Status

	for _, tool := range []string{"stop_application", "start_application"} {
		for _, scope := range []string{"read", "deploy"} {
			tok := mintScoped(t, adminToken, []string{scope})
			assert.Equal(t, "the "+tool+` tool requires the "write" scope; this token has: `+scope,
				rpcError(t, callToolArgs(t, tok, tool, triggerArgs(appID))))
		}
	}
	assert.Empty(t, env.Runtime.StopCalls, "a refused call must not touch a container")
	assert.Empty(t, env.Runtime.StartCalls)
	assert.Equal(t, statusBefore, getApp(t, appID).Status, "nor change the recorded status")

	adminUserID := extractID(mustAuthMe(t, adminToken)["id"])
	pinned := createPinnedAPIToken(t, adminUserID, projectID, []string{"write"})
	for _, tool := range []string{"stop_application", "start_application"} {
		assert.Equal(t, "access denied", toolErrorText(t, callToolArgs(t, pinned, tool, triggerArgs(otherApp))))
	}
	assert.Empty(t, env.Runtime.StopCalls)
	assert.Empty(t, env.Runtime.StartCalls)
}

func TestMCP_StartStop_AreAuditedAgainstTheToken(t *testing.T) {
	resetDB(t)
	adminToken := env.SetupAdmin(t, "mcp-lifecycle-audit@test.com", "password123")
	_, appID := lifecycleApp(t, adminToken)
	token := mintScoped(t, adminToken, []string{"write"})
	base := newAuditedMCP(t)

	var got map[string]any
	decodeToolResult(t, callToolAt(t, base, token, "stop_application", triggerArgs(appID)), &got)
	decodeToolResult(t, callToolAt(t, base, token, "start_application", triggerArgs(appID)), &got)

	for _, action := range []string{"stop_application", "start_application"} {
		var tokenID *string
		require.Eventually(t, func() bool {
			return env.Pool.QueryRow(context.Background(),
				"SELECT token_id::text FROM audit_logs WHERE action = $1 AND resource_id = $2", action, appID,
			).Scan(&tokenID) == nil
		}, 2*time.Second, 10*time.Millisecond, "%s must write an audit row", action)
		require.NotNil(t, tokenID, "%s must be attributed to the token", action)
	}
}
