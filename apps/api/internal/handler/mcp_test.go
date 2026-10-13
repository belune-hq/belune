package handler_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/weiliang79/belune/internal/store/generated"
	"github.com/weiliang79/belune/internal/testutil"
)

// mcpHeaders builds the headers a streamable-HTTP MCP client sends: a Bearer
// token plus an Accept header advertising both response forms — the
// stateless transport rejects a POST missing either, per the streamable HTTP
// spec, regardless of which one the server actually replies with.
func mcpHeaders(token string) map[string]string {
	h := testutil.AuthHeader(token)
	h["Accept"] = "application/json, text/event-stream"
	return h
}

// callToolArgs issues one JSON-RPC tools/call request and returns the raw
// HTTP response — callers decode it differently depending on whether they
// expect a JSON-RPC envelope (success or tool-level error) or the plain
// {"error":...} body a middleware rejection writes before the request ever
// reaches the MCP server.
func callToolArgs(t *testing.T, token, name string, args map[string]any) *http.Response {
	t.Helper()
	if args == nil {
		args = map[string]any{}
	}
	return env.DoRequest(t, "POST", "/mcp", map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "tools/call",
		"params": map[string]any{
			"name":      name,
			"arguments": args,
		},
	}, mcpHeaders(token))
}

func callTool(t *testing.T, token, name string) *http.Response {
	t.Helper()
	return callToolArgs(t, token, name, nil)
}

type jsonRPCToolResult struct {
	Result *struct {
		IsError bool `json:"isError"`
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	} `json:"result"`
	Error *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// decodeToolResult asserts the call succeeded at both the transport level
// (200, no JSON-RPC protocol error) and the tool level (IsError false), then
// unmarshals its sole text content block into target.
func decodeToolResult(t *testing.T, resp *http.Response, target any) {
	t.Helper()
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	var rpc jsonRPCToolResult
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&rpc))
	require.Nil(t, rpc.Error, "tools/call returned a JSON-RPC protocol error: %+v", rpc.Error)
	require.NotNil(t, rpc.Result)
	require.False(t, rpc.Result.IsError, "tool call reported an error: %+v", rpc.Result.Content)
	require.Len(t, rpc.Result.Content, 1)
	require.NoError(t, json.Unmarshal([]byte(rpc.Result.Content[0].Text), target))
}

// toolErrorText asserts the call reached the tool and the tool itself
// reported an error (IsError), returning that error's text.
func toolErrorText(t *testing.T, resp *http.Response) string {
	t.Helper()
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	var rpc jsonRPCToolResult
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&rpc))
	require.Nil(t, rpc.Error, "tools/call returned a JSON-RPC protocol error: %+v", rpc.Error)
	require.NotNil(t, rpc.Result)
	require.True(t, rpc.Result.IsError, "expected a tool-level error, got: %+v", rpc.Result.Content)
	require.Len(t, rpc.Result.Content, 1)
	return rpc.Result.Content[0].Text
}

// TestMCP_ReadScopedTokenCanListProjects is the PR1 checkpoint: transport,
// auth and one tool all wired correctly end to end.
func TestMCP_ReadScopedTokenCanListProjects(t *testing.T) {
	resetDB(t)
	adminToken := env.SetupAdmin(t, "mcp-admin@test.com", "password123")
	project := env.CreateProject(t, adminToken, "MCP Project", "mcp-project")
	projectID := extractID(project["id"])

	readToken := mintScoped(t, adminToken, []string{"read"})

	var projects []map[string]any
	decodeToolResult(t, callTool(t, readToken, "list_projects"), &projects)
	require.Len(t, projects, 1)
	assert.Equal(t, projectID, projects[0]["id"])
	assert.Equal(t, "mcp-project", projects[0]["slug"])
}

