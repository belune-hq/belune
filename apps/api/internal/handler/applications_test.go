package handler_test

import (
	"fmt"
	"net/http"
	"testing"

	"github.com/hibiken/asynq"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/belune-hq/belune/internal/testutil"
)

func TestCreateApplication(t *testing.T) {
	resetDB(t)
	token := env.SetupAdmin(t, "admin@test.com", "password123")
	project := env.CreateProject(t, token, "Test Project", "test-project")
	projectID := extractID(project["id"])

	// Git type application
	app := env.CreateApplication(t, token, projectID, map[string]any{
		"name":        "My App",
		"type":        "git",
		"build_type":  "dockerfile",
		"source_repo": "https://github.com/test/repo",
	})
	assert.Equal(t, "My App", app["name"])
	assert.Equal(t, "git", app["type"])
	assert.Contains(t, app["slug"], "test-project")

	// Image type application
	app2 := env.CreateApplication(t, token, projectID, map[string]any{
		"name":         "Image App",
		"type":         "image",
		"build_type":   "image",
		"source_image": "nginx:latest",
	})
	assert.Equal(t, "Image App", app2["name"])
	assert.Equal(t, "image", app2["type"])
}

func TestListApplications(t *testing.T) {
	resetDB(t)
	token := env.SetupAdmin(t, "admin@test.com", "password123")
	project := env.CreateProject(t, token, "Test Project", "test-project")
	projectID := extractID(project["id"])

	env.CreateApplication(t, token, projectID, map[string]any{
		"name": "App 1", "type": "git", "build_type": "dockerfile", "source_repo": "https://github.com/test/repo",
	})
	env.CreateApplication(t, token, projectID, map[string]any{
		"name": "App 2", "type": "git", "build_type": "dockerfile", "source_repo": "https://github.com/test/repo",
	})

	resp := env.DoRequest(t, "GET", fmt.Sprintf("/api/projects/%s/applications", projectID), nil, testutil.AuthHeader(token))
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	apps := testutil.ReadJSONArray(t, resp)
	assert.Len(t, apps, 2)
}

func TestUpdateApplication(t *testing.T) {
	resetDB(t)
	token := env.SetupAdmin(t, "admin@test.com", "password123")
	project := env.CreateProject(t, token, "Test Project", "test-project")
	projectID := extractID(project["id"])

	app := env.CreateApplication(t, token, projectID, map[string]any{
		"name": "Original", "type": "git", "build_type": "dockerfile", "source_repo": "https://github.com/test/repo",
	})
	appID := extractID(app["id"])

	resp := env.DoRequest(t, "PUT", fmt.Sprintf("/api/projects/%s/applications/%s", projectID, appID), map[string]any{
		"name":        "Updated",
		"source_repo": "https://github.com/new/repo",
	}, testutil.AuthHeader(token))
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	result := testutil.ReadJSON(t, resp)
	assert.Equal(t, "Updated", result["name"])
}

func TestDeleteApplication(t *testing.T) {
	resetDB(t)
	token := env.SetupAdmin(t, "admin@test.com", "password123")
	project := env.CreateProject(t, token, "Test Project", "test-project")
	projectID := extractID(project["id"])

	app := env.CreateApplication(t, token, projectID, map[string]any{
		"name": "To Delete", "type": "git", "build_type": "dockerfile", "source_repo": "https://github.com/test/repo",
	})
	appID := extractID(app["id"])

	resp := env.DoRequest(t, "DELETE", fmt.Sprintf("/api/projects/%s/applications/%s", projectID, appID), nil, testutil.AuthHeader(token))
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	resp.Body.Close()

	// Verify runtime was called
	require.Greater(t, len(env.Runtime.StopCalls), 0)
	require.Greater(t, len(env.Runtime.RemoveCalls), 0)
}

func TestDeployApplication(t *testing.T) {
	resetDB(t)
	token := env.SetupAdmin(t, "admin@test.com", "password123")
	project := env.CreateProject(t, token, "Test Project", "test-project")
	projectID := extractID(project["id"])

	app := env.CreateApplication(t, token, projectID, map[string]any{
		"name": "Deploy Me", "type": "git", "build_type": "dockerfile",
		"source_repo": "https://github.com/test/repo",
	})
	appID := extractID(app["id"])

	resp := env.DoRequest(t, "POST", fmt.Sprintf("/api/projects/%s/applications/%s/deploy", projectID, appID), nil, testutil.AuthHeader(token))
	assert.Equal(t, http.StatusAccepted, resp.StatusCode)
	result := testutil.ReadJSON(t, resp)
	assert.Equal(t, "pending", result["status"])

	// Verify asynq task was enqueued
	require.Len(t, env.Asynq.Tasks, 1)
	assert.Equal(t, "deploy", env.Asynq.Tasks[0].TypeName)
}

