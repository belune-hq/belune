package handler_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/belune-hq/belune/internal/testutil"
)

// deployHookFixture creates a project + application of the given type and
// returns their IDs.
func deployHookFixture(t *testing.T, token, appType string) (projectID, appID string) {
	t.Helper()
	project := env.CreateProject(t, token, "Hook Project", "hook-project")
	projectID = extractID(project["id"])

	body := map[string]any{"name": "Hook App", "type": appType}
	if appType == "git" {
		body["build_type"] = "dockerfile"
		body["source_repo"] = "https://github.com/test/hook-repo"
	} else {
		body["build_type"] = "image"
		body["source_image"] = "nginx:1.27"
	}
	app := env.CreateApplication(t, token, projectID, body)
	return projectID, extractID(app["id"])
}

// generateHook calls the generate endpoint and returns the fresh token.
func generateHook(t *testing.T, authToken, projectID, appID string) string {
	t.Helper()
	resp := env.DoRequest(t, "POST",
		fmt.Sprintf("/api/projects/%s/applications/%s/deploy-hook", projectID, appID),
		nil, testutil.AuthHeader(authToken))
	require.Equal(t, http.StatusOK, resp.StatusCode)
	result := testutil.ReadJSON(t, resp)
	require.Equal(t, true, result["enabled"])
	hookToken, _ := result["token"].(string)
	require.NotEmpty(t, hookToken)
	assert.Equal(t, "/api/webhooks/deploy/"+hookToken, result["path"])
	return hookToken
}

// seedSuccessfulDeployment inserts a succeeded deployment so an image app has a
// current image tag for the hook's reload path to redeploy.
func seedSuccessfulDeployment(t *testing.T, appID, imageTag string) {
	t.Helper()
	_, err := env.Pool.Exec(context.Background(),
		`INSERT INTO deployments (application_id, status, triggered_by, image_tag)
		 VALUES ($1, 'success', 'manual', $2)`, appID, imageTag)
	require.NoError(t, err)
}

func TestDeployHook_GenerateRevealAndStatus(t *testing.T) {
	resetDB(t)
	authToken := env.SetupAdmin(t, "admin@test.com", "password123")
	projectID, appID := deployHookFixture(t, authToken, "git")
	statusPath := fmt.Sprintf("/api/projects/%s/applications/%s/deploy-hook", projectID, appID)

	// Disabled until generated.
	resp := env.DoRequest(t, "GET", statusPath, nil, testutil.AuthHeader(authToken))
	require.Equal(t, http.StatusOK, resp.StatusCode)
	result := testutil.ReadJSON(t, resp)
	assert.Equal(t, false, result["enabled"])
	assert.Empty(t, result["token"])

	hookToken := generateHook(t, authToken, projectID, appID)

	// Status now reports enabled but must never carry the token itself.
	resp = env.DoRequest(t, "GET", statusPath, nil, testutil.AuthHeader(authToken))
	require.Equal(t, http.StatusOK, resp.StatusCode)
	result = testutil.ReadJSON(t, resp)
	assert.Equal(t, true, result["enabled"])
	assert.Empty(t, result["token"])

	// Reveal round-trips the stored token through the keyring.
	resp = env.DoRequest(t, "GET", statusPath+"/reveal", nil, testutil.AuthHeader(authToken))
	require.Equal(t, http.StatusOK, resp.StatusCode)
	result = testutil.ReadJSON(t, resp)
	assert.Equal(t, hookToken, result["token"])
}

func TestDeployHook_TriggerGitApp(t *testing.T) {
	resetDB(t)
	authToken := env.SetupAdmin(t, "admin@test.com", "password123")
	projectID, appID := deployHookFixture(t, authToken, "git")
	hookToken := generateHook(t, authToken, projectID, appID)

	env.Asynq.Tasks = nil
	resp := env.DoRequest(t, "POST", "/api/webhooks/deploy/"+hookToken, nil, nil)
	require.Equal(t, http.StatusAccepted, resp.StatusCode)
	assert.Equal(t, "queued", testutil.ReadJSON(t, resp)["status"])

	require.Len(t, env.Asynq.Tasks, 1)
	assert.Equal(t, "deploy", env.Asynq.Tasks[0].TypeName)

	// A git app builds from its configured branch, so the payload must NOT pin
	// an image tag — that would silently turn the deploy into a reload.
	var payload map[string]any
	require.NoError(t, json.Unmarshal(env.Asynq.Tasks[0].Payload, &payload))
	assert.Equal(t, appID, payload["application_id"])
	assert.Empty(t, payload["rollback_image_tag"])

	// The deployment is attributed to the hook, not to a manual click.
	var triggeredBy string
	require.NoError(t, env.Pool.QueryRow(context.Background(),
		`SELECT triggered_by FROM deployments WHERE application_id = $1`, appID).Scan(&triggeredBy))
	assert.Equal(t, "hook", triggeredBy)
}