// TestMCP_ScopeEnforced asserts the trap the build plan called out twice:
// RequireScopeByMethod would have demanded "write" for this POST, the exact
// inverse of a read-only server. A metrics-only token (the one scope that
// does NOT satisfy "read" in the lattice) must be rejected before the
// request ever reaches the MCP server.
func TestMCP_ScopeEnforced(t *testing.T) {
	resetDB(t)
	adminToken := env.SetupAdmin(t, "mcp-scope@test.com", "password123")

	metricsToken := mintScoped(t, adminToken, []string{"metrics"})
	resp := callTool(t, metricsToken, "list_projects")
	defer resp.Body.Close()
	assert.Equal(t, http.StatusForbidden, resp.StatusCode)
	body := testutil.ReadJSON(t, resp)
	assert.Equal(t, "token lacks required scope: read", body["error"])
}

// TestMCP_SessionRejected pins the design decision that /mcp accepts only a
// personal access token: MCP clients are machines, and accepting a session
// JWT there would blur the boundary the PAT model draws (and would skip
// CSRF, which already exempts the Bearer branch, with no compensating
// control). A session JWT authenticates fine everywhere else, so this must
// be rejected structurally, not merely by scope.
func TestMCP_SessionRejected(t *testing.T) {
	resetDB(t)
	adminToken := env.SetupAdmin(t, "mcp-session@test.com", "password123")

	resp := callTool(t, adminToken, "list_projects")
	defer resp.Body.Close()
	assert.Equal(t, http.StatusForbidden, resp.StatusCode)
	body := testutil.ReadJSON(t, resp)
	assert.Equal(t, "this action requires a personal access token, not a session", body["error"])
}

// TestMCP_GetProject covers the second tool in the toolset (PR2).
func TestMCP_GetProject(t *testing.T) {
	resetDB(t)
	adminToken := env.SetupAdmin(t, "mcp-get-project@test.com", "password123")
	project := env.CreateProject(t, adminToken, "MCP Project", "mcp-project")
	projectID := extractID(project["id"])
	readToken := mintScoped(t, adminToken, []string{"read"})

	var got map[string]any
	decodeToolResult(t, callToolArgs(t, readToken, "get_project", map[string]any{
		"project_id": projectID,
	}), &got)
	assert.Equal(t, projectID, got["id"])
	assert.Equal(t, "mcp-project", got["slug"])
	assert.Equal(t, false, got["shared"])
}

// TestMCP_ApplicationTools covers list_applications and get_application.
func TestMCP_ApplicationTools(t *testing.T) {
	resetDB(t)
	adminToken := env.SetupAdmin(t, "mcp-apps@test.com", "password123")
	project := env.CreateProject(t, adminToken, "MCP Project", "mcp-project")
	projectID := extractID(project["id"])
	app := minimalApp(t, adminToken, projectID)
	appID := extractID(app["id"])
	readToken := mintScoped(t, adminToken, []string{"read"})

	var apps []map[string]any
	decodeToolResult(t, callToolArgs(t, readToken, "list_applications", map[string]any{
		"project_id": projectID,
	}), &apps)
	require.Len(t, apps, 1)
	assert.Equal(t, appID, apps[0]["id"])
	assert.Equal(t, projectID, apps[0]["project_id"])

	var got map[string]any
	decodeToolResult(t, callToolArgs(t, readToken, "get_application", map[string]any{
		"application_id": appID,
	}), &got)
	assert.Equal(t, appID, got["id"])
	assert.Equal(t, "image", got["type"])
	// No deploy has happened yet — pending_change must be suppressed, not
	// "config" or "source", matching handler.pendingChange's own rule.
	assert.Nil(t, got["pending_change"])
}

