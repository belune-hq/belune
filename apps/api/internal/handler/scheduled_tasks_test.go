package handler_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/belune-hq/belune/internal/store/generated"
	"github.com/belune-hq/belune/internal/testutil"
)

type taskFixture struct {
	token, base string
	appUUID     pgtype.UUID
}

func newTaskFixture(t *testing.T) taskFixture {
	t.Helper()
	resetDB(t)
	env.Asynq.Tasks = nil
	token := env.SetupAdmin(t, "admin@test.com", "password123")
	project := env.CreateProject(t, token, "Task Project", "task-project")
	projectID := extractID(project["id"])
	app := env.CreateApplication(t, token, projectID, map[string]any{
		"name": "Task App", "type": "image", "build_type": "image", "source_image": "nginx:latest",
	})
	appID := extractID(app["id"])
	var appUUID pgtype.UUID
	require.NoError(t, appUUID.Scan(appID))
	return taskFixture{
		token: token, appUUID: appUUID,
		base: fmt.Sprintf("/api/projects/%s/applications/%s/scheduled-tasks", projectID, appID),
	}
}

func (f taskFixture) do(t *testing.T, method, path string, body any) (int, map[string]any) {
	t.Helper()
	resp := env.DoRequest(t, method, f.base+path, body, testutil.AuthHeader(f.token))
	if resp.StatusCode == http.StatusNoContent {
		return resp.StatusCode, nil
	}
	return resp.StatusCode, testutil.ReadJSON(t, resp)
}

func (f taskFixture) setStatus(t *testing.T, status string) {
	t.Helper()
	_, err := env.Queries.UpdateApplicationStatus(context.Background(), generated.UpdateApplicationStatusParams{ID: f.appUUID, Status: status})
	require.NoError(t, err)
}

func TestScheduledTask_CreateValidatesAndDefaults(t *testing.T) {
	f := newTaskFixture(t)

	code, body := f.do(t, "POST", "", map[string]any{"name": "nightly", "command": "php artisan schedule:run", "cron": "0 3 * * *", "timezone": "Asia/Kuala_Lumpur"})
	require.Equal(t, http.StatusCreated, code)
	assert.Equal(t, "Asia/Kuala_Lumpur", body["timezone"])
	assert.EqualValues(t, 300, body["timeout_seconds"])
	assert.EqualValues(t, 20, body["keep_runs"])
	assert.Contains(t, body["next_run_at"], "+08:00", "next run must be shown in the task's own zone")

	for name, req := range map[string]map[string]any{
		"typo'd timezone":      {"name": "a", "command": "x", "cron": "0 3 * * *", "timezone": "Mars/Olympus"},
		"sub-minute schedule":  {"name": "a", "command": "x", "cron": "@every 30s"},
		"six-field cron":       {"name": "a", "command": "x", "cron": "* * * * * *"},
		"embedded CRON_TZ":     {"name": "a", "command": "x", "cron": "CRON_TZ=UTC 0 3 * * *"},
		"missing command":      {"name": "a", "cron": "0 3 * * *"},
		"timeout out of range": {"name": "a", "command": "x", "cron": "0 3 * * *", "timeout_seconds": 0},
	} {
		code, _ := f.do(t, "POST", "", req)
		assert.Equal(t, http.StatusBadRequest, code, name)
	}

	// Every-minute is the Laravel case and must work.
	code, _ = f.do(t, "POST", "", map[string]any{"name": "laravel", "command": "php artisan schedule:run", "cron": "* * * * *"})
	assert.Equal(t, http.StatusCreated, code)
}

// UpdateScheduledTask writes every column, so a partial PUT must keep what it omits.
func TestScheduledTask_PartialUpdateKeepsOmittedFields(t *testing.T) {
	f := newTaskFixture(t)
	_, created := f.do(t, "POST", "", map[string]any{
		"name": "nightly", "command": "echo hi", "cron": "0 3 * * *", "timezone": "Asia/Tokyo", "timeout_seconds": 60, "keep_runs": 5,
	})
	id := created["id"].(string)

	code, body := f.do(t, "PUT", "/"+id, map[string]any{"enabled": false})
	require.Equal(t, http.StatusOK, code)
	assert.Equal(t, false, body["enabled"])
	assert.Equal(t, "echo hi", body["command"])
	assert.Equal(t, "Asia/Tokyo", body["timezone"])
	assert.Equal(t, "0 3 * * *", body["cron"])
	assert.EqualValues(t, 60, body["timeout_seconds"])
	assert.EqualValues(t, 5, body["keep_runs"])
}

