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

func changeSource(t *testing.T, token, projectID, appID string, body map[string]any) *http.Response {
	t.Helper()
	return env.DoRequest(t, "POST",
		fmt.Sprintf("/api/projects/%s/applications/%s/change-source", projectID, appID),
		body, testutil.AuthHeader(token))
}

// The reason this feature exists: delete-and-recreate destroys everything that
// belongs to the application rather than to its source. A switch must keep all
// of it.
func TestChangeSource_PreservesEverythingButTheSource(t *testing.T) {
	resetDB(t)
	token := env.SetupAdmin(t, "admin@test.com", "password123")
	project := env.CreateProject(t, token, "Test Project", "test-project")
	projectID := extractID(project["id"])
	app := env.CreateApplication(t, token, projectID, map[string]any{
		"name": "Switcher", "type": "git", "build_type": "railpack",
		"source_repo": "https://github.com/test/repo", "branch": "main",
		"git_token": "ghp_secret",
	})
	appID := extractID(app["id"])
	ctx := context.Background()

	var appUUID pgtype.UUID
	require.NoError(t, appUUID.Scan(appID))

	// Give it the full complement of things that must survive.
	resp := env.DoRequest(t, "POST",
		fmt.Sprintf("/api/projects/%s/applications/%s/domains", projectID, appID),
		map[string]any{"hostname": "keep-me.example.com", "ssl_enabled": true},
		testutil.AuthHeader(token))
	require.Equal(t, http.StatusCreated, resp.StatusCode)
	resp.Body.Close()

	resp = env.DoRequest(t, "PUT",
		fmt.Sprintf("/api/projects/%s/applications/%s/env", projectID, appID),
		map[string]any{"vars": []map[string]any{{"key": "KEEP", "value": "yes"}}},
		testutil.AuthHeader(token))
	require.Equal(t, http.StatusOK, resp.StatusCode)
	resp.Body.Close()

	resp = env.DoRequest(t, "POST",
		fmt.Sprintf("/api/projects/%s/applications/%s/deploy-hook", projectID, appID),
		map[string]any{}, testutil.AuthHeader(token))
	require.Equal(t, http.StatusOK, resp.StatusCode)
	resp.Body.Close()

	_, err := env.Queries.CreateDeployment(ctx, generated.CreateDeploymentParams{
		ApplicationID: appUUID, Status: "success", TriggeredBy: "manual",
	})
	require.NoError(t, err)

	// Switch to an image.
	resp = changeSource(t, token, projectID, appID, map[string]any{
		"type": "image", "source_image": "nginx:1.27",
	})
	require.Equal(t, http.StatusOK, resp.StatusCode)
	result := testutil.ReadJSON(t, resp)
	assert.Equal(t, "image", result["type"])
	assert.Equal(t, "image", result["build_type"])

	// The source moved.
	var (
		typ, buildType string
		repo, image    *string
		branch         *string
		hasGitCreds    bool
		hasWebhook     bool
		hasDeployHook  bool
		sourceChanged  bool
	)
	require.NoError(t, env.Pool.QueryRow(ctx, `
		SELECT type, build_type, source_repo, source_image, branch,
		       git_credentials_encrypted IS NOT NULL,
		       webhook_secret_encrypted IS NOT NULL,
		       deploy_hook_token_hash IS NOT NULL,
		       source_changed_at IS NOT NULL
		  FROM applications WHERE id = $1`, appID).
		Scan(&typ, &buildType, &repo, &image, &branch,
			&hasGitCreds, &hasWebhook, &hasDeployHook, &sourceChanged))

	assert.Equal(t, "image", typ)
	assert.Equal(t, "image", buildType)
	assert.Nil(t, repo, "the repository must be cleared, or a push webhook could still match it")
	assert.Nil(t, branch)
	require.NotNil(t, image)
	assert.Equal(t, "nginx:1.27", *image)
	assert.False(t, hasGitCreds, "git credentials authenticate against a repo this app no longer has")
	assert.False(t, hasWebhook, "the push secret can never fire again")

	// The deploy hook is source-agnostic — it just triggers a deploy.
	assert.True(t, hasDeployHook, "the deploy hook must survive the switch")

	// The running container is still the pre-switch image, so this reads
	// "Deploy to apply" rather than being silently marked current.
	assert.True(t, sourceChanged)

	// Everything owned by the application is intact.
	var domains, envVars, deployments int
	require.NoError(t, env.Pool.QueryRow(ctx,
		`SELECT count(*) FROM domains WHERE application_id = $1`, appID).Scan(&domains))
	require.NoError(t, env.Pool.QueryRow(ctx,
		`SELECT count(*) FROM env_vars WHERE application_id = $1`, appID).Scan(&envVars))
	require.NoError(t, env.Pool.QueryRow(ctx,
		`SELECT count(*) FROM deployments WHERE application_id = $1`, appID).Scan(&deployments))
	assert.Equal(t, 1, domains, "domains and their certificates must survive")
	assert.Equal(t, 1, envVars)
	assert.Equal(t, 1, deployments, "deployment history must survive")
}