// TestMCP_DatabaseTools covers list_databases and get_database, and asserts
// the wire shape never carries the encrypted credentials column — an MCP
// client has no use for ciphertext and no reason to be handed it.
func TestMCP_DatabaseTools(t *testing.T) {
	resetDB(t)
	adminToken := env.SetupAdmin(t, "mcp-dbs@test.com", "password123")
	project := env.CreateProject(t, adminToken, "MCP Project", "mcp-project")
	projectID := extractID(project["id"])

	resp := env.DoRequest(t, "POST", fmt.Sprintf("/api/projects/%s/databases", projectID), map[string]any{
		"name": "mydb",
		"type": "postgres",
	}, testutil.AuthHeader(adminToken))
	require.Equal(t, http.StatusAccepted, resp.StatusCode)
	dbID := extractID(testutil.ReadJSON(t, resp)["id"])

	readToken := mintScoped(t, adminToken, []string{"read"})

	var dbs []map[string]any
	decodeToolResult(t, callToolArgs(t, readToken, "list_databases", map[string]any{
		"project_id": projectID,
	}), &dbs)
	require.Len(t, dbs, 1)
	assert.Equal(t, dbID, dbs[0]["id"])
	_, hasCreds := dbs[0]["credentials_encrypted"]
	assert.False(t, hasCreds, "list_databases must never surface credentials_encrypted")

	var got map[string]any
	decodeToolResult(t, callToolArgs(t, readToken, "get_database", map[string]any{
		"database_id": dbID,
	}), &got)
	assert.Equal(t, dbID, got["id"])
	assert.Equal(t, "postgres", got["type"])
	_, hasCreds = got["credentials_encrypted"]
	assert.False(t, hasCreds, "get_database must never surface credentials_encrypted")
}

// TestMCP_ListDeploymentsEmpty asserts the bounded-output tool returns "[]",
// never "null", for an application that has never deployed.
func TestMCP_ListDeploymentsEmpty(t *testing.T) {
	resetDB(t)
	adminToken := env.SetupAdmin(t, "mcp-deploys@test.com", "password123")
	project := env.CreateProject(t, adminToken, "MCP Project", "mcp-project")
	projectID := extractID(project["id"])
	app := minimalApp(t, adminToken, projectID)
	appID := extractID(app["id"])
	readToken := mintScoped(t, adminToken, []string{"read"})

	resp := callToolArgs(t, readToken, "list_deployments", map[string]any{
		"application_id": appID,
	})
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	var rpc jsonRPCToolResult
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&rpc))
	require.Nil(t, rpc.Error)
	require.NotNil(t, rpc.Result)
	require.False(t, rpc.Result.IsError)
	assert.Equal(t, "[]", rpc.Result.Content[0].Text)
}

// TestMCP_ApplicationLogs pins the demux trap: ContainerLogsTail's stream is
// stdcopy-multiplexed (not a TTY), and reading it raw puts an 8-byte frame
// header's frequently-printable length bytes in front of the real log text.
func TestMCP_ApplicationLogs(t *testing.T) {
	resetDB(t)
	adminToken := env.SetupAdmin(t, "mcp-logs@test.com", "password123")
	project := env.CreateProject(t, adminToken, "MCP Project", "mcp-project")
	projectID := extractID(project["id"])
	app := minimalApp(t, adminToken, projectID)
	appID := extractID(app["id"])
	readToken := mintScoped(t, adminToken, []string{"read"})

	env.Runtime.ContainerLogsTail_ = dockerLogFrames("app started", "listening on :8080")
	t.Cleanup(func() { env.Runtime.ContainerLogsTail_ = "" })

	resp := callToolArgs(t, readToken, "get_application_logs", map[string]any{
		"application_id": appID,
	})
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	var rpc jsonRPCToolResult
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&rpc))
	require.Nil(t, rpc.Error)
	require.NotNil(t, rpc.Result)
	require.False(t, rpc.Result.IsError, "tool call reported an error: %+v", rpc.Result.Content)
	require.Len(t, rpc.Result.Content, 1)
	text := rpc.Result.Content[0].Text
	assert.Contains(t, text, "app started")
	assert.Contains(t, text, "listening on :8080")
	// The stdcopy frame header (a leading 0x01 stream-type byte plus a
	// 4-byte length) must be gone — a raw read would leave stray bytes
	// ahead of each line instead of clean text.
	assert.NotContains(t, text, "\x01\x00\x00\x00")
}

