package handler_test

import (
	"context"
	"fmt"
	"net/http"
	"testing"

	"github.com/hibiken/asynq"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/belune-hq/belune/internal/status"
	"github.com/belune-hq/belune/internal/store/generated"
	"github.com/belune-hq/belune/internal/testutil"
)

func TestGetGlobalDeployments(t *testing.T) {
	resetDB(t)
	adminToken := env.SetupAdmin(t, "admin@test.com", "password123")

	// Create a member
	env.DoRequest(t, "POST", "/api/users", map[string]string{
		"email": "member@test.com", "password": "password123", "role": "member",
	}, testutil.AuthHeader(adminToken)).Body.Close()
	memberToken := env.LoginAs(t, "member@test.com", "password123")

	// Admin creates a project + app + triggers a deploy
	project := env.CreateProject(t, adminToken, "Admin Project", "admin-project")
	projectID := extractID(project["id"])
	app := env.CreateApplication(t, adminToken, projectID, map[string]any{
		"name": "App A", "type": "git", "build_type": "dockerfile",
		"source_repo": "https://github.com/test/repo",
	})
	appID := extractID(app["id"])
	resp := env.DoRequest(t, "POST", fmt.Sprintf("/api/projects/%s/applications/%s/deploy", projectID, appID), nil, testutil.AuthHeader(adminToken))
	assert.Equal(t, http.StatusAccepted, resp.StatusCode)
	resp.Body.Close()

	// Admin sees the deployment
	resp = env.DoRequest(t, "GET", "/api/deployments", nil, testutil.AuthHeader(adminToken))
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	rows := testutil.ReadJSONArray(t, resp)
	assert.Len(t, rows, 1)

	// Member (who owns no projects) sees none
	resp = env.DoRequest(t, "GET", "/api/deployments", nil, testutil.AuthHeader(memberToken))
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	memberRows := testutil.ReadJSONArray(t, resp)
	assert.Len(t, memberRows, 0)

	// Unauthenticated is rejected
	resp = env.DoRequest(t, "GET", "/api/deployments", nil, nil)
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
	resp.Body.Close()
}

func TestRollbackDeployment(t *testing.T) {
	resetDB(t)
	adminToken := env.SetupAdmin(t, "admin@test.com", "password123")

	project := env.CreateProject(t, adminToken, "Rollback Project", "rollback-project")
	projectID := extractID(project["id"])
	app := env.CreateApplication(t, adminToken, projectID, map[string]any{
		"name": "Rollback App", "type": "image", "build_type": "image",
		"source_image": "nginx:latest",
	})
	appID := extractID(app["id"])

	// Trigger a deploy to get a deployment record
	deployResp := env.DoRequest(t, "POST", fmt.Sprintf("/api/projects/%s/applications/%s/deploy", projectID, appID), nil, testutil.AuthHeader(adminToken))
	require.Equal(t, http.StatusAccepted, deployResp.StatusCode)
	deployment := testutil.ReadJSON(t, deployResp)
	deploymentID := extractID(deployment["id"])

	// Scan the deployment UUID
	var deployUUID pgtype.UUID
	require.NoError(t, deployUUID.Scan(deploymentID))

	// Simulate worker completing: set status=success and image_tag
	ctx := context.Background()
	_, err := env.Queries.UpdateDeploymentStatus(ctx, generated.UpdateDeploymentStatusParams{
		ID:     deployUUID,
		Status: status.DeploymentSuccess,
	})
	require.NoError(t, err)
	require.NoError(t, env.Queries.UpdateDeploymentImageTag(ctx, generated.UpdateDeploymentImageTagParams{
		ID:       deployUUID,
		ImageTag: pgtype.Text{String: "nginx:latest", Valid: true},
	}))

	// Clear enqueued tasks so we can detect the rollback task
	env.Asynq.Tasks = nil

	// Happy path: rollback to the successful deployment
	resp := env.DoRequest(t, "POST", fmt.Sprintf("/api/projects/%s/applications/%s/rollback", projectID, appID), map[string]string{
		"deployment_id": deploymentID,
	}, testutil.AuthHeader(adminToken))
	assert.Equal(t, http.StatusAccepted, resp.StatusCode)
	result := testutil.ReadJSON(t, resp)
	assert.Equal(t, "pending", result["status"])
	assert.Equal(t, "rollback", result["triggered_by"])
	require.Len(t, env.Asynq.Tasks, 1)
	assert.Equal(t, "deploy", env.Asynq.Tasks[0].TypeName)

	// Conflict: a deploy is genuinely in progress. Enqueue returns
	// ErrTaskIDConflict and the active task cannot be superseded (DeleteTask
	// fails), so the rollback is rejected with 409.
	env.Asynq.EnqueueErr = asynq.ErrTaskIDConflict
	env.Inspector.DeleteTaskErr = fmt.Errorf("task is active")
	resp = env.DoRequest(t, "POST", fmt.Sprintf("/api/projects/%s/applications/%s/rollback", projectID, appID), map[string]string{
		"deployment_id": deploymentID,
	}, testutil.AuthHeader(adminToken))
	assert.Equal(t, http.StatusConflict, resp.StatusCode)
	resp.Body.Close()
	env.Asynq.EnqueueErr = nil
	env.Inspector.DeleteTaskErr = nil

	// Error: deployment_id missing
	resp = env.DoRequest(t, "POST", fmt.Sprintf("/api/projects/%s/applications/%s/rollback", projectID, appID), map[string]string{}, testutil.AuthHeader(adminToken))
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
	resp.Body.Close()

	// Error: rollback to a non-successful deployment
	deployResp2 := env.DoRequest(t, "POST", fmt.Sprintf("/api/projects/%s/applications/%s/deploy", projectID, appID), nil, testutil.AuthHeader(adminToken))
	require.Equal(t, http.StatusAccepted, deployResp2.StatusCode)
	pending := testutil.ReadJSON(t, deployResp2)
	pendingID := extractID(pending["id"])

	resp = env.DoRequest(t, "POST", fmt.Sprintf("/api/projects/%s/applications/%s/rollback", projectID, appID), map[string]string{
		"deployment_id": pendingID,
	}, testutil.AuthHeader(adminToken))
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
	resp.Body.Close()

	// Conflict: the stored image has been deleted off the host. Rollback does
	// not pull, so it must refuse rather than take the running app down trying
	// to recreate from an image that is gone — and it must not enqueue anything.
	env.Runtime.ImageMissing_ = true
	env.Asynq.Tasks = nil
	resp = env.DoRequest(t, "POST", fmt.Sprintf("/api/projects/%s/applications/%s/rollback", projectID, appID), map[string]string{
		"deployment_id": deploymentID,
	}, testutil.AuthHeader(adminToken))
	assert.Equal(t, http.StatusConflict, resp.StatusCode)
	resp.Body.Close()
	assert.Empty(t, env.Asynq.Tasks, "a rollback to a missing image must not enqueue a deploy")
	env.Runtime.ImageMissing_ = false
}

