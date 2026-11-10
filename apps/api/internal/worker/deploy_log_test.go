package worker_test

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/hibiken/asynq"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/belune-hq/belune/internal/runtime"
	"github.com/belune-hq/belune/internal/store/generated"
	"github.com/belune-hq/belune/internal/testutil"
)

// addWorkerType gives the application a non-routable process type with one instance,
// as the API does when an operator adds a worker.
func addWorkerType(t *testing.T, app generated.Application, name string) {
	t.Helper()
	ctx := context.Background()
	pt, err := testQueries.CreateProcessType(ctx, generated.CreateProcessTypeParams{
		ApplicationID: app.ID, Name: name, Command: "run " + name, Replicas: 1, Routable: false,
	})
	require.NoError(t, err)
	_, err = testQueries.CreateInstance(ctx, generated.CreateInstanceParams{
		ApplicationID: app.ID, ProcessTypeID: pt.ID, Idx: 1, ContainerName: app.Slug + "-" + name + "-1", Status: "pending",
	})
	require.NoError(t, err)
}

// deployLogLines returns the messages of the deployment's stored log, in order.
func deployLogLines(t *testing.T, get func() (string, bool)) []string {
	t.Helper()
	blob, ok := get()
	require.True(t, ok, "the deployment stored no log at all")
	var out []string
	for _, l := range strings.Split(strings.TrimSpace(blob), "\n") {
		if l == "" {
			continue
		}
		var e struct {
			Msg string `json:"msg"`
		}
		require.NoError(t, json.Unmarshal([]byte(l), &e), l)
		out = append(out, e.Msg)
	}
	return out
}

func storedLog(t *testing.T, app generated.Application, dep generated.Deployment) []string {
	t.Helper()
	return deployLogLines(t, func() (string, bool) {
		d, err := testQueries.GetDeployment(context.Background(), dep.ID)
		require.NoError(t, err)
		return d.BuildLogs.String, d.BuildLogs.Valid && d.BuildLogs.String != ""
	})
}

// The deploy log names EVERY process type, in order, and keeps the pull output the
// container phase is appended to (build_logs is a REPLACE, so a second sink would have
// destroyed it).
func TestDeployLog_NamesEveryProcessTypeAndKeepsThePullOutput(t *testing.T) {
	t.Cleanup(func() { _ = testutil.TruncateAll(context.Background(), testPool) })
	app, dep := seedApp(t)
	addWorkerType(t, app, "queue")
	addWorkerType(t, app, "scheduler")

	h := newTestHandler(&testutil.MockContainerRuntime{}, &testutil.MockProxyManager{})
	require.NoError(t, h.HandleDeployTask(context.Background(), asynq.NewTask("deploy", mustMarshalPayload(t, app.ID, dep.ID))))

	lines := storedLog(t, app, dep)
	require.NotEmpty(t, lines)
	assert.Contains(t, lines[0], "Pulling image", "the pull output survives the container phase")
	assert.Contains(t, lines, "Image ready; deploying web ×1, queue ×1, scheduler ×1")
	for _, l := range []string{"Starting queue-1", "queue-1 is up", "Starting scheduler-1", "scheduler-1 is up"} {
		assert.Contains(t, lines, l)
	}
	assert.Contains(t, lines, "Containers up: queue-1, scheduler-1, web-1")
	for _, l := range lines {
		assert.NotContains(t, l, "starting container", "the singular wording from when an application was one container")
	}
	// Order is the deploy's order: workers come up before the routable type is touched.
	idx := func(want string) int {
		for i, l := range lines {
			if strings.HasPrefix(l, want) {
				return i
			}
		}
		return -1
	}
	assert.Less(t, idx("queue-1 is up"), idx("Rolling web-1"))
	assert.Less(t, idx("Rolling web-1"), idx("Containers up"))
}