// TestMCP_DomainTLSStatus is a wiring check for the differentiator tool: a
// domain shows up with its hostname, application and project attribution.
func TestMCP_DomainTLSStatus(t *testing.T) {
	resetDB(t)
	adminToken := env.SetupAdmin(t, "mcp-tls@test.com", "password123")
	project := env.CreateProject(t, adminToken, "MCP Project", "mcp-project")
	projectID := extractID(project["id"])
	app := minimalApp(t, adminToken, projectID)
	appID := extractID(app["id"])

	resp := env.DoRequest(t, "POST", fmt.Sprintf("/api/projects/%s/applications/%s/domains", projectID, appID),
		map[string]any{"hostname": "mcp-test.example.com"}, testutil.AuthHeader(adminToken))
	require.Equal(t, http.StatusCreated, resp.StatusCode)
	resp.Body.Close()

	readToken := mintScoped(t, adminToken, []string{"read"})
	var rows []map[string]any
	decodeToolResult(t, callTool(t, readToken, "list_domain_tls_status"), &rows)
	require.Len(t, rows, 1)
	assert.Equal(t, "mcp-test.example.com", rows[0]["hostname"])
	assert.Equal(t, appID, rows[0]["application_id"])
	assert.Equal(t, projectID, rows[0]["project_id"])
}

// TestMCP_DomainTLSStatusIsBounded proves the limit argument actually
// truncates the SQL query rather than being accepted and ignored — the
// install-wide response list_domain_tls_status has no other bound.
func TestMCP_DomainTLSStatusIsBounded(t *testing.T) {
	resetDB(t)
	adminToken := env.SetupAdmin(t, "mcp-tls-limit@test.com", "password123")
	project := env.CreateProject(t, adminToken, "MCP Project", "mcp-project")
	projectID := extractID(project["id"])
	app := minimalApp(t, adminToken, projectID)
	appID := extractID(app["id"])

	for _, host := range []string{"a.example.com", "b.example.com", "c.example.com"} {
		resp := env.DoRequest(t, "POST", fmt.Sprintf("/api/projects/%s/applications/%s/domains", projectID, appID),
			map[string]any{"hostname": host}, testutil.AuthHeader(adminToken))
		require.Equal(t, http.StatusCreated, resp.StatusCode)
		resp.Body.Close()
	}

	readToken := mintScoped(t, adminToken, []string{"read"})
	var rows []map[string]any
	decodeToolResult(t, callToolArgs(t, readToken, "list_domain_tls_status", map[string]any{
		"limit": 1,
	}), &rows)
	require.Len(t, rows, 1)
}

// TestMCP_ListProjectBackups is a wiring check: an empty project reports an
// empty (never null) backup activity feed.
func TestMCP_ListProjectBackups(t *testing.T) {
	resetDB(t)
	adminToken := env.SetupAdmin(t, "mcp-backups@test.com", "password123")
	project := env.CreateProject(t, adminToken, "MCP Project", "mcp-project")
	projectID := extractID(project["id"])
	readToken := mintScoped(t, adminToken, []string{"read"})

	resp := callToolArgs(t, readToken, "list_project_backups", map[string]any{
		"project_id": projectID,
	})
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	var rpc jsonRPCToolResult
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&rpc))
	require.Nil(t, rpc.Error)
	require.NotNil(t, rpc.Result)
	require.False(t, rpc.Result.IsError)
	assert.Equal(t, "[]", rpc.Result.Content[0].Text)
}