func TestDeployHook_TriggerImageAppDoesNotPinOldDigest(t *testing.T) {
	resetDB(t)
	authToken := env.SetupAdmin(t, "admin@test.com", "password123")
	projectID, appID := deployHookFixture(t, authToken, "image")
	hookToken := generateHook(t, authToken, projectID, appID)
	// The app is already running a digest-pinned image.
	seedSuccessfulDeployment(t, appID, "nginx@sha256:oldoldold")

	env.Asynq.Tasks = nil
	resp := env.DoRequest(t, "POST", "/api/webhooks/deploy/"+hookToken, nil, nil)
	require.Equal(t, http.StatusAccepted, resp.StatusCode)

	require.Len(t, env.Asynq.Tasks, 1)
	var payload map[string]any
	require.NoError(t, json.Unmarshal(env.Asynq.Tasks[0].Payload, &payload))
	// The whole point of the hook is that CI just pushed a NEW image to the
	// configured tag. Pinning the running digest (the Reload path) would
	// redeploy the exact image the caller is trying to replace, so the payload
	// must leave it empty and let the deploy re-pull and re-pin.
	assert.Empty(t, payload["rollback_image_tag"])
}

func TestDeployHook_ImageAppWithoutPriorDeploymentStillDeploys(t *testing.T) {
	resetDB(t)
	authToken := env.SetupAdmin(t, "admin@test.com", "password123")
	projectID, appID := deployHookFixture(t, authToken, "image")
	hookToken := generateHook(t, authToken, projectID, appID)

	env.Asynq.Tasks = nil
	// Never deployed: the hook pulls source_image from scratch, exactly as a
	// first manual deploy would. There is nothing to be conflicted about.
	resp := env.DoRequest(t, "POST", "/api/webhooks/deploy/"+hookToken, nil, nil)
	assert.Equal(t, http.StatusAccepted, resp.StatusCode)
	resp.Body.Close()
	assert.Len(t, env.Asynq.Tasks, 1)
}

func TestDeployHook_UnknownTokenIs404(t *testing.T) {
	resetDB(t)
	authToken := env.SetupAdmin(t, "admin@test.com", "password123")
	projectID, appID := deployHookFixture(t, authToken, "git")
	generateHook(t, authToken, projectID, appID)

	env.Asynq.Tasks = nil
	for _, bad := range []string{"not-a-real-token", "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"} {
		resp := env.DoRequest(t, "POST", "/api/webhooks/deploy/"+bad, nil, nil)
		assert.Equal(t, http.StatusNotFound, resp.StatusCode, "token %q", bad)
		resp.Body.Close()
	}
	assert.Empty(t, env.Asynq.Tasks)
}

func TestDeployHook_RegenerateInvalidatesOldToken(t *testing.T) {
	resetDB(t)
	authToken := env.SetupAdmin(t, "admin@test.com", "password123")
	projectID, appID := deployHookFixture(t, authToken, "git")
	oldToken := generateHook(t, authToken, projectID, appID)
	newToken := generateHook(t, authToken, projectID, appID)
	require.NotEqual(t, oldToken, newToken)

	env.Asynq.Tasks = nil
	resp := env.DoRequest(t, "POST", "/api/webhooks/deploy/"+oldToken, nil, nil)
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
	resp.Body.Close()
	assert.Empty(t, env.Asynq.Tasks)

	resp = env.DoRequest(t, "POST", "/api/webhooks/deploy/"+newToken, nil, nil)
	assert.Equal(t, http.StatusAccepted, resp.StatusCode)
	resp.Body.Close()
	assert.Len(t, env.Asynq.Tasks, 1)
}

func TestDeployHook_DeleteDisablesTrigger(t *testing.T) {
	resetDB(t)
	authToken := env.SetupAdmin(t, "admin@test.com", "password123")
	projectID, appID := deployHookFixture(t, authToken, "git")
	hookToken := generateHook(t, authToken, projectID, appID)
	hookPath := fmt.Sprintf("/api/projects/%s/applications/%s/deploy-hook", projectID, appID)

	resp := env.DoRequest(t, "DELETE", hookPath, nil, testutil.AuthHeader(authToken))
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, false, testutil.ReadJSON(t, resp)["enabled"])

	env.Asynq.Tasks = nil
	resp = env.DoRequest(t, "POST", "/api/webhooks/deploy/"+hookToken, nil, nil)
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
	resp.Body.Close()
	assert.Empty(t, env.Asynq.Tasks)

	// Reveal has nothing left to hand back.
	resp = env.DoRequest(t, "GET", hookPath+"/reveal", nil, testutil.AuthHeader(authToken))
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
	resp.Body.Close()
}

