package handler_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func createAppArgs(projectID, name string, extra map[string]any) map[string]any {
	args := map[string]any{
		"project_id": projectID, "name": name,
		"type": "image", "build_type": "image", "source_image": "nginx:1.27-alpine",
	}
	for k, v := range extra {
		args[k] = v
	}
	return args
}

// TestMCP_CreateApplication_CreatesButDoesNotDeploy is the positive control,
// and pins the one thing an assistant most often gets wrong about create:
// nothing is running afterwards.
func TestMCP_CreateApplication_CreatesButDoesNotDeploy(t *testing.T) {
	resetDB(t)
	adminToken := env.SetupAdmin(t, "mcp-create-app@test.com", "password123")
	projectID := extractID(env.CreateProject(t, adminToken, "P", "p")["id"])
	token := mintScoped(t, adminToken, []string{"write"})

	var got struct {
		Application map[string]any `json:"application"`
		Note        string         `json:"note"`
	}
	decodeToolResult(t, callToolArgs(t, token, "create_application",
		createAppArgs(projectID, "Made By Tool", nil)), &got)

	assert.Equal(t, "Made By Tool", got.Application["name"])
	assert.Contains(t, got.Note, "trigger_deployment",
		"the reply must say it is not deployed yet")

	var count int
	require.NoError(t, env.Pool.QueryRow(context.Background(),
		`SELECT count(*) FROM applications WHERE project_id = $1 AND name = 'Made By Tool'`, projectID).Scan(&count))
	assert.Equal(t, 1, count)

	// Nothing was deployed: no deployment row exists for it.
	require.NoError(t, env.Pool.QueryRow(context.Background(),
		`SELECT count(*) FROM deployments d JOIN applications a ON a.id = d.application_id
		  WHERE a.project_id = $1`, projectID).Scan(&count))
	assert.Equal(t, 0, count, "create must not deploy")
}

// TestMCP_CreateApplication_RefusesADuplicateName covers the recorded decision:
// error on duplicate rather than returning the existing resource, because
// return-existing leaves the caller unable to tell "I created this" from "this
// already existed". The error must NAME the conflict so a retry is
// recoverable — a bare conflict turns a recoverable state into a confused one.
//
// ⚠️ Nothing below the tool enforces this: an application's slug carries its
// own id, so the database accepts two applications with the same name.
func TestMCP_CreateApplication_RefusesADuplicateName(t *testing.T) {
	resetDB(t)
	adminToken := env.SetupAdmin(t, "mcp-create-dup@test.com", "password123")
	projectID := extractID(env.CreateProject(t, adminToken, "Mine", "mine")["id"])
	token := mintScoped(t, adminToken, []string{"write"})

	decodeToolResult(t, callToolArgs(t, token, "create_application",
		createAppArgs(projectID, "Only One", nil)), &struct{}{})

	msg := toolErrorText(t, callToolArgs(t, token, "create_application",
		createAppArgs(projectID, "Only One", nil)))
	assert.Contains(t, msg, "Only One", "the error must name the application")
	assert.Contains(t, msg, "Mine", "the error must name the project")
	assert.Contains(t, msg, "get_application", "the error must say how to recover")

	// Case and surrounding space are the same name to a person, so they are
	// the same name here — an assistant varying either is still a retry.
	assert.Contains(t, toolErrorText(t, callToolArgs(t, token, "create_application",
		createAppArgs(projectID, "  only one  ", nil))), "already exists")

	var count int
	require.NoError(t, env.Pool.QueryRow(context.Background(),
		`SELECT count(*) FROM applications WHERE project_id = $1`, projectID).Scan(&count))
	assert.Equal(t, 1, count, "a refused create must not have written a second application")
}