// TestMCP_ProjectPinBlocksOtherProjects is the pin-enforcement counterpart
// of TestScope_ReadTokenCannotWrite: a token pinned to project A must not
// reach project B through any tool argument, even though nothing about the
// call's HTTP path carries a {projectId} for middleware.RequirePinnedProjectMatch
// to check — each tool must enforce this itself.
func TestMCP_ProjectPinBlocksOtherProjects(t *testing.T) {
	resetDB(t)
	adminToken := env.SetupAdmin(t, "mcp-pin@test.com", "password123")
	ownProject := env.CreateProject(t, adminToken, "Own Project", "own-project")
	otherProject := env.CreateProject(t, adminToken, "Other Project", "other-project")
	ownProjectID := extractID(ownProject["id"])
	otherProjectID := extractID(otherProject["id"])
	otherApp := minimalApp(t, adminToken, otherProjectID)
	otherAppID := extractID(otherApp["id"])

	adminUserID := extractID(mustAuthMe(t, adminToken)["id"])
	pinnedToken := createPinnedAPIToken(t, adminUserID, ownProjectID, []string{"read"})

	assert.Equal(t, "access denied", toolErrorText(t, callToolArgs(t, pinnedToken, "get_project", map[string]any{
		"project_id": otherProjectID,
	})))
	assert.Equal(t, "access denied", toolErrorText(t, callToolArgs(t, pinnedToken, "list_applications", map[string]any{
		"project_id": otherProjectID,
	})))
	assert.Equal(t, "access denied", toolErrorText(t, callToolArgs(t, pinnedToken, "get_application", map[string]any{
		"application_id": otherAppID,
	})))

	// The pinned project itself must still work.
	var got map[string]any
	decodeToolResult(t, callToolArgs(t, pinnedToken, "get_project", map[string]any{
		"project_id": ownProjectID,
	}), &got)
	assert.Equal(t, ownProjectID, got["id"])
}

// TestMCP_ListProjectsPinSurvivesLimit is the regression test for a bug this
// package avoided rather than shipped: list_projects pushes the pin into the
// SQL query (see pinnedProjectUUIDs) instead of applying it as a post-query
// Go-side filter. If it filtered in Go instead, a small limit could truncate
// the row set to the newest projects BEFORE the pin ever got a chance to
// narrow it — silently returning an empty (or incomplete) list for a pinned
// token whose visible projects aren't among the newest few. Pinned to TWO
// projects, both older than the limit-truncated newer ones, so the same bug
// class would also be caught if it only dropped one of the two.
func TestMCP_ListProjectsPinSurvivesLimit(t *testing.T) {
	resetDB(t)
	adminToken := env.SetupAdmin(t, "mcp-pin-limit@test.com", "password123")

	// Created first, so neither is among the most-recently-created projects.
	pinnedProjectA := env.CreateProject(t, adminToken, "Pinned Project A", "pinned-project-a")
	pinnedProjectAID := extractID(pinnedProjectA["id"])
	pinnedProjectB := env.CreateProject(t, adminToken, "Pinned Project B", "pinned-project-b")
	pinnedProjectBID := extractID(pinnedProjectB["id"])
	// Created after them, so a naive "LIMIT then filter" would return these
	// instead of the pinned projects.
	env.CreateProject(t, adminToken, "Newer Project 1", "newer-project-1")
	env.CreateProject(t, adminToken, "Newer Project 2", "newer-project-2")

	adminUserID := extractID(mustAuthMe(t, adminToken)["id"])
	pinnedToken := createMultiPinnedAPIToken(t, adminUserID, []string{pinnedProjectAID, pinnedProjectBID}, []string{"read"})

	var projects []map[string]any
	decodeToolResult(t, callToolArgs(t, pinnedToken, "list_projects", map[string]any{
		"limit": 2,
	}), &projects)
	require.Len(t, projects, 2)
	gotIDs := []string{projects[0]["id"].(string), projects[1]["id"].(string)}
	assert.ElementsMatch(t, []string{pinnedProjectAID, pinnedProjectBID}, gotIDs)
}