func TestDeployHook_RequiresApplicationAccess(t *testing.T) {
	resetDB(t)
	adminToken := env.SetupAdmin(t, "admin@test.com", "password123")
	projectID, appID := deployHookFixture(t, adminToken, "git")

	env.DoRequest(t, "POST", "/api/users", map[string]string{
		"email": "member@test.com", "password": "password123", "role": "member",
	}, testutil.AuthHeader(adminToken)).Body.Close()
	memberToken := env.LoginAs(t, "member@test.com", "password123")

	hookPath := fmt.Sprintf("/api/projects/%s/applications/%s/deploy-hook", projectID, appID)
	for _, tc := range []struct{ method, path string }{
		{"GET", hookPath},
		{"GET", hookPath + "/reveal"},
		{"POST", hookPath},
		{"DELETE", hookPath},
	} {
		resp := env.DoRequest(t, tc.method, tc.path, nil, testutil.AuthHeader(memberToken))
		assert.Equal(t, http.StatusForbidden, resp.StatusCode, "%s %s", tc.method, tc.path)
		resp.Body.Close()
	}
}

// The user-facing "Branch" writes both columns: `branch` decides what we clone,
// `auto_deploy_branch` decides which pushes deploy. They must never drift —
// that divergence is what made a master-default repo silently never deploy.
func TestApplicationBranch_WritesBothColumns(t *testing.T) {
	resetDB(t)
	authToken := env.SetupAdmin(t, "admin@test.com", "password123")
	project := env.CreateProject(t, authToken, "Branch Project", "branch-project")
	projectID := extractID(project["id"])

	app := env.CreateApplication(t, authToken, projectID, map[string]any{
		"name":        "Branch App",
		"type":        "git",
		"build_type":  "dockerfile",
		"source_repo": "https://github.com/test/branch-repo",
		"branch":      "master",
	})
	appID := extractID(app["id"])

	var branch, autoBranch string
	require.NoError(t, env.Pool.QueryRow(context.Background(),
		`SELECT COALESCE(branch,''), COALESCE(auto_deploy_branch,'') FROM applications WHERE id = $1`,
		appID).Scan(&branch, &autoBranch))
	assert.Equal(t, "master", branch)
	assert.Equal(t, "master", autoBranch)

	// Updating moves both together.
	resp := env.DoRequest(t, "PUT",
		fmt.Sprintf("/api/projects/%s/applications/%s", projectID, appID),
		map[string]any{
			"name": "Branch App", "branch": "develop",
			"source_repo": "https://github.com/test/branch-repo",
		},
		testutil.AuthHeader(authToken))
	require.Equal(t, http.StatusOK, resp.StatusCode)
	resp.Body.Close()

	require.NoError(t, env.Pool.QueryRow(context.Background(),
		`SELECT COALESCE(branch,''), COALESCE(auto_deploy_branch,'') FROM applications WHERE id = $1`,
		appID).Scan(&branch, &autoBranch))
	assert.Equal(t, "develop", branch)
	assert.Equal(t, "develop", autoBranch)
}

// Empty means "the repository's default ref" and must store NULL — that is the
// state every pre-existing application is in, so blank preserves old behaviour.
func TestApplicationBranch_EmptyStoresNull(t *testing.T) {
	resetDB(t)
	authToken := env.SetupAdmin(t, "admin@test.com", "password123")
	project := env.CreateProject(t, authToken, "Branch Project", "branch-project")
	projectID := extractID(project["id"])

	app := env.CreateApplication(t, authToken, projectID, map[string]any{
		"name": "Default Branch App", "type": "git",
		"build_type": "dockerfile", "source_repo": "https://github.com/test/x",
	})

	var isNull bool
	require.NoError(t, env.Pool.QueryRow(context.Background(),
		`SELECT branch IS NULL FROM applications WHERE id = $1`, extractID(app["id"])).Scan(&isNull))
	assert.True(t, isNull, "blank branch must store NULL, not an empty string")
}

func TestApplicationBranch_RejectsInvalidName(t *testing.T) {
	resetDB(t)
	authToken := env.SetupAdmin(t, "admin@test.com", "password123")
	project := env.CreateProject(t, authToken, "Branch Project", "branch-project")
	projectID := extractID(project["id"])

	resp := env.DoRequest(t, "POST", fmt.Sprintf("/api/projects/%s/applications", projectID),
		map[string]any{
			"name": "Bad Branch", "type": "git", "build_type": "dockerfile",
			"source_repo": "https://github.com/test/x", "branch": "--upload-pack=evil",
		}, testutil.AuthHeader(authToken))
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
	resp.Body.Close()
}
