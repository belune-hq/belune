package handler_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/belune-hq/belune/internal/testutil"
)

// settingsApp creates a project and an application with DISTINCTIVE resource,
// health-check and runtime values already set, so a tool that quietly resets
// one of them is visible rather than indistinguishable from the defaults.
func settingsApp(t *testing.T, adminToken string) (projectID, appID string) {
	t.Helper()
	projectID = extractID(env.CreateProject(t, adminToken, "S", "s")["id"])
	appID = extractID(minimalApp(t, adminToken, projectID)["id"])

	put := func(path string, body any) {
		t.Helper()
		resp := env.DoRequest(t, "PUT",
			fmt.Sprintf("/api/projects/%s/applications/%s/%s", projectID, appID, path),
			body, testutil.AuthHeader(adminToken))
		defer resp.Body.Close()
		require.Equal(t, http.StatusOK, resp.StatusCode, "seeding %s", path)
	}
	put("resources", map[string]any{"cpu_limit": 1.5, "memory_limit": 805306368})
	put("health-check", map[string]any{"type": "http", "path": "/seeded", "expect_status": 204, "timeout_seconds": 7})
	put("runtime", map[string]any{"readonly_rootfs": true, "container_caps": "minimal"})
	return projectID, appID
}

// seedDatabaseRow inserts a running database directly: these tools are about
// resource limits, not the provisioning flow, and a real provision would need
// a container. 'running' is deliberate — it is the status that makes
// SetResources attempt its live apply, which is the path worth exercising.
func seedDatabaseRow(t *testing.T, projectID, slug string) string {
	t.Helper()
	var id string
	require.NoError(t, env.Pool.QueryRow(context.Background(), `
		INSERT INTO databases (project_id, type, name, slug, version, status, credentials_encrypted)
		VALUES ($1::uuid, 'postgres', 'db', $2, '16', 'running', '\x00')
		RETURNING id::text`, projectID, slug).Scan(&id))
	return id
}

type appRuntimeRow struct {
	cpu      float64
	memory   int64
	hcType   string
	hcPath   string
	readonly bool
	caps     string
}

func readAppRuntime(t *testing.T, appID string) appRuntimeRow {
	t.Helper()
	var r appRuntimeRow
	var path *string
	require.NoError(t, env.Pool.QueryRow(context.Background(), `
		SELECT cpu_limit, memory_limit, health_check_type, health_check_path,
		       readonly_rootfs, container_caps
		FROM applications WHERE id = $1`, appID).
		Scan(&r.cpu, &r.memory, &r.hcType, &path, &r.readonly, &r.caps))
	if path != nil {
		r.hcPath = *path
	}
	return r
}

// TestMCP_SetResources_AppliesAndLeavesTheRestAlone is the positive control for
// the resources tool, and the guard against the full-row-update trap: the tool
// writes through SetApplicationResources, which must not disturb the health
// check or the runtime profile beside it.
func TestMCP_SetResources_AppliesAndLeavesTheRestAlone(t *testing.T) {
	resetDB(t)
	adminToken := env.SetupAdmin(t, "mcp-res@test.com", "password123")
	_, appID := settingsApp(t, adminToken)
	token := mintScoped(t, adminToken, []string{"write"})

	var got struct {
		Application map[string]any `json:"application"`
		Note        string         `json:"note"`
	}
	decodeToolResult(t, callToolArgs(t, token, "set_application_resources", map[string]any{
		"application_id": appID, "cpu_limit": 0.25, "memory_limit": 268435456,
	}), &got)

	after := readAppRuntime(t, appID)
	assert.Equal(t, 0.25, after.cpu)
	assert.Equal(t, int64(268435456), after.memory)
	assert.Contains(t, got.Note, "trigger_deployment",
		"the result must say the running container is unaffected until a redeploy")

	// ⚠️ The point of the seeded values: a resources write must not touch these.
	assert.Equal(t, "http", after.hcType, "the health check must survive a resources change")
	assert.Equal(t, "/seeded", after.hcPath, "the health check path must survive a resources change")
	assert.True(t, after.readonly, "the runtime profile must survive a resources change")
	assert.Equal(t, "minimal", after.caps, "the capability profile must survive a resources change")
}