// TestMCP_CreateApplication_ValidatesSourceAndRejectsCredentials: the tool
// shares the REST validator, and deliberately exposes no credential fields.
func TestMCP_CreateApplication_ValidatesSourceAndRejectsCredentials(t *testing.T) {
	resetDB(t)
	adminToken := env.SetupAdmin(t, "mcp-create-valid@test.com", "password123")
	projectID := extractID(env.CreateProject(t, adminToken, "P", "p")["id"])
	token := mintScoped(t, adminToken, []string{"write"})

	// An image application with no image is refused by the shared validator.
	assert.NotEmpty(t, toolErrorText(t, callToolArgs(t, token, "create_application", map[string]any{
		"project_id": projectID, "name": "No Image", "type": "image", "build_type": "image",
	})))

	// ⚠️ A git integration and a git token are credentials, and the schema is
	// closed, so passing either is rejected at the protocol layer rather than
	// silently ignored.
	for _, field := range []string{"git_integration_id", "git_token", "cpu_limit"} {
		args := createAppArgs(projectID, "Smuggled", map[string]any{field: "x"})
		assert.Contains(t, toolErrorText(t, callToolArgs(t, token, "create_application", args)),
			"unexpected additional properties", "%s must not be accepted", field)
	}

	var count int
	require.NoError(t, env.Pool.QueryRow(context.Background(),
		`SELECT count(*) FROM applications WHERE project_id = $1`, projectID).Scan(&count))
	assert.Equal(t, 0, count, "no refused call may have written an application")
}

// TestMCP_CreateDatabase_ProvisionsAndRefusesOther covers the happy path, the
// async note, and the deliberate exclusion of the "other" engine.
func TestMCP_CreateDatabase_ProvisionsAndRefusesOther(t *testing.T) {
	resetDB(t)
	adminToken := env.SetupAdmin(t, "mcp-create-db@test.com", "password123")
	projectID := extractID(env.CreateProject(t, adminToken, "P", "p")["id"])
	token := mintScoped(t, adminToken, []string{"write"})

	var got struct {
		Database map[string]any `json:"database"`
		Note     string         `json:"note"`
	}
	decodeToolResult(t, callToolArgs(t, token, "create_database", map[string]any{
		"project_id": projectID, "name": "maindb", "type": "postgres",
	}), &got)
	assert.Equal(t, "maindb", got.Database["name"])
	assert.Contains(t, got.Note, "get_database", "the reply must say provisioning is async")

	// The version defaulted rather than being left empty.
	var version string
	require.NoError(t, env.Pool.QueryRow(context.Background(),
		`SELECT version FROM databases WHERE project_id = $1 AND name = 'maindb'`, projectID).Scan(&version))
	assert.NotEmpty(t, version, "an omitted version must take the engine default")

	// ⚠️ "other" is excluded with a reason, not reported as merely invalid.
	msg := toolErrorText(t, callToolArgs(t, token, "create_database", map[string]any{
		"project_id": projectID, "name": "custom", "type": "other",
	}))
	assert.Contains(t, msg, "dashboard", "the refusal must say where to do it instead")

	assert.Contains(t, toolErrorText(t, callToolArgs(t, token, "create_database", map[string]any{
		"project_id": projectID, "name": "nope", "type": "cassandra",
	})), "postgres, mysql, redis, mongo")

	// Credentials are never echoed back by the tool.
	assert.NotContains(t, fmt.Sprint(got.Database), "password")
}

// TestMCP_Create_ScopeAndPin is the security half for both tools: read and
// deploy scopes cannot reach them, and a pinned token cannot create inside
// another project — with the positive control that it still can inside its own.
func TestMCP_Create_ScopeAndPin(t *testing.T) {
	resetDB(t)
	adminToken := env.SetupAdmin(t, "mcp-create-scope@test.com", "password123")
	adminUserID := extractID(mustAuthMe(t, adminToken)["id"])
	mine := extractID(env.CreateProject(t, adminToken, "Mine", "mine")["id"])
	other := extractID(env.CreateProject(t, adminToken, "Other", "other")["id"])

	calls := map[string]map[string]any{
		"create_application": {"type": "image", "build_type": "image", "source_image": "nginx:1.27-alpine"},
		"create_database":    {"type": "postgres"},
	}

	for _, scope := range []string{"read", "deploy"} {
		tok := mintScoped(t, adminToken, []string{scope})
		for name, extra := range calls {
			args := map[string]any{"project_id": mine, "name": "ScopeProbe"}
			for k, v := range extra {
				args[k] = v
			}
			assert.Equal(t, fmt.Sprintf("the %s tool requires the %q scope; this token has: %s", name, "write", scope),
				rpcError(t, callToolArgs(t, tok, name, args)))
		}
	}

	pinned := createPinnedAPIToken(t, adminUserID, mine, []string{"write"})
	for name, extra := range calls {
		own := map[string]any{"project_id": mine, "name": "PinOwn-" + name}
		foreign := map[string]any{"project_id": other, "name": "PinForeign-" + name}
		for k, v := range extra {
			own[k], foreign[k] = v, v
		}
		t.Run(name+" creates in its own project", func(t *testing.T) {
			decodeToolResult(t, callToolArgs(t, pinned, name, own), &struct{}{})
		})
		t.Run(name+" refuses another project", func(t *testing.T) {
			assert.Equal(t, "access denied", toolErrorText(t, callToolArgs(t, pinned, name, foreign)))
		})
	}

	var apps, dbs int
	require.NoError(t, env.Pool.QueryRow(context.Background(),
		`SELECT count(*) FROM applications WHERE project_id = $1`, other).Scan(&apps))
	require.NoError(t, env.Pool.QueryRow(context.Background(),
		`SELECT count(*) FROM databases WHERE project_id = $1`, other).Scan(&dbs))
	assert.Equal(t, 0, apps, "a refused create wrote into another project")
	assert.Equal(t, 0, dbs, "a refused create wrote into another project")
}