// A failed deploy still has its output: the pull, and the line saying which process type
// failed. This is the regression the one-sink design risks, and a happy-path test cannot see it.
func TestDeployLog_AFailedDeployKeepsItsOutput(t *testing.T) {
	t.Cleanup(func() { _ = testutil.TruncateAll(context.Background(), testPool) })
	ctx := context.Background()
	app, dep := seedApp(t)
	addWorkerType(t, app, "queue")

	rt := &testutil.MockContainerRuntime{CreateFunc: func(cfg runtime.ContainerConfig) (string, error) {
		if cfg.Labels[runtime.LabelProcessType] == "queue" {
			return "", fmt.Errorf("no space left on device")
		}
		return "mock-container-id", nil
	}}
	h := newTestHandler(rt, &testutil.MockProxyManager{})
	err := h.HandleDeployTask(ctx, asynq.NewTask("deploy", mustMarshalPayload(t, app.ID, dep.ID)))
	require.Error(t, err)

	d, err := testQueries.GetDeployment(ctx, dep.ID)
	require.NoError(t, err)
	require.Equal(t, "failed", d.Status)

	lines := storedLog(t, app, dep)
	assert.Contains(t, lines[0], "Pulling image", "the build/pull output is not lost on failure")
	assert.Contains(t, lines, "Starting queue-1")
	var failed string
	for _, l := range lines {
		if strings.HasPrefix(l, "queue-1 failed") {
			failed = l
		}
	}
	assert.Contains(t, failed, "no space left on device")
	assert.NotContains(t, lines, "queue-1 is up")
}

// The log is stored on EVERY exit, including a panic: that is what the defer is for. The
// asserted line is written AFTER the last explicit persist, so only the defer can have
// stored it.
func TestDeployLog_IsStoredEvenWhenTheDeployPanics(t *testing.T) {
	t.Cleanup(func() { _ = testutil.TruncateAll(context.Background(), testPool) })
	app, dep := seedApp(t)
	addWorkerType(t, app, "queue")
	rt := &testutil.MockContainerRuntime{CreateFunc: func(runtime.ContainerConfig) (string, error) {
		panic("boom")
	}}
	h := newTestHandler(rt, &testutil.MockProxyManager{})
	assert.Panics(t, func() {
		_ = h.HandleDeployTask(context.Background(), asynq.NewTask("deploy", mustMarshalPayload(t, app.ID, dep.ID)))
	})

	lines := storedLog(t, app, dep)
	assert.Contains(t, lines[0], "Pulling image")
	assert.Contains(t, lines, "Starting queue-1", "written after the last explicit persist: only the defer stored it")
}