func TestChangeSource_ImageToGit(t *testing.T) {
	resetDB(t)
	token := env.SetupAdmin(t, "admin@test.com", "password123")
	project := env.CreateProject(t, token, "Test Project", "test-project")
	projectID := extractID(project["id"])
	app := env.CreateApplication(t, token, projectID, map[string]any{
		"name": "Switcher", "type": "image", "build_type": "image",
		"source_image": "nginx:1.25",
	})
	appID := extractID(app["id"])

	resp := changeSource(t, token, projectID, appID, map[string]any{
		"type": "git", "source_repo": "https://github.com/test/repo",
		"branch": "develop", "build_type": "dockerfile",
	})
	require.Equal(t, http.StatusOK, resp.StatusCode)
	resp.Body.Close()

	var typ, buildType string
	var image *string
	var branch, autoBranch string
	require.NoError(t, env.Pool.QueryRow(context.Background(), `
		SELECT type, build_type, source_image, COALESCE(branch,''), COALESCE(auto_deploy_branch,'')
		  FROM applications WHERE id = $1`, appID).
		Scan(&typ, &buildType, &image, &branch, &autoBranch))

	assert.Equal(t, "git", typ)
	assert.Equal(t, "dockerfile", buildType)
	assert.Nil(t, image, "the image reference must be cleared")
	// Both branch columns move together, as everywhere else.
	assert.Equal(t, "develop", branch)
	assert.Equal(t, "develop", autoBranch)
}

func TestChangeSource_Rejections(t *testing.T) {
	resetDB(t)
	token := env.SetupAdmin(t, "admin@test.com", "password123")
	project := env.CreateProject(t, token, "Test Project", "test-project")
	projectID := extractID(project["id"])
	app := env.CreateApplication(t, token, projectID, map[string]any{
		"name": "Switcher", "type": "image", "build_type": "image",
		"source_image": "nginx:1.25",
	})
	appID := extractID(app["id"])

	cases := map[string]struct {
		body map[string]any
		want int
	}{
		"already that type": {
			map[string]any{"type": "image", "source_image": "nginx:1.27"},
			http.StatusBadRequest,
		},
		"switching to git with no repo": {
			map[string]any{"type": "git"},
			http.StatusBadRequest,
		},
		"unknown type": {
			map[string]any{"type": "compose"},
			http.StatusBadRequest,
		},
		"invalid branch name": {
			map[string]any{
				"type": "git", "source_repo": "https://github.com/a/b",
				"branch": "--upload-pack=evil",
			},
			http.StatusBadRequest,
		},
	}

	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			resp := changeSource(t, token, projectID, appID, c.body)
			defer resp.Body.Close()
			assert.Equal(t, c.want, resp.StatusCode)
		})
	}

	// None of them changed anything.
	var typ string
	require.NoError(t, env.Pool.QueryRow(context.Background(),
		`SELECT type FROM applications WHERE id = $1`, appID).Scan(&typ))
	assert.Equal(t, "image", typ)
}

// The worker re-reads the application row at several stages, so switching
// mid-deploy could build one source and deploy the other.
func TestChangeSource_BlockedDuringDeploy(t *testing.T) {
	resetDB(t)
	token := env.SetupAdmin(t, "admin@test.com", "password123")
	project := env.CreateProject(t, token, "Test Project", "test-project")
	projectID := extractID(project["id"])
	app := env.CreateApplication(t, token, projectID, map[string]any{
		"name": "Switcher", "type": "image", "build_type": "image",
		"source_image": "nginx:1.25",
	})
	appID := extractID(app["id"])

	var appUUID pgtype.UUID
	require.NoError(t, appUUID.Scan(appID))
	_, err := env.Queries.CreateDeployment(context.Background(), generated.CreateDeploymentParams{
		ApplicationID: appUUID, Status: "building", TriggeredBy: "manual",
	})
	require.NoError(t, err)

	resp := changeSource(t, token, projectID, appID, map[string]any{
		"type": "git", "source_repo": "https://github.com/test/repo",
	})
	defer resp.Body.Close()
	assert.Equal(t, http.StatusConflict, resp.StatusCode)
}

// Preview children are git-only by construction — each exists because a branch
// matched a pattern — so switching the parent to an image would orphan them.
func TestChangeSource_BlockedWithPreviewChildren(t *testing.T) {
	resetDB(t)
	token := env.SetupAdmin(t, "admin@test.com", "password123")
	project := env.CreateProject(t, token, "Test Project", "test-project")
	projectID := extractID(project["id"])
	app := env.CreateApplication(t, token, projectID, map[string]any{
		"name": "Parent", "type": "git", "build_type": "railpack",
		"source_repo": "https://github.com/test/repo",
	})
	appID := extractID(app["id"])

	var projectUUID, appUUID pgtype.UUID
	require.NoError(t, projectUUID.Scan(projectID))
	require.NoError(t, appUUID.Scan(appID))

	_, err := env.Queries.CreatePreviewApplication(context.Background(), generated.CreatePreviewApplicationParams{
		ProjectID:           projectUUID,
		Name:                "Parent (feature-x)",
		Slug:                "parent-feature-x",
		Type:                "git",
		SourceRepo:          pgtype.Text{String: "https://github.com/test/repo", Valid: true},
		BuildType:           "railpack",
		ParentApplicationID: appUUID,
		Branch:              pgtype.Text{String: "feature-x", Valid: true},
	})
	require.NoError(t, err)

	resp := changeSource(t, token, projectID, appID, map[string]any{
		"type": "image", "source_image": "nginx:1.27",
	})
	defer resp.Body.Close()
	assert.Equal(t, http.StatusConflict, resp.StatusCode)

	var typ string
	require.NoError(t, env.Pool.QueryRow(context.Background(),
		`SELECT type FROM applications WHERE id = $1`, appID).Scan(&typ))
	assert.Equal(t, "git", typ, "the parent must be untouched")
}
