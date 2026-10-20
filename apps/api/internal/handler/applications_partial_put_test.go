package handler_test

import (
	"context"
	"fmt"
	"net/http"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/belune-hq/belune/internal/store/generated"
	"github.com/belune-hq/belune/internal/testutil"
)

// putPopulatedApp is a git application with every editable field populated, and
// a connected provider account and stored credentials attached, so a PUT that
// drops any of them shows as a difference rather than as an empty column
// staying empty.
func putPopulatedApp(t *testing.T, token, projectID string) (appID string, row generated.Application) {
	t.Helper()
	created := env.CreateApplication(t, token, projectID, map[string]any{
		"name":            "Populated App",
		"type":            "git",
		"build_type":      "dockerfile",
		"source_repo":     "https://github.com/example/repo.git",
		"branch":          "main",
		"root_directory":  "apps/web",
		"dockerfile_path": "Dockerfile.prod",
	})
	appID = extractID(created["id"])

	var integrationID string
	require.NoError(t, env.Pool.QueryRow(context.Background(), `
		INSERT INTO git_integrations (provider, account_login, config_encrypted, created_by)
		VALUES ('github', 'octo', '\x00', $1) RETURNING id::text`,
		extractID(mustAuthMe(t, token)["id"])).Scan(&integrationID))
	_, err := env.Pool.Exec(context.Background(), `
		UPDATE applications
		   SET build_type_override = 'railpack', builder_image = 'example/builder:1',
		       health_check_path = '/up', cpu_limit = 0.5, memory_limit = 268435456,
		       git_integration_id = $2, git_credentials_encrypted = '\xdeadbeef'
		 WHERE id = $1`, appID, integrationID)
	require.NoError(t, err)
	return appID, putFetchApp(t, appID)
}

func putFetchApp(t *testing.T, appID string) generated.Application {
	t.Helper()
	var id pgtype.UUID
	require.NoError(t, id.Scan(appID))
	app, err := env.Queries.GetApplication(context.Background(), id)
	require.NoError(t, err)
	return app
}

func putApp(t *testing.T, token, projectID, appID string, body map[string]any) *http.Response {
	t.Helper()
	return env.DoRequest(t, "PUT", fmt.Sprintf("/api/projects/%s/applications/%s", projectID, appID),
		body, testutil.AuthHeader(token))
}

// An omitted key must keep the stored value. Before this, every omitted source
// field decoded to "", the service stored "" as NULL, and a partial PUT cleared
// the rest with a 200. The WHOLE row is compared, so a column nobody listed is
// covered too.
func TestUpdateApplication_PartialPutLeavesEveryOtherColumnUntouched(t *testing.T) {
	resetDB(t)
	token := env.SetupAdmin(t, "partial-put@test.com", "password123")
	projectID := extractID(env.CreateProject(t, token, "P", "p")["id"])
	appID, before := putPopulatedApp(t, token, projectID)

	resp := putApp(t, token, projectID, appID, map[string]any{"branch": "develop"})
	require.Equal(t, http.StatusOK, resp.StatusCode)
	resp.Body.Close()

	after := putFetchApp(t, appID)
	want := before
	want.Branch = pgtype.Text{String: "develop", Valid: true}
	want.AutoDeployBranch = want.Branch // moves in lockstep with branch, by the service's rule
	want.UpdatedAt = after.UpdatedAt
	want.SourceChangedAt = after.SourceChangedAt
	assert.True(t, after.SourceChangedAt.Valid, "a source edit must stamp the change marker")
	assert.Equal(t, want, after, "every column other than branch must be untouched")
}

func TestUpdateApplication_EachSourceFieldAloneMovesOnlyItself(t *testing.T) {
	resetDB(t)
	token := env.SetupAdmin(t, "partial-put-each@test.com", "password123")
	projectID := extractID(env.CreateProject(t, token, "P", "p")["id"])
	appID, _ := putPopulatedApp(t, token, projectID)

	for field, value := range map[string]string{
		"source_repo":         "https://github.com/example/other.git",
		"root_directory":      "apps/api",
		"dockerfile_path":     "Dockerfile.dev",
		"build_type_override": "buildpacks",
		"builder_image":       "example/builder:2",
	} {
		before := putFetchApp(t, appID)
		resp := putApp(t, token, projectID, appID, map[string]any{field: value})
		require.Equal(t, http.StatusOK, resp.StatusCode, field)
		resp.Body.Close()

		after := putFetchApp(t, appID)
		after.UpdatedAt, before.UpdatedAt = pgtype.Timestamptz{}, pgtype.Timestamptz{}
		after.SourceChangedAt, before.SourceChangedAt = pgtype.Timestamptz{}, pgtype.Timestamptz{}
		switch field {
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

// An explicit empty string still clears — that is the contract the dashboard,
// which always sends the whole form, relies on.
func TestUpdateApplication_EmptyStringStillClears(t *testing.T) {
	resetDB(t)
	token := env.SetupAdmin(t, "partial-put-clear@test.com", "password123")
	projectID := extractID(env.CreateProject(t, token, "P", "p")["id"])
	appID, _ := putPopulatedApp(t, token, projectID)

	resp := putApp(t, token, projectID, appID, map[string]any{
		"dockerfile_path": "", "root_directory": "", "branch": "", "builder_image": "", "build_type_override": "",
	})
	require.Equal(t, http.StatusOK, resp.StatusCode)
	resp.Body.Close()

	app := putFetchApp(t, appID)
	assert.False(t, app.DockerfilePath.Valid)
	assert.False(t, app.RootDirectory.Valid)
	assert.False(t, app.Branch.Valid)
	assert.False(t, app.BuilderImage.Valid)
	assert.False(t, app.BuildTypeOverride.Valid)
	assert.Equal(t, "https://github.com/example/repo.git", app.SourceRepo.String, "an omitted field is still kept alongside cleared ones")
}

// Validation runs against the EFFECTIVE values, so an omitted key cannot be
// used to slip past it, and a refusal leaves the row alone.
func TestUpdateApplication_PartialPutIsValidatedAgainstTheStoredRow(t *testing.T) {
	resetDB(t)
	token := env.SetupAdmin(t, "partial-put-valid@test.com", "password123")
	projectID := extractID(env.CreateProject(t, token, "P", "p")["id"])
	appID, before := putPopulatedApp(t, token, projectID)

	resp := putApp(t, token, projectID, appID, map[string]any{"source_image": "nginx"})
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode, "a git app may not gain a source_image")
	resp.Body.Close()
	resp = putApp(t, token, projectID, appID, map[string]any{"source_repo": ""})
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode, "a git app may not lose its source_repo")
	resp.Body.Close()
	resp = putApp(t, token, projectID, appID, map[string]any{"branch": "-rf"})
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
	resp.Body.Close()

	assert.Equal(t, before, putFetchApp(t, appID), "a refused PUT must not touch the row")
}