// TestMCP_SetResources_RefusesALimitDockerCannotApply covers the floor this
// package adds above REST. An assistant filling a number from conversation is
// the caller that confuses bytes with megabytes, and the result is an
// application that cannot start at all.
func TestMCP_SetResources_RefusesALimitDockerCannotApply(t *testing.T) {
	resetDB(t)
	adminToken := env.SetupAdmin(t, "mcp-res-floor@test.com", "password123")
	_, appID := settingsApp(t, adminToken)
	token := mintScoped(t, adminToken, []string{"write"})
	before := readAppRuntime(t, appID)

	for _, tc := range []struct {
		name    string
		mem     any
		wantErr string
	}{
		{"512 read as megabytes", 512, "at least 6291456"},
		{"negative", -1, "cannot be negative"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Contains(t, toolErrorText(t, callToolArgs(t, token, "set_application_resources", map[string]any{
				"application_id": appID, "cpu_limit": 1, "memory_limit": tc.mem,
			})), tc.wantErr)
		})
	}
	assert.Equal(t, before, readAppRuntime(t, appID), "a refused call must change nothing")

	// The control: 0 is legitimate and means unlimited, so the floor must not
	// reject it.
	var ok struct{ Note string }
	decodeToolResult(t, callToolArgs(t, token, "set_application_resources", map[string]any{
		"application_id": appID, "cpu_limit": 0, "memory_limit": 0,
	}), &ok)
	assert.Equal(t, int64(0), readAppRuntime(t, appID).memory, "0 must be accepted as unlimited")
}

// TestMCP_SetHealthCheck_ReplacesTheWholeConfiguration pins the behaviour the
// shared validator exists for: switching mechanism must clear the other one's
// fields, not leave a stale command a later deploy would still run.
func TestMCP_SetHealthCheck_ReplacesTheWholeConfiguration(t *testing.T) {
	resetDB(t)
	adminToken := env.SetupAdmin(t, "mcp-hc@test.com", "password123")
	_, appID := settingsApp(t, adminToken)
	token := mintScoped(t, adminToken, []string{"write"})

	var got struct{ Note string }
	decodeToolResult(t, callToolArgs(t, token, "set_application_health_check", map[string]any{
		"application_id": appID, "type": "command", "command": "pg_isready", "retries": 4,
	}), &got)

	var hcPath, hcCommand *string
	var retries *int32
	require.NoError(t, env.Pool.QueryRow(context.Background(),
		`SELECT health_check_path, health_check_command, health_check_retries FROM applications WHERE id = $1`, appID).
		Scan(&hcPath, &hcCommand, &retries))
	assert.Nil(t, hcPath, "switching to a command check must clear the http path")
	require.NotNil(t, hcCommand)
	assert.Equal(t, "pg_isready", *hcCommand)
	require.NotNil(t, retries)
	assert.Equal(t, int32(4), *retries)

	// Invalid input writes nothing, and reports why.
	assert.Contains(t, toolErrorText(t, callToolArgs(t, token, "set_application_health_check", map[string]any{
		"application_id": appID, "type": "http", "path": "no-leading-slash",
	})), "must start with /")
	assert.Contains(t, toolErrorText(t, callToolArgs(t, token, "set_application_health_check", map[string]any{
		"application_id": appID, "type": "nonsense",
	})), "one of: none, http, command")
}

// TestMCP_SetRuntime_AppliesAndValidates covers the hardening profile tool.
func TestMCP_SetRuntime_AppliesAndValidates(t *testing.T) {
	resetDB(t)
	adminToken := env.SetupAdmin(t, "mcp-rt@test.com", "password123")
	_, appID := settingsApp(t, adminToken)
	token := mintScoped(t, adminToken, []string{"write"})

	var got struct{ Note string }
	decodeToolResult(t, callToolArgs(t, token, "set_application_runtime", map[string]any{
		"application_id": appID, "readonly_rootfs": false, "container_caps": "standard",
	}), &got)
	after := readAppRuntime(t, appID)
	assert.False(t, after.readonly)
	assert.Equal(t, "standard", after.caps)

	// An unknown profile is refused rather than silently stored — the column is
	// a free-text string, so nothing downstream would catch it.
	assert.Contains(t, toolErrorText(t, callToolArgs(t, token, "set_application_runtime", map[string]any{
		"application_id": appID, "readonly_rootfs": true, "container_caps": "privileged",
	})), "must be 'minimal' or 'standard'")
	assert.Equal(t, "standard", readAppRuntime(t, appID).caps, "a refused profile must not be written")
}

