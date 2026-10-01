package handler_test

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/weiliang79/belune/internal/store/generated"
)

// richGitApp is a git application with every editable field and several
// non-editable ones populated, so a tool that drops any of them shows up as a
// difference rather than as an empty column staying empty.
func richGitApp(t *testing.T, token, projectID string) (appID string, row generated.Application) {
	t.Helper()
	created := env.CreateApplication(t, token, projectID, map[string]any{
		"name":            "Rich App",
		"type":            "git",
		"build_type":      "dockerfile",
		"source_repo":     "https://github.com/example/repo.git",
		"branch":          "main",
		"root_directory":  "apps/web",
		"dockerfile_path": "Dockerfile.prod",
	})
	appID = extractID(created["id"])
	// Not settable through create (or owned by other endpoints): written
	// directly so the update has something to destroy.
	_, err := env.Pool.Exec(context.Background(), `
		UPDATE applications
		   SET build_type_override = 'railpack', builder_image = 'example/builder:1',
		       health_check_path = '/up', cpu_limit = 0.5, memory_limit = 268435456
		 WHERE id = $1`, appID)
	require.NoError(t, err)

	// A connected provider account and stored git credentials: the two
	// attachments an update has no business touching, and the two a tool that
	// rebuilt the row from its own arguments would silently detach.
	var integrationID string
	require.NoError(t, env.Pool.QueryRow(context.Background(), `
		INSERT INTO git_integrations (provider, account_login, config_encrypted, created_by)
		VALUES ('github', 'octo', '\x00', $1) RETURNING id::text`,
		extractID(mustAuthMe(t, token)["id"])).Scan(&integrationID))
	_, err = env.Pool.Exec(context.Background(),
		`UPDATE applications SET git_integration_id = $2, git_credentials_encrypted = '\xdeadbeef' WHERE id = $1`,
		appID, integrationID)
	require.NoError(t, err)
	return appID, getApp(t, appID)
}

func getApp(t *testing.T, appID string) generated.Application {
	t.Helper()
	var id pgtype.UUID
	require.NoError(t, id.Scan(appID))
	app, err := env.Queries.GetApplication(context.Background(), id)
	require.NoError(t, err)
	return app
}

func updateArgs(appID string, fields map[string]any) map[string]any {
	args := map[string]any{"application_id": appID}
	for k, v := range fields {
		args[k] = v
	}
	return args
}

// The release's likeliest harm: an assistant mentions one field and the tool
// writes a whole row. Setting ONE field must leave every other column of the
// row byte-identical — the whole struct is compared, so a column nobody
// thought to list is covered too.
func TestMCP_UpdateApplication_OneFieldLeavesEveryOtherColumnUntouched(t *testing.T) {
	resetDB(t)
	adminToken := env.SetupAdmin(t, "mcp-update@test.com", "password123")
	project := env.CreateProject(t, adminToken, "P", "p")
	appID, before := richGitApp(t, adminToken, extractID(project["id"]))
	token := mintScoped(t, adminToken, []string{"write"})

	var got struct {
		ChangedFields []string `json:"changed_fields"`
	}
	decodeToolResult(t, callToolArgs(t, token, "update_application", updateArgs(appID, map[string]any{"branch": "develop"})), &got)
	assert.Equal(t, []string{"branch"}, got.ChangedFields)

	after := getApp(t, appID)
	want := before
	want.Branch = pgtype.Text{String: "develop", Valid: true}
	want.AutoDeployBranch = want.Branch // moves in lockstep with branch, by the service's rule
	// Bookkeeping that a real edit legitimately moves.
	want.UpdatedAt = after.UpdatedAt
	want.SourceChangedAt = after.SourceChangedAt
	assert.True(t, after.SourceChangedAt.Valid, "a source edit must stamp the change marker")
	assert.Equal(t, want, after, "every column other than branch must be untouched")
}

// Each editable field, set alone, moves only itself.
func TestMCP_UpdateApplication_EachFieldAloneMovesOnlyItself(t *testing.T) {
	resetDB(t)
	adminToken := env.SetupAdmin(t, "mcp-update-each@test.com", "password123")
	project := env.CreateProject(t, adminToken, "P", "p")
	appID, _ := richGitApp(t, adminToken, extractID(project["id"]))
	token := mintScoped(t, adminToken, []string{"write"})

	for field, value := range map[string]string{
		"name":                "Renamed",
		"source_repo":         "https://github.com/example/other.git",
		"root_directory":      "apps/api",
		"dockerfile_path":     "Dockerfile.dev",
		"build_type_override": "buildpacks",
		"builder_image":       "example/builder:2",
	} {
		before := getApp(t, appID)
		var got struct {
			ChangedFields []string `json:"changed_fields"`
		}
		decodeToolResult(t, callToolArgs(t, token, "update_application", updateArgs(appID, map[string]any{field: value})), &got)
		assert.Equal(t, []string{field}, got.ChangedFields, field)

		after := getApp(t, appID)
		// Everything outside the one field and the bookkeeping must match.
		after.UpdatedAt, before.UpdatedAt = pgtype.Timestamptz{}, pgtype.Timestamptz{}
		after.SourceChangedAt, before.SourceChangedAt = pgtype.Timestamptz{}, pgtype.Timestamptz{}
		after.ConfigChangedAt, before.ConfigChangedAt = pgtype.Timestamptz{}, pgtype.Timestamptz{}
		switch field {
		case "name":
			after.Name = before.Name
		case "source_repo":
			after.SourceRepo = before.SourceRepo
		case "root_directory":
			after.RootDirectory = before.RootDirectory
		case "dockerfile_path":
			after.DockerfilePath = before.DockerfilePath
		case "build_type_override":
			after.BuildTypeOverride = before.BuildTypeOverride
		case "builder_image":
			after.BuilderImage = before.BuilderImage
		}
		assert.Equal(t, before, after, "setting only %s must not move any other column", field)
	}
}