func TestListApplicationLogs(t *testing.T) {
	resetDB(t)
	adminToken := env.SetupAdmin(t, "admin@test.com", "password123")

	project := env.CreateProject(t, adminToken, "Log Project", "log-project")
	projectID := extractID(project["id"])
	app := env.CreateApplication(t, adminToken, projectID, map[string]any{
		"name": "Log App", "type": "image", "build_type": "image",
		"source_image": "nginx:latest",
	})
	appID := extractID(app["id"])

	// No logs yet — should return empty array
	resp := env.DoRequest(t, "GET", fmt.Sprintf("/api/projects/%s/applications/%s/logs/history", projectID, appID), nil, testutil.AuthHeader(adminToken))
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	logs := testutil.ReadJSONArray(t, resp)
	assert.Len(t, logs, 0)

	// Insert a log entry directly via queries
	var appUUID pgtype.UUID
	require.NoError(t, appUUID.Scan(appID))
	ctx := context.Background()
	require.NoError(t, env.Queries.InsertContainerLog(ctx, generated.InsertContainerLogParams{
		SourceType: "application",
		SourceID:   appUUID,
		Level:      "info",
		Stream:     "stdout",
		Message:    "hello from test",
	}))

	// Should now return 1 entry
	resp = env.DoRequest(t, "GET", fmt.Sprintf("/api/projects/%s/applications/%s/logs/history", projectID, appID), nil, testutil.AuthHeader(adminToken))
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	logs = testutil.ReadJSONArray(t, resp)
	assert.Len(t, logs, 1)

	firstLog := logs[0].(map[string]any)
	assert.Equal(t, "stdout", firstLog["stream"])
	assert.Equal(t, "hello from test", firstLog["message"])

	// Unauthenticated is rejected
	resp = env.DoRequest(t, "GET", fmt.Sprintf("/api/projects/%s/applications/%s/logs/history", projectID, appID), nil, nil)
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
	resp.Body.Close()
}

// A deployment that is still in flight must not carry a finish time. The status
// query used to stamp finished_at on every transition, so a poller mid-build
// read finished_at == build start and concluded the deploy was done.
func TestDeployment_FinishedAtOnlySetOnTerminalStatus(t *testing.T) {
	resetDB(t)
	adminToken := env.SetupAdmin(t, "admin@test.com", "password123")

	project := env.CreateProject(t, adminToken, "Finished Project", "finished-project")
	projectID := extractID(project["id"])
	app := env.CreateApplication(t, adminToken, projectID, map[string]any{
		"name": "Finished App", "type": "image", "build_type": "image",
		"source_image": "nginx:latest",
	})
	appID := extractID(app["id"])

	ctx := context.Background()
	newDeployment := func() pgtype.UUID {
		resp := env.DoRequest(t, "POST", fmt.Sprintf("/api/projects/%s/applications/%s/deploy", projectID, appID), nil, testutil.AuthHeader(adminToken))
		require.Equal(t, http.StatusAccepted, resp.StatusCode)
		var id pgtype.UUID
		require.NoError(t, id.Scan(extractID(testutil.ReadJSON(t, resp)["id"])))
		return id
	}
	finishedAt := func(id pgtype.UUID) pgtype.Timestamptz {
		d, err := env.Queries.GetDeployment(ctx, id)
		require.NoError(t, err)
		return d.FinishedAt
	}
	setStatus := func(id pgtype.UUID, s string) {
		_, err := env.Queries.UpdateDeploymentStatus(ctx, generated.UpdateDeploymentStatusParams{ID: id, Status: s})
		require.NoError(t, err)
	}

	id := newDeployment()
	for _, s := range []string{status.DeploymentBuilding, status.DeploymentDeploying} {
		setStatus(id, s)
		if s == status.DeploymentBuilding {
			require.NoError(t, env.Queries.SetDeploymentBuildStarted(ctx, id))
		}
		assert.False(t, finishedAt(id).Valid, "finished_at must be unset while %s", s)
	}
	setStatus(id, status.DeploymentSuccess)
	assert.True(t, finishedAt(id).Valid, "finished_at must be set once the deployment succeeds")

	failed := newDeployment()
	setStatus(failed, status.DeploymentBuilding)
	assert.False(t, finishedAt(failed).Valid)
	setStatus(failed, status.DeploymentFailed)
	assert.True(t, finishedAt(failed).Valid, "finished_at must be set once the deployment fails")
}