// auditStatusFlips records what build_logs held at the instant each deployment status
// was written. The UI switches from the live stream to the stored blob when the status
// changes, so the blob must already be complete THEN -- reading it after the deploy
// returns cannot show that, because the closing persist has run by then.
func auditStatusFlips(t *testing.T) func() map[string]string {
	t.Helper()
	ctx := context.Background()
	_, err := testPool.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS f4_status_audit (status text, logs text);
		TRUNCATE f4_status_audit;
		CREATE OR REPLACE FUNCTION f4_audit() RETURNS trigger AS $$ BEGIN
			IF NEW.status IS DISTINCT FROM OLD.status THEN
				INSERT INTO f4_status_audit VALUES (NEW.status, COALESCE(NEW.build_logs, ''));
			END IF; RETURN NEW; END $$ LANGUAGE plpgsql;
		DROP TRIGGER IF EXISTS f4_audit ON deployments;
		CREATE TRIGGER f4_audit BEFORE UPDATE ON deployments FOR EACH ROW EXECUTE FUNCTION f4_audit()`)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = testPool.Exec(ctx, `DROP TRIGGER IF EXISTS f4_audit ON deployments; DROP TABLE IF EXISTS f4_status_audit`)
	})
	return func() map[string]string {
		rows, err := testPool.Query(ctx, `SELECT status, logs FROM f4_status_audit`)
		require.NoError(t, err)
		defer rows.Close()
		out := map[string]string{}
		for rows.Next() {
			var st, logs string
			require.NoError(t, rows.Scan(&st, &logs))
			out[st] = logs
		}
		return out
	}
}

func TestDeployLog_IsCompleteBeforeTheStatusFlipsToSuccess(t *testing.T) {
	t.Cleanup(func() { _ = testutil.TruncateAll(context.Background(), testPool) })
	app, dep := seedApp(t)
	addWorkerType(t, app, "queue")
	audit := auditStatusFlips(t)

	h := newTestHandler(&testutil.MockContainerRuntime{}, &testutil.MockProxyManager{})
	require.NoError(t, h.HandleDeployTask(context.Background(), asynq.NewTask("deploy", mustMarshalPayload(t, app.ID, dep.ID))))

	atFlip := audit()["success"]
	assert.Contains(t, atFlip, "queue-1 is up")
	assert.Contains(t, atFlip, "Containers up: queue-1, web-1", "the last line is in the blob the UI is about to read")
}

func TestDeployLog_IsCompleteBeforeTheStatusFlipsToFailed(t *testing.T) {
	t.Cleanup(func() { _ = testutil.TruncateAll(context.Background(), testPool) })
	app, dep := seedApp(t)
	addWorkerType(t, app, "queue")
	audit := auditStatusFlips(t)
	rt := &testutil.MockContainerRuntime{CreateFunc: func(cfg runtime.ContainerConfig) (string, error) {
		if cfg.Labels[runtime.LabelProcessType] == "queue" {
			return "", fmt.Errorf("no space left on device")
		}
		return "mock-container-id", nil
	}}
	h := newTestHandler(rt, &testutil.MockProxyManager{})
	require.Error(t, h.HandleDeployTask(context.Background(), asynq.NewTask("deploy", mustMarshalPayload(t, app.ID, dep.ID))))

	atFlip := audit()["failed"]
	assert.Contains(t, atFlip, "Pulling image")
	assert.Contains(t, atFlip, "queue-1 failed", "the failure line is stored before the deployment reads failed")
}

// Several replicas: the summary counts instances, and labels carry the index.
func TestDeployLog_CountsReplicas(t *testing.T) {
	t.Cleanup(func() { _ = testutil.TruncateAll(context.Background(), testPool) })
	ctx := context.Background()
	app, dep := seedApp(t)
	pts, err := testQueries.ListProcessTypesByApplication(ctx, app.ID)
	require.NoError(t, err)
	_, err = testQueries.CreateInstance(ctx, generated.CreateInstanceParams{
		ApplicationID: app.ID, ProcessTypeID: pts[0].ID, Idx: 2, ContainerName: app.Slug + "-web-2", Status: "pending",
	})
	require.NoError(t, err)

	h := newTestHandler(&testutil.MockContainerRuntime{}, &testutil.MockProxyManager{})
	require.NoError(t, h.HandleDeployTask(ctx, asynq.NewTask("deploy", mustMarshalPayload(t, app.ID, dep.ID))))

	lines := storedLog(t, app, dep)
	assert.Contains(t, lines, "Image ready; deploying web ×2")
	assert.Contains(t, lines, "Containers up: web-1, web-2")
}

// A rollback has no pull or build, and used to leave an empty log.
func TestDeployLog_ARollbackSaysWhatItRedeploys(t *testing.T) {
	t.Cleanup(func() { _ = testutil.TruncateAll(context.Background(), testPool) })
	app, dep := seedApp(t)
	h := newTestHandler(&testutil.MockContainerRuntime{}, &testutil.MockProxyManager{})
	payload, err := json.Marshal(map[string]string{
		"application_id": uuidString(app.ID), "deployment_id": uuidString(dep.ID), "rollback_image_tag": "nginx@sha256:abc",
	})
	require.NoError(t, err)
	require.NoError(t, h.HandleDeployTask(context.Background(), asynq.NewTask("deploy", payload)))

	lines := storedLog(t, app, dep)
	assert.Contains(t, lines[0], "Redeploying image nginx@sha256:abc")
}