// Absent keeps; empty string clears; an explicit null is "not mentioned", the
// safe reading of an assistant that sends null for a field it is skipping.
func TestMCP_UpdateApplication_AbsentKeepsEmptyClearsNullIsAbsent(t *testing.T) {
	resetDB(t)
	adminToken := env.SetupAdmin(t, "mcp-update-clear@test.com", "password123")
	project := env.CreateProject(t, adminToken, "P", "p")
	appID, _ := richGitApp(t, adminToken, extractID(project["id"]))
	token := mintScoped(t, adminToken, []string{"write"})

	// null alone is nothing at all.
	assert.Contains(t, toolErrorText(t, callToolArgs(t, token, "update_application",
		updateArgs(appID, map[string]any{"branch": nil}))), "no fields to update")
	assert.Equal(t, "main", getApp(t, appID).Branch.String)

	// null beside a real field must not clear the null one.
	var got struct{ ChangedFields []string }
	decodeToolResult(t, callToolArgs(t, token, "update_application",
		updateArgs(appID, map[string]any{"branch": nil, "root_directory": ""})), &got)
	app := getApp(t, appID)
	assert.Equal(t, "main", app.Branch.String, "null is not a request to clear")
	assert.False(t, app.RootDirectory.Valid, "an empty string clears root_directory")
	assert.Equal(t, "Dockerfile.prod", app.DockerfilePath.String, "an omitted field is kept")
}

// A save the validator would refuse over REST must be refused here, with the
// row untouched — and with the message that says what to do next.
func TestMCP_UpdateApplication_RefusesIncoherentSourceWithoutWriting(t *testing.T) {
	resetDB(t)
	adminToken := env.SetupAdmin(t, "mcp-update-invalid@test.com", "password123")
	project := env.CreateProject(t, adminToken, "P", "p")
	appID, before := richGitApp(t, adminToken, extractID(project["id"]))
	token := mintScoped(t, adminToken, []string{"write"})

	assert.Contains(t, toolErrorText(t, callToolArgs(t, token, "update_application",
		updateArgs(appID, map[string]any{"source_repo": ""}))), "needs a source_repo")
	assert.Contains(t, toolErrorText(t, callToolArgs(t, token, "update_application",
		updateArgs(appID, map[string]any{"source_image": "nginx"}))), "remove source_image")
	assert.Contains(t, toolErrorText(t, callToolArgs(t, token, "update_application",
		updateArgs(appID, map[string]any{"branch": "-rf"}))), "invalid branch")
	assert.Contains(t, toolErrorText(t, callToolArgs(t, token, "update_application",
		updateArgs(appID, map[string]any{"name": "  "}))), "name cannot be empty")

	assert.Equal(t, before, getApp(t, appID), "a refused update must not touch the row")
}

func TestMCP_UpdateApplication_ScopeAndPin(t *testing.T) {
	resetDB(t)
	adminToken := env.SetupAdmin(t, "mcp-update-scope@test.com", "password123")
	project := env.CreateProject(t, adminToken, "P", "p")
	projectID := extractID(project["id"])
	appID, before := richGitApp(t, adminToken, projectID)
	otherProject := env.CreateProject(t, adminToken, "Other", "other")
	otherApp, _ := richGitApp(t, adminToken, extractID(otherProject["id"]))

	args := updateArgs(appID, map[string]any{"branch": "develop"})
	for _, scope := range []string{"read", "deploy"} {
		tok := mintScoped(t, adminToken, []string{scope})
		assert.Equal(t, `the update_application tool requires the "write" scope; this token has: `+scope,
			rpcError(t, callToolArgs(t, tok, "update_application", args)))
	}
	assert.Equal(t, before, getApp(t, appID), "a refused call must not write")

	adminUserID := extractID(mustAuthMe(t, adminToken)["id"])
	pinned := createPinnedAPIToken(t, adminUserID, projectID, []string{"write"})
	assert.Equal(t, "access denied", toolErrorText(t, callToolArgs(t, pinned, "update_application",
		updateArgs(otherApp, map[string]any{"branch": "develop"}))))
}

func TestMCP_UpdateApplication_IsAuditedAgainstTheToken(t *testing.T) {
	resetDB(t)
	adminToken := env.SetupAdmin(t, "mcp-update-audit@test.com", "password123")
	project := env.CreateProject(t, adminToken, "P", "p")
	appID, _ := richGitApp(t, adminToken, extractID(project["id"]))
	token := mintScoped(t, adminToken, []string{"write"})
	base := newAuditedMCP(t)

	var got map[string]any
	decodeToolResult(t, callToolAt(t, base, token, "update_application",
		updateArgs(appID, map[string]any{"branch": "develop"})), &got)

	var tokenID *string
	var details string
	require.Eventually(t, func() bool {
		return env.Pool.QueryRow(context.Background(),
			"SELECT token_id::text, details::text FROM audit_logs WHERE action = 'update_application' AND resource_id = $1", appID,
		).Scan(&tokenID, &details) == nil
	}, 2*time.Second, 10*time.Millisecond, "the tool must write an audit row")
	require.NotNil(t, tokenID)
	assert.Contains(t, details, "branch")
}