func TestStopStartRestartApplication(t *testing.T) {
	resetDB(t)
	token := env.SetupAdmin(t, "admin@test.com", "password123")
	project := env.CreateProject(t, token, "Test Project", "test-project")
	projectID := extractID(project["id"])

	app := env.CreateApplication(t, token, projectID, map[string]any{
		"name": "Lifecycle App", "type": "git", "build_type": "dockerfile", "source_repo": "https://github.com/test/repo",
	})
	appID := extractID(app["id"])

	// Stop
	resp := env.DoRequest(t, "POST", fmt.Sprintf("/api/projects/%s/applications/%s/stop", projectID, appID), nil, testutil.AuthHeader(token))
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	result := testutil.ReadJSON(t, resp)
	assert.Equal(t, "stopped", result["status"])
	require.Greater(t, len(env.Runtime.StopCalls), 0)

	// Start
	resp = env.DoRequest(t, "POST", fmt.Sprintf("/api/projects/%s/applications/%s/start", projectID, appID), nil, testutil.AuthHeader(token))
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	result = testutil.ReadJSON(t, resp)
	assert.Equal(t, "running", result["status"])
	require.Greater(t, len(env.Runtime.StartCalls), 0)

	// Restart
	env.Runtime.StopCalls = nil
	env.Runtime.StartCalls = nil
	resp = env.DoRequest(t, "POST", fmt.Sprintf("/api/projects/%s/applications/%s/restart", projectID, appID), nil, testutil.AuthHeader(token))
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	result = testutil.ReadJSON(t, resp)
	assert.Equal(t, "running", result["status"])
	require.Greater(t, len(env.Runtime.StopCalls), 0)
	require.Greater(t, len(env.Runtime.StartCalls), 0)
}

func TestBuildApplication(t *testing.T) {
	resetDB(t)
	token := env.SetupAdmin(t, "admin@test.com", "password123")
	project := env.CreateProject(t, token, "Test Project", "test-project")
	projectID := extractID(project["id"])

	app := env.CreateApplication(t, token, projectID, map[string]any{
		"name": "Build Me", "type": "git", "build_type": "dockerfile",
		"source_repo": "https://github.com/test/repo",
	})
	appID := extractID(app["id"])

	resp := env.DoRequest(t, "POST", fmt.Sprintf("/api/projects/%s/applications/%s/build", projectID, appID), nil, testutil.AuthHeader(token))
	assert.Equal(t, http.StatusAccepted, resp.StatusCode)
	resp.Body.Close()

	// Verify asynq task was enqueued with "build" type
	require.Len(t, env.Asynq.Tasks, 1)
	assert.Equal(t, "build", env.Asynq.Tasks[0].TypeName)
}

func TestCreateApplication_WebhookSecretAutoGenerated(t *testing.T) {
	resetDB(t)
	token := env.SetupAdmin(t, "admin@test.com", "password123")
	project := env.CreateProject(t, token, "Test Project", "test-project")
	projectID := extractID(project["id"])

	app := env.CreateApplication(t, token, projectID, map[string]any{
		"name": "Webhook App", "type": "git", "build_type": "dockerfile",
		"source_repo": "https://github.com/test/repo",
	})

	// The response reports presence only; the value comes from reveal.
	assert.Equal(t, true, app["has_webhook_secret"])
	assert.NotContains(t, app, "webhook_secret")

	resp := env.DoRequest(t, "GET",
		fmt.Sprintf("/api/projects/%s/applications/%s/webhook/reveal", projectID, extractID(app["id"])),
		nil, testutil.AuthHeader(token))
	require.Equal(t, http.StatusOK, resp.StatusCode)
	secret, ok := testutil.ReadJSON(t, resp)["webhook_secret"].(string)
	require.True(t, ok, "webhook_secret should be a string")
	assert.Len(t, secret, 64, "webhook_secret should be a 32-byte hex string")
}