// TestMCP_Settings_ScopeAndPin is the security half for every settings tool: the
// read/deploy scopes cannot reach them, and a project-pinned token cannot
// touch another project's application or database.
func TestMCP_Settings_ScopeAndPin(t *testing.T) {
	resetDB(t)
	adminToken := env.SetupAdmin(t, "mcp-set-scope@test.com", "password123")
	adminUserID := extractID(mustAuthMe(t, adminToken)["id"])
	mineProject, mineApp := settingsApp(t, adminToken)

	otherProject := extractID(env.CreateProject(t, adminToken, "Other", "other")["id"])
	otherApp := extractID(minimalApp(t, adminToken, otherProject)["id"])

	calls := map[string]map[string]any{
		"set_application_resources":    {"cpu_limit": 1, "memory_limit": 268435456},
		"set_application_health_check": {"type": "none"},
		"set_application_runtime":      {"readonly_rootfs": false, "container_caps": "standard"},
	}

	// ⚠️ Scope first. Each tool requires "write"; read and deploy must not do.
	for _, scope := range []string{"read", "deploy"} {
		tok := mintScoped(t, adminToken, []string{scope})
		for name, args := range calls {
			a := map[string]any{"application_id": mineApp}
			for k, v := range args {
				a[k] = v
			}
			assert.Equal(t, fmt.Sprintf("the %s tool requires the %q scope; this token has: %s", name, "write", scope),
				rpcError(t, callToolArgs(t, tok, name, a)))
		}
	}

	// ⚠️ Then the pin, with a positive control: the same token must still reach
	// its OWN application, or an unconditional deny would pass this test.
	pinned := createPinnedAPIToken(t, adminUserID, mineProject, []string{"write"})
	for name, args := range calls {
		own := map[string]any{"application_id": mineApp}
		foreign := map[string]any{"application_id": otherApp}
		for k, v := range args {
			own[k], foreign[k] = v, v
		}
		t.Run(name+" reaches its own project", func(t *testing.T) {
			var ok struct{ Note string }
			decodeToolResult(t, callToolArgs(t, pinned, name, own), &ok)
		})
		t.Run(name+" refuses a foreign project", func(t *testing.T) {
			assert.Equal(t, "access denied", toolErrorText(t, callToolArgs(t, pinned, name, foreign)))
		})
	}

	// The foreign application must be untouched by any of the refusals.
	foreignAfter := readAppRuntime(t, otherApp)
	assert.True(t, foreignAfter.readonly, "a refused runtime change still wrote to another project")
	assert.Equal(t, "minimal", foreignAfter.caps)
}

// TestMCP_SetDatabaseResources_ScopeAndPin covers the one database write on the
// MCP surface. Separate from the application cases because it needs a real
// database row and because, unlike the application tool, its change is applied
// live — so the note it returns is part of the contract.
func TestMCP_SetDatabaseResources_ScopeAndPin(t *testing.T) {
	resetDB(t)
	adminToken := env.SetupAdmin(t, "mcp-dbres@test.com", "password123")
	adminUserID := extractID(mustAuthMe(t, adminToken)["id"])
	mineProject := extractID(env.CreateProject(t, adminToken, "Mine", "mine")["id"])
	mineDB := seedDatabaseRow(t, mineProject, "mine-db")
	otherProject := extractID(env.CreateProject(t, adminToken, "Other", "other")["id"])
	otherDB := seedDatabaseRow(t, otherProject, "other-db")

	args := func(id string, mem int64) map[string]any {
		return map[string]any{"database_id": id, "cpu_limit": 0.5, "memory_limit": mem}
	}

	for _, scope := range []string{"read", "deploy"} {
		tok := mintScoped(t, adminToken, []string{scope})
		assert.Equal(t, fmt.Sprintf("the set_database_resources tool requires the %q scope; this token has: %s", "write", scope),
			rpcError(t, callToolArgs(t, tok, "set_database_resources", args(mineDB, 268435456))))
	}

	token := mintScoped(t, adminToken, []string{"write"})
	assert.Contains(t, toolErrorText(t, callToolArgs(t, token, "set_database_resources", args(mineDB, 512))),
		"at least 6291456", "the byte/megabyte floor applies to databases too")

	pinned := createPinnedAPIToken(t, adminUserID, mineProject, []string{"write"})
	var ok struct {
		Database map[string]any `json:"database"`
		Note     string         `json:"note"`
	}
	decodeToolResult(t, callToolArgs(t, pinned, "set_database_resources", args(mineDB, 268435456)), &ok)
	// ⚠️ The note must reflect what ACTUALLY happened, not the database's
	// status. seedDatabaseRow leaves internal_host NULL, so there is no
	// container to apply the limit to and the service reports appliedLive
	// false — the reply must therefore say the change waits for the next
	// container, not claim it is already in force. An earlier version inferred
	// this from status == 'running' and would have claimed success here.
	assert.Equal(t, "saved; it takes effect the next time the container is created", ok.Note,
		"the reply must not claim a live apply that did not happen")

	assert.Equal(t, "access denied",
		toolErrorText(t, callToolArgs(t, pinned, "set_database_resources", args(otherDB, 268435456))),
		"a pinned token reached another project's database")

	var cpu float64
	var mem int64
	require.NoError(t, env.Pool.QueryRow(context.Background(),
		`SELECT cpu_limit, memory_limit FROM databases WHERE id = $1`, mineDB).Scan(&cpu, &mem))
	assert.Equal(t, 0.5, cpu)
	assert.Equal(t, int64(268435456), mem)

	require.NoError(t, env.Pool.QueryRow(context.Background(),
		`SELECT cpu_limit, memory_limit FROM databases WHERE id = $1`, otherDB).Scan(&cpu, &mem))
	assert.Equal(t, float64(0), cpu, "the refused call still wrote to another project's database")
	assert.Equal(t, int64(0), mem)
}