// TestMCP_ToolsListing_NoDestructiveTools structurally asserts the destructive
// boundary: delete/restore tools must not be registered at all, not merely
// unreachable behind a scope check that a future refactor could accidentally
// loosen. The exact list is a deliberate tripwire — adding a tool means
// editing it here, in review, alongside its scope in mcpserver/scope.go.
func TestMCP_ToolsListing_NoDestructiveTools(t *testing.T) {
	resetDB(t)
	adminToken := env.SetupAdmin(t, "mcp-tools-list@test.com", "password123")
	readToken := mintScoped(t, adminToken, []string{"read"})

	resp := env.DoRequest(t, "POST", "/mcp", map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "tools/list",
		"params":  map[string]any{},
	}, mcpHeaders(readToken))
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	var rpc struct {
		Result *struct {
			Tools []struct {
				Name string `json:"name"`
			} `json:"tools"`
		} `json:"result"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&rpc))
	require.NotNil(t, rpc.Result)

	names := make([]string, len(rpc.Result.Tools))
	for i, tool := range rpc.Result.Tools {
		names[i] = tool.Name
	}
	assert.ElementsMatch(t, []string{
		"list_projects", "get_project",
		"list_applications", "get_application",
		"list_databases", "get_database",
		"list_deployments", "get_application_logs", "get_deployment_logs",
		"list_domain_tls_status", "list_project_backups",
		"trigger_deployment", "update_application", "start_application", "stop_application",
		"set_application_env_vars",
		"set_application_resources", "set_application_health_check", "set_application_runtime",
		"set_database_resources",
		"create_application", "create_database", "create_domain",
	}, names)

	for _, name := range names {
		for _, word := range []string{"delete", "restore", "destroy", "remove", "drop"} {
			assert.False(t, strings.Contains(name, word),
				"tool %q reads as destructive — tokens cannot destroy, so no MCP tool may", name)
		}
	}
}

// TestMCP_NonAdminCannotDistinguishNotFoundFromForbidden is the regression
// test for the existence-oracle bug: a non-admin token must get the exact
// same "access denied" whether a resource is a genuine nonexistent id or one
// that exists but belongs to someone else, the same way handler.canAccessOwned
// collapses the two on the REST side. Needs a real member — every other MCP
// test in this file uses an admin-owned token, which never exercises the
// non-admin branch that had the bug.
func TestMCP_NonAdminCannotDistinguishNotFoundFromForbidden(t *testing.T) {
	resetDB(t)
	adminToken := env.SetupAdmin(t, "mcp-oracle-admin@test.com", "password123")

	// A project and application the member does not own and that is not shared.
	project := env.CreateProject(t, adminToken, "Admins Only", "admins-only")
	projectID := extractID(project["id"])
	app := minimalApp(t, adminToken, projectID)
	appID := extractID(app["id"])

	env.DoRequest(t, "POST", "/api/users", map[string]string{
		"email": "mcp-oracle-member@test.com", "password": "password123", "role": "member",
	}, testutil.AuthHeader(adminToken)).Body.Close()
	memberToken := env.LoginAs(t, "mcp-oracle-member@test.com", "password123")
	memberReadToken := mintScoped(t, memberToken, []string{"read"})

	const nonexistentID = "00000000-0000-0000-0000-000000000000"

	// get_project: existing-but-inaccessible vs. genuinely nonexistent.
	assert.Equal(t, "access denied", toolErrorText(t, callToolArgs(t, memberReadToken, "get_project", map[string]any{
		"project_id": projectID,
	})), "existing project the member doesn't own")
	assert.Equal(t, "access denied", toolErrorText(t, callToolArgs(t, memberReadToken, "get_project", map[string]any{
		"project_id": nonexistentID,
	})), "nonexistent project must read identically, not \"project not found\"")

	// get_application: same pair, exercising the "fetch it yourself, then
	// authorize" shape rather than authorizeProject's own internal fetch.
	assert.Equal(t, "access denied", toolErrorText(t, callToolArgs(t, memberReadToken, "get_application", map[string]any{
		"application_id": appID,
	})), "existing application the member doesn't own")
	assert.Equal(t, "access denied", toolErrorText(t, callToolArgs(t, memberReadToken, "get_application", map[string]any{
		"application_id": nonexistentID,
	})), "nonexistent application must read identically, not \"application not found\"")

	// An admin token, by contrast, still gets the real "not found" — only a
	// non-admin's view collapses the two.
	adminReadToken := mintScoped(t, adminToken, []string{"read"})
	assert.Equal(t, "project not found", toolErrorText(t, callToolArgs(t, adminReadToken, "get_project", map[string]any{
		"project_id": nonexistentID,
	})))
}

// seedBuildLog inserts a deployment for appID carrying the given build log.
func seedBuildLog(t *testing.T, appID, buildLog string) string {
	t.Helper()
	ctx := context.Background()
	var appUUID pgtype.UUID
	require.NoError(t, appUUID.Scan(appID))
	d, err := env.Queries.CreateDeployment(ctx, generated.CreateDeploymentParams{
		ApplicationID: appUUID, Status: "failed", TriggeredBy: "manual",
	})
	require.NoError(t, err)
	require.NoError(t, env.Queries.UpdateDeploymentBuildLogs(ctx, generated.UpdateDeploymentBuildLogsParams{
		ID: d.ID, BuildLogs: pgtype.Text{String: buildLog, Valid: true},
	}))
	return uuid.UUID(d.ID.Bytes).String()
}

func toolText(t *testing.T, resp *http.Response) string {
	t.Helper()
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var rpc jsonRPCToolResult
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&rpc))
	require.Nil(t, rpc.Error)
	require.NotNil(t, rpc.Result)
	require.False(t, rpc.Result.IsError, "tool call reported an error: %+v", rpc.Result.Content)
	require.Len(t, rpc.Result.Content, 1)
	return rpc.Result.Content[0].Text
}

// TestMCP_DeploymentLogs: a bounded tail of the stored build log, with terminal
// colour stripped in BOTH forms — the raw ESC byte and the JSON-escaped
// "\u001b" a NDJSON build log actually carries.
func TestMCP_DeploymentLogs(t *testing.T) {
	resetDB(t)
	adminToken := env.SetupAdmin(t, "mcp-deplogs@test.com", "password123")
	project := env.CreateProject(t, adminToken, "MCP Project", "mcp-project")
	app := minimalApp(t, adminToken, extractID(project["id"]))
	readToken := mintScoped(t, adminToken, []string{"read"})

	logBlob := strings.Join([]string{
		`{"ts":"2026-09-29T10:00:00Z","level":"info","msg":"step 1"}`,
		`{"ts":"2026-09-29T10:00:01Z","level":"warn","msg":"\u001b[33mmise\u001b[0m WARN slow"}`,
		"raw \x1b[31mred\x1b[0m line",
		`{"ts":"2026-09-29T10:00:03Z","level":"error","msg":"build failed: exit 1"}`,
	}, "\n")
	depID := seedBuildLog(t, extractID(app["id"]), logBlob)

	text := toolText(t, callToolArgs(t, readToken, "get_deployment_logs", map[string]any{"deployment_id": depID}))
	assert.Contains(t, text, "2026-09-29T10:00:03Z error build failed: exit 1")
	assert.NotContains(t, text, `"msg"`, "the NDJSON envelope should be rendered, not passed through")
	assert.Contains(t, text, "mise")
	assert.Contains(t, text, "raw red line")
	assert.NotContains(t, text, "\x1b")
	assert.NotContains(t, text, "\\u001b")
	assert.NotContains(t, text, "[33m")

	// tail bounds the output and says the head was dropped.
	text = toolText(t, callToolArgs(t, readToken, "get_deployment_logs", map[string]any{"deployment_id": depID, "tail": 1}))
	assert.Contains(t, text, "build failed: exit 1")
	assert.NotContains(t, text, "step 1")
	assert.Contains(t, text, "3 earlier lines omitted")
}

func TestMCP_DeploymentLogsNoLogRecorded(t *testing.T) {
	resetDB(t)
	adminToken := env.SetupAdmin(t, "mcp-deplogs-empty@test.com", "password123")
	project := env.CreateProject(t, adminToken, "MCP Project", "mcp-project")
	app := minimalApp(t, adminToken, extractID(project["id"]))
	readToken := mintScoped(t, adminToken, []string{"read"})
	depID := seedBuildLog(t, extractID(app["id"]), "")

	text := toolText(t, callToolArgs(t, readToken, "get_deployment_logs", map[string]any{"deployment_id": depID}))
	assert.Contains(t, text, "no build log recorded")
}

// TestMCP_DeploymentLogsAccess: a project pin and non-admin ownership both gate
// the build log, and a non-admin cannot tell a foreign deployment from a
// nonexistent one.
func TestMCP_DeploymentLogsAccess(t *testing.T) {
	resetDB(t)
	adminToken := env.SetupAdmin(t, "mcp-deplogs-acl@test.com", "password123")
	ownProject := env.CreateProject(t, adminToken, "Own Project", "own-project")
	otherProject := env.CreateProject(t, adminToken, "Other Project", "other-project")
	otherApp := minimalApp(t, adminToken, extractID(otherProject["id"]))
	depID := seedBuildLog(t, extractID(otherApp["id"]), "secret build output")

	adminUserID := extractID(mustAuthMe(t, adminToken)["id"])
	pinned := createPinnedAPIToken(t, adminUserID, extractID(ownProject["id"]), []string{"read"})
	assert.Equal(t, "access denied", toolErrorText(t, callToolArgs(t, pinned, "get_deployment_logs", map[string]any{"deployment_id": depID})))

	env.DoRequest(t, "POST", "/api/users", map[string]string{
		"email": "mcp-deplogs-member@test.com", "password": "password123", "role": "member",
	}, testutil.AuthHeader(adminToken)).Body.Close()
	memberRead := mintScoped(t, env.LoginAs(t, "mcp-deplogs-member@test.com", "password123"), []string{"read"})
	assert.Equal(t, "access denied", toolErrorText(t, callToolArgs(t, memberRead, "get_deployment_logs", map[string]any{"deployment_id": depID})))
	assert.Equal(t, "access denied", toolErrorText(t, callToolArgs(t, memberRead, "get_deployment_logs", map[string]any{
		"deployment_id": "00000000-0000-0000-0000-000000000000",
	})), "nonexistent must read identically to forbidden")
}

// The container-log tool strips terminal colour too (the dashboard already
// does), so an assistant does not read "[33mWARN[0m".
func TestMCP_ApplicationLogsStripsANSI(t *testing.T) {
	resetDB(t)
	adminToken := env.SetupAdmin(t, "mcp-logs-ansi@test.com", "password123")
	project := env.CreateProject(t, adminToken, "MCP Project", "mcp-project")
	app := minimalApp(t, adminToken, extractID(project["id"]))
	readToken := mintScoped(t, adminToken, []string{"read"})

	env.Runtime.ContainerLogsTail_ = dockerLogFrames("\x1b[33mmise\x1b[0m \x1b[33mWARN\x1b[0m slow")
	t.Cleanup(func() { env.Runtime.ContainerLogsTail_ = "" })

	text := toolText(t, callToolArgs(t, readToken, "get_application_logs", map[string]any{"application_id": extractID(app["id"])}))
	assert.Contains(t, text, "mise WARN slow")
	assert.NotContains(t, text, "\x1b")
	assert.NotContains(t, text, "[33m")
}