func TestCreateApplication_ResourceLimits(t *testing.T) {
	resetDB(t)
	token := env.SetupAdmin(t, "admin@test.com", "password123")
	project := env.CreateProject(t, token, "Test Project", "test-project")
	projectID := extractID(project["id"])

	app := env.CreateApplication(t, token, projectID, map[string]any{
		"name": "Limited App", "type": "git", "build_type": "dockerfile", "source_repo": "https://github.com/test/repo",
		"cpu_limit":    0.5,
		"memory_limit": 536870912, // 512 MB
	})
	assert.Equal(t, 0.5, app["cpu_limit"])
	assert.Equal(t, float64(536870912), app["memory_limit"])

	// Limits are updated through their own endpoint now.
	appID := extractID(app["id"])
	resp := env.DoRequest(t, "PUT", fmt.Sprintf("/api/projects/%s/applications/%s/resources", projectID, appID), map[string]any{
		"cpu_limit":    1.0,
		"memory_limit": 1073741824, // 1 GB
	}, testutil.AuthHeader(token))
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	result := testutil.ReadJSON(t, resp)
	assert.Equal(t, 1.0, result["cpu_limit"])
	assert.Equal(t, float64(1073741824), result["memory_limit"])

	// A general settings save preserves the limits rather than resetting them.
	resp = env.DoRequest(t, "PUT", fmt.Sprintf("/api/projects/%s/applications/%s", projectID, appID), map[string]any{
		"source_repo": "https://github.com/test/repo",
	}, testutil.AuthHeader(token))
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	result = testutil.ReadJSON(t, resp)
	assert.Equal(t, 1.0, result["cpu_limit"], "settings save must not reset the CPU limit")
	assert.Equal(t, float64(1073741824), result["memory_limit"])
}

func TestCreateApplication_HealthCheckPath(t *testing.T) {
	resetDB(t)
	token := env.SetupAdmin(t, "admin@test.com", "password123")
	project := env.CreateProject(t, token, "Test Project", "test-project")
	projectID := extractID(project["id"])

	app := env.CreateApplication(t, token, projectID, map[string]any{
		"name": "Healthy App", "type": "git", "build_type": "dockerfile", "source_repo": "https://github.com/test/repo",
		"health_check_path": "/healthz",
	})
	assert.Equal(t, "/healthz", app["health_check_path"])

	// The health path is now managed by its own endpoint, not the general
	// update — a settings save must never clear it.
	appID := extractID(app["id"])
	resp := env.DoRequest(t, "PUT", fmt.Sprintf("/api/projects/%s/applications/%s/health-check", projectID, appID), map[string]any{
		"type": "http",
		"path": "/ready",
	}, testutil.AuthHeader(token))
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	result := testutil.ReadJSON(t, resp)
	assert.Equal(t, "/ready", result["health_check_path"])

	// A general settings save preserves it rather than clearing it.
	resp = env.DoRequest(t, "PUT", fmt.Sprintf("/api/projects/%s/applications/%s", projectID, appID), map[string]any{
		"source_repo": "https://github.com/test/repo",
	}, testutil.AuthHeader(token))
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	result = testutil.ReadJSON(t, resp)
	assert.Equal(t, "/ready", result["health_check_path"])
}

func TestDeployApplication_LockReturns409(t *testing.T) {
	resetDB(t)
	token := env.SetupAdmin(t, "admin@test.com", "password123")
	project := env.CreateProject(t, token, "Test Project", "test-project")
	projectID := extractID(project["id"])

	app := env.CreateApplication(t, token, projectID, map[string]any{
		"name": "Lock App", "type": "git", "build_type": "dockerfile",
		"source_repo": "https://github.com/test/repo",
	})
	appID := extractID(app["id"])

	// First deploy succeeds
	resp := env.DoRequest(t, "POST", fmt.Sprintf("/api/projects/%s/applications/%s/deploy", projectID, appID), nil, testutil.AuthHeader(token))
	assert.Equal(t, http.StatusAccepted, resp.StatusCode)
	resp.Body.Close()

	// Simulate a deploy genuinely in progress: enqueue returns ErrTaskIDConflict
	// and the active task cannot be superseded (DeleteTask fails).
	env.Asynq.EnqueueErr = asynq.ErrTaskIDConflict
	env.Inspector.DeleteTaskErr = fmt.Errorf("task is active")

	// Second deploy while one is in progress → 409
	resp = env.DoRequest(t, "POST", fmt.Sprintf("/api/projects/%s/applications/%s/deploy", projectID, appID), nil, testutil.AuthHeader(token))
	assert.Equal(t, http.StatusConflict, resp.StatusCode)
	resp.Body.Close()

	// A stale (non-active) locked task instead should be superseded: DeleteTask
	// succeeds and the re-enqueue goes through, so the deploy is accepted.
	env.Asynq.EnqueueErr = asynq.ErrTaskIDConflict
	env.Asynq.EnqueueOnce = true // conflict clears after the first (pre-delete) enqueue
	env.Inspector.DeleteTaskErr = nil
	env.Inspector.DeleteTaskCalls = nil
	resp = env.DoRequest(t, "POST", fmt.Sprintf("/api/projects/%s/applications/%s/deploy", projectID, appID), nil, testutil.AuthHeader(token))
	assert.Equal(t, http.StatusAccepted, resp.StatusCode)
	resp.Body.Close()
	assert.Equal(t, []string{"critical/deploy:" + appID}, env.Inspector.DeleteTaskCalls)

	// Clear the error for subsequent tests
	env.Asynq.EnqueueErr = nil
	env.Asynq.EnqueueOnce = false
}