func TestScheduledTask_CommandIsKeptOutOfTheAuditLog(t *testing.T) {
	f := newTaskFixture(t)
	base := auditedServerURL(t)
	raw, err := json.Marshal(map[string]any{"name": "secretive", "command": "curl -H 'Authorization: s3cr3t-token' https://x", "cron": "0 3 * * *"})
	require.NoError(t, err)
	// base is a second server over the same DB with a real audit service; f.base
	// is "/api/projects/.../scheduled-tasks".
	req, err := http.NewRequest("POST", base+f.base, bytes.NewReader(raw))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	for k, v := range testutil.AuthHeader(f.token) {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	resp.Body.Close()
	require.Equal(t, http.StatusCreated, resp.StatusCode)

	var details []byte
	require.Eventually(t, func() bool {
		return env.Pool.QueryRow(context.Background(),
			`SELECT details FROM audit_logs WHERE action = 'create_scheduled_task'`).Scan(&details) == nil
	}, 2*time.Second, 10*time.Millisecond, "creating a task must write an audit row")
	assert.NotContains(t, string(details), "s3cr3t-token")
	assert.Contains(t, string(details), "secretive")
}

func TestScheduledTask_ManualRunRules(t *testing.T) {
	f := newTaskFixture(t)
	_, created := f.do(t, "POST", "", map[string]any{"name": "t", "command": "true", "cron": "0 3 * * *"})
	id := created["id"].(string)

	f.setStatus(t, "stopped")
	code, body := f.do(t, "POST", "/"+id+"/run", nil)
	assert.Equal(t, http.StatusConflict, code, "a stopped app must not run tasks")
	assert.Contains(t, body["error"], "not running")
	assert.Empty(t, env.Asynq.Tasks)

	f.setStatus(t, "running")
	code, _ = f.do(t, "POST", "/"+id+"/run", nil)
	require.Equal(t, http.StatusAccepted, code)
	require.Len(t, env.Asynq.Tasks, 1)
	assert.Equal(t, "scheduled_task:run", env.Asynq.Tasks[0].TypeName)

	// The first run is still 'running' (no worker in this test), so a second is refused.
	code, body = f.do(t, "POST", "/"+id+"/run", nil)
	assert.Equal(t, http.StatusConflict, code)
	assert.Contains(t, body["error"], "in progress")

	// A manual run must not move the schedule baseline.
	task, err := env.Queries.GetScheduledTask(context.Background(), mustUUID(t, id))
	require.NoError(t, err)
	assert.False(t, task.LastRunAt.Valid)
}

func TestScheduledTask_EnqueueFailureLeavesNoRun(t *testing.T) {
	f := newTaskFixture(t)
	_, created := f.do(t, "POST", "", map[string]any{"name": "t", "command": "true", "cron": "0 3 * * *"})
	id := created["id"].(string)
	f.setStatus(t, "running")

	env.Asynq.EnqueueErr = fmt.Errorf("redis down")
	env.Asynq.EnqueueOnce = true
	code, _ := f.do(t, "POST", "/"+id+"/run", nil)
	assert.Equal(t, http.StatusInternalServerError, code)

	// The orphan row would otherwise count as in flight and wedge the task.
	code, _ = f.do(t, "POST", "/"+id+"/run", nil)
	assert.Equal(t, http.StatusAccepted, code)
}

func TestNeedsAttention_CountsTasksNotRuns(t *testing.T) {
	f := newTaskFixture(t)
	ctx := context.Background()
	_, created := f.do(t, "POST", "", map[string]any{"name": "t", "command": "true", "cron": "0 3 * * *"})
	taskID := mustUUID(t, created["id"].(string))

	run := func(status string) {
		r, err := env.Queries.InsertScheduledTaskRun(ctx, generated.InsertScheduledTaskRunParams{TaskID: taskID, Trigger: "schedule"})
		require.NoError(t, err)
		require.NoError(t, env.Queries.FinishScheduledTaskRun(ctx, generated.FinishScheduledTaskRunParams{ID: r.ID, Status: status}))
	}
	failed := func() any {
		resp := env.DoRequest(t, "GET", "/api/stats", nil, testutil.AuthHeader(f.token))
		require.Equal(t, http.StatusOK, resp.StatusCode)
		return testutil.ReadJSON(t, resp)["needs_attention"].(map[string]any)["failed_tasks"]
	}

	run("failed")
	run("failed")
	run("timed_out")
	assert.EqualValues(t, 1, failed(), "three failing runs of one task is one thing needing attention")

	_, err := env.Queries.InsertSkippedScheduledTaskRun(ctx, generated.InsertSkippedScheduledTaskRunParams{
		TaskID: taskID, Trigger: "schedule", SkipReason: pgtype.Text{String: "application is not running", Valid: true},
	})
	require.NoError(t, err)
	assert.EqualValues(t, 1, failed(), "a later skip must not mask an unfixed failure")

	run("succeeded")
	assert.EqualValues(t, 0, failed(), "a later success resolves it")
}

func mustUUID(t *testing.T, s string) pgtype.UUID {
	t.Helper()
	var u pgtype.UUID
	require.NoError(t, u.Scan(s))
	return u
}