// TestMCP_Settings_AreAuditedAgainstTheToken: the audit log is the operator's
// only record of what an assistant changed, so every one of these must leave a
// row attributed to the token, with the same action name REST uses.
func TestMCP_Settings_AreAuditedAgainstTheToken(t *testing.T) {
	resetDB(t)
	adminToken := env.SetupAdmin(t, "mcp-set-audit@test.com", "password123")
	_, appID := settingsApp(t, adminToken)
	token := mintScoped(t, adminToken, []string{"write"})
	base := newAuditedMCP(t)

	for _, tc := range []struct {
		tool, action string
		args         map[string]any
	}{
		{"set_application_resources", "set_resources", map[string]any{"cpu_limit": 2, "memory_limit": 536870912}},
		{"set_application_health_check", "set_health_check", map[string]any{"type": "none"}},
		{"set_application_runtime", "update_application_runtime", map[string]any{"readonly_rootfs": false, "container_caps": "standard"}},
	} {
		t.Run(tc.tool, func(t *testing.T) {
			a := map[string]any{"application_id": appID}
			for k, v := range tc.args {
				a[k] = v
			}
			decodeToolResult(t, callToolAt(t, base, token, tc.tool, a), &struct{}{})

			var tokenID *string
			var details string
			require.Eventually(t, func() bool {
				return env.Pool.QueryRow(context.Background(),
					"SELECT token_id::text, COALESCE(details::text,'{}') FROM audit_logs WHERE action = $1 AND resource_id = $2",
					tc.action, appID).Scan(&tokenID, &details) == nil
			}, 2*time.Second, 10*time.Millisecond, "%s must write an audit row as %q", tc.tool, tc.action)
			require.NotNil(t, tokenID, "attributed to the token, not read as a human action")
			assert.NotEqual(t, "{}", details, "the row must say what changed")
			var parsed map[string]any
			require.NoError(t, json.Unmarshal([]byte(details), &parsed))
		})
	}
}

// TestMCP_Settings_AuditUsesTheCanonicalID: the audit row must key on the
// resource's own id, not the spelling the caller happened to send.
//
// ⚠️ pgtype.UUID parses an uppercased id just as happily, so echoing the
// argument wrote "1F82A18D-..." and a query for the canonical lowercase form
// missed the row — the audit log's whole job is being findable. The caller
// here is an LLM, which varies spelling far more readily than a dashboard
// does. Same class as the deploy task id canonicalisation on this branch.
func TestMCP_Settings_AuditUsesTheCanonicalID(t *testing.T) {
	resetDB(t)
	adminToken := env.SetupAdmin(t, "mcp-audit-canon@test.com", "password123")
	_, appID := settingsApp(t, adminToken)
	token := mintScoped(t, adminToken, []string{"write"})
	base := newAuditedMCP(t)

	shouted := strings.ToUpper(appID)
	require.NotEqual(t, appID, shouted, "the fixture id must contain hex letters for this to mean anything")

	decodeToolResult(t, callToolAt(t, base, token, "set_application_resources", map[string]any{
		"application_id": shouted, "cpu_limit": 1, "memory_limit": 268435456,
	}), &struct{}{})

	var found int
	require.Eventually(t, func() bool {
		env.Pool.QueryRow(context.Background(),
			"SELECT count(*) FROM audit_logs WHERE action = 'set_resources' AND resource_id = $1", appID).Scan(&found)
		return found == 1
	}, 2*time.Second, 10*time.Millisecond,
		"the row must be findable by the canonical id, whatever spelling the caller used")
}