// TestMCP_Create_AreAuditedAgainstTheToken — the operator's record of what the
// assistant made.
func TestMCP_Create_AreAuditedAgainstTheToken(t *testing.T) {
	resetDB(t)
	adminToken := env.SetupAdmin(t, "mcp-create-audit@test.com", "password123")
	projectID := extractID(env.CreateProject(t, adminToken, "P", "p")["id"])
	token := mintScoped(t, adminToken, []string{"write"})
	base := newAuditedMCP(t)

	decodeToolResult(t, callToolAt(t, base, token, "create_application",
		createAppArgs(projectID, "Audited App", nil)), &struct{}{})
	decodeToolResult(t, callToolAt(t, base, token, "create_database", map[string]any{
		"project_id": projectID, "name": "auditeddb", "type": "postgres",
	}), &struct{}{})

	for _, tc := range []struct{ action, resourceType, needle string }{
		{"create_application", "application", "Audited App"},
		{"create_database", "database", "auditeddb"},
	} {
		t.Run(tc.action, func(t *testing.T) {
			var tokenID *string
			var details string
			require.Eventually(t, func() bool {
				return env.Pool.QueryRow(context.Background(),
					"SELECT token_id::text, COALESCE(details::text,'{}') FROM audit_logs WHERE action = $1 AND resource_type = $2",
					tc.action, tc.resourceType).Scan(&tokenID, &details) == nil
			}, 2*time.Second, 10*time.Millisecond, "%s must write an audit row", tc.action)
			require.NotNil(t, tokenID, "attributed to the token, not read as a human action")
			assert.Contains(t, details, tc.needle, "the row must name what was created")
		})
	}
}

// TestMCP_CreateDomain_ServesHTTPAndNeverRequestsACertificate is the point of
// the tool's shape: TLS is off and not a parameter.
//
// ⚠️ Requesting a certificate is the one thing a domain write does that leaves
// the install — it makes the server talk to Let's Encrypt, which keeps
// per-account state, and a hostname that does not resolve here yet spends that
// hostname's failed-validation budget, delaying the certificate that would
// otherwise have worked. A person adding a domain has just decided where DNS
// points; a tool call has not.
func TestMCP_CreateDomain_ServesHTTPAndNeverRequestsACertificate(t *testing.T) {
	resetDB(t)
	adminToken := env.SetupAdmin(t, "mcp-domain@test.com", "password123")
	projectID := extractID(env.CreateProject(t, adminToken, "P", "p")["id"])
	appID := extractID(minimalApp(t, adminToken, projectID)["id"])
	token := mintScoped(t, adminToken, []string{"write"})

	var got struct {
		Domain map[string]any `json:"domain"`
		Note   string         `json:"note"`
	}
	decodeToolResult(t, callToolArgs(t, token, "create_domain", map[string]any{
		"application_id": appID, "hostname": "App.Example.COM",
	}), &got)

	assert.Equal(t, "app.example.com", got.Domain["hostname"], "the hostname is lowercased")
	assert.Equal(t, "off", got.Domain["ssl_mode"], "TLS must be off")
	assert.Contains(t, got.Note, "HTTPS", "the reply must say HTTPS is not on")

	var sslMode string
	var sslEnabled, forceHTTPS bool
	require.NoError(t, env.Pool.QueryRow(context.Background(),
		`SELECT ssl_mode, ssl_enabled, force_https FROM domains WHERE application_id = $1`, appID).
		Scan(&sslMode, &sslEnabled, &forceHTTPS))
	assert.Equal(t, "off", sslMode)
	assert.False(t, sslEnabled, "no certificate may be requested")
	assert.False(t, forceHTTPS, "force_https is meaningless with TLS off")

	// ⚠️ The schema is closed, so a caller cannot turn TLS on by passing the
	// field anyway — which is what makes "not a parameter" true rather than
	// merely default.
	for _, field := range []string{"ssl_mode", "ssl_enabled", "certificate_id", "advanced_config"} {
		assert.Contains(t, toolErrorText(t, callToolArgs(t, token, "create_domain", map[string]any{
			"application_id": appID, "hostname": "other.example.com", field: "x",
		})), "unexpected additional properties", "%s must not be accepted", field)
	}
}

// TestMCP_CreateDomain_RefusesDuplicatesAndBadHostnames: a duplicate names the
// collision, and an invalid hostname is refused before anything is written.
func TestMCP_CreateDomain_RefusesDuplicatesAndBadHostnames(t *testing.T) {
	resetDB(t)
	adminToken := env.SetupAdmin(t, "mcp-domain-dup@test.com", "password123")
	projectID := extractID(env.CreateProject(t, adminToken, "P", "p")["id"])
	appID := extractID(minimalApp(t, adminToken, projectID)["id"])
	token := mintScoped(t, adminToken, []string{"write"})

	decodeToolResult(t, callToolArgs(t, token, "create_domain", map[string]any{
		"application_id": appID, "hostname": "dup.example.com",
	}), &struct{}{})

	assert.Contains(t, toolErrorText(t, callToolArgs(t, token, "create_domain", map[string]any{
		"application_id": appID, "hostname": "dup.example.com",
	})), "already configured")

	for _, bad := range []string{"not a hostname", "no-tld", "http://example.com", "exa mple.com"} {
		assert.Contains(t, toolErrorText(t, callToolArgs(t, token, "create_domain", map[string]any{
			"application_id": appID, "hostname": bad,
		})), "invalid hostname", "%q must be refused", bad)
	}

	var count int
	require.NoError(t, env.Pool.QueryRow(context.Background(),
		`SELECT count(*) FROM domains WHERE application_id = $1`, appID).Scan(&count))
	assert.Equal(t, 1, count, "only the one valid hostname may have been written")
}

// TestMCP_CreateDomain_ScopeAndPin: write-only, and a pinned token cannot add a
// hostname to another project's application — with the positive control.
func TestMCP_CreateDomain_ScopeAndPin(t *testing.T) {
	resetDB(t)
	adminToken := env.SetupAdmin(t, "mcp-domain-scope@test.com", "password123")
	adminUserID := extractID(mustAuthMe(t, adminToken)["id"])
	mine := extractID(env.CreateProject(t, adminToken, "Mine", "mine")["id"])
	mineApp := extractID(minimalApp(t, adminToken, mine)["id"])
	other := extractID(env.CreateProject(t, adminToken, "Other", "other")["id"])
	otherApp := extractID(minimalApp(t, adminToken, other)["id"])

	for _, scope := range []string{"read", "deploy"} {
		tok := mintScoped(t, adminToken, []string{scope})
		assert.Equal(t, fmt.Sprintf("the create_domain tool requires the %q scope; this token has: %s", "write", scope),
			rpcError(t, callToolArgs(t, tok, "create_domain", map[string]any{
				"application_id": mineApp, "hostname": "scope.example.com",
			})))
	}

	pinned := createPinnedAPIToken(t, adminUserID, mine, []string{"write"})
	decodeToolResult(t, callToolArgs(t, pinned, "create_domain", map[string]any{
		"application_id": mineApp, "hostname": "own.example.com",
	}), &struct{}{})

	assert.Equal(t, "access denied", toolErrorText(t, callToolArgs(t, pinned, "create_domain", map[string]any{
		"application_id": otherApp, "hostname": "foreign.example.com",
	})))

	var count int
	require.NoError(t, env.Pool.QueryRow(context.Background(),
		`SELECT count(*) FROM domains WHERE application_id = $1`, otherApp).Scan(&count))
	assert.Equal(t, 0, count, "a refused call added a hostname to another project's application")
}
