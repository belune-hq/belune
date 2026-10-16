package worker_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"testing"
	"time"

	"github.com/hibiken/asynq"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/weiliang79/belune/internal/config"
	"github.com/weiliang79/belune/internal/service"
	"github.com/weiliang79/belune/internal/store/generated"
	"github.com/weiliang79/belune/internal/testutil"
	"github.com/weiliang79/belune/internal/worker"
)

func seedTask(t *testing.T, appID pgtype.UUID, cron string) generated.ApplicationScheduledTask {
	t.Helper()
	task, err := testQueries.CreateScheduledTask(context.Background(), generated.CreateScheduledTaskParams{
		ApplicationID: appID, Name: "t-" + randomSuffix(t), Command: "echo hi", Cron: cron,
		Timezone: "UTC", Enabled: true, TimeoutSeconds: 30, KeepRuns: 3,
	})
	require.NoError(t, err)
	// Pretend it last ran long ago so the sweep finds it due.
	require.NoError(t, testQueries.SetScheduledTaskLastRun(context.Background(), generated.SetScheduledTaskLastRunParams{
		ID: task.ID, LastRunAt: pgtype.Timestamptz{Time: time.Now().Add(-48 * time.Hour), Valid: true},
	}))
	return task
}

func setAppStatus(t *testing.T, id pgtype.UUID, status string) {
	t.Helper()
	_, err := testQueries.UpdateApplicationStatus(context.Background(), generated.UpdateApplicationStatusParams{ID: id, Status: status})
	require.NoError(t, err)
}

func taskRuns(t *testing.T, id pgtype.UUID) []generated.ApplicationScheduledTaskRun {
	t.Helper()
	runs, err := testQueries.ListScheduledTaskRuns(context.Background(), generated.ListScheduledTaskRunsParams{TaskID: id, Limit: 100})
	require.NoError(t, err)
	return runs
}

func sweepHandler(enq *testutil.MockTaskEnqueuer, max int) *worker.TaskHandler {
	h := newTestHandler(&testutil.MockContainerRuntime{}, nil)
	h.Enqueuer = enq
	h.Config = &config.Config{ScheduledTaskMaxConcurrent: max}
	return h
}

func TestScheduledTaskSweep_DispatchesStampsAndSkips(t *testing.T) {
	ctx := context.Background()
	app, _ := seedApp(t)
	setAppStatus(t, app.ID, "running")
	task := seedTask(t, app.ID, "0 * * * *")

	enq := &testutil.MockTaskEnqueuer{}
	h := sweepHandler(enq, 2)
	h.HandleBackupScheduleSweep(ctx)

	runs := taskRuns(t, task.ID)
	require.Len(t, runs, 1)
	assert.Equal(t, "running", runs[0].Status)
	assert.Equal(t, "schedule", runs[0].Trigger)
	require.Len(t, enq.Tasks, 1)
	assert.Equal(t, worker.TypeScheduledTask, enq.Tasks[0].TypeName)

	got, _ := testQueries.GetScheduledTask(ctx, task.ID)
	assert.True(t, got.LastRunAt.Time.After(time.Now().Add(-time.Minute)), "a dispatched run stamps the baseline")

	// Same minute again: not due, and nothing new is recorded.
	h.HandleBackupScheduleSweep(ctx)
	assert.Len(t, taskRuns(t, task.ID), 1)

	// A stopped app records a visible skip (and consumes the occurrence).
	app2, _ := seedApp(t)
	setAppStatus(t, app2.ID, "stopped")
	stopped := seedTask(t, app2.ID, "0 * * * *")
	h.HandleBackupScheduleSweep(ctx)
	sr := taskRuns(t, stopped.ID)
	require.Len(t, sr, 1)
	assert.Equal(t, "skipped", sr[0].Status)
	assert.Equal(t, "application is not running", sr[0].SkipReason.String)
}

func TestScheduledTaskSweep_FailedEnqueueLeavesBaselineAlone(t *testing.T) {
	ctx := context.Background()
	app, _ := seedApp(t)
	setAppStatus(t, app.ID, "running")
	task := seedTask(t, app.ID, "0 * * * *")
	before, _ := testQueries.GetScheduledTask(ctx, task.ID)

	enq := &testutil.MockTaskEnqueuer{EnqueueErr: fmt.Errorf("redis down"), EnqueueOnce: true}
	h := sweepHandler(enq, 2)
	h.HandleBackupScheduleSweep(ctx)

	after, _ := testQueries.GetScheduledTask(ctx, task.ID)
	assert.True(t, before.LastRunAt.Time.Equal(after.LastRunAt.Time), "baseline must not move on a failed enqueue")
	assert.Empty(t, taskRuns(t, task.ID), "no orphan 'running' row to wedge the task")

	h.HandleBackupScheduleSweep(ctx) // next sweep retries
	assert.Len(t, taskRuns(t, task.ID), 1)
}

// The cap is enforced at ENQUEUE time: a run over the cap must never reach the
// queue, because a queued run still holds a worker slot.
func TestScheduledTaskSweep_GlobalCapIsEnforcedBeforeEnqueue(t *testing.T) {
	ctx := context.Background()
	// Other tests leave rows behind; make the count deterministic.
	_, err := testPool.Exec(ctx, `DELETE FROM application_scheduled_task_runs`)
	require.NoError(t, err)

	var tasks []generated.ApplicationScheduledTask
	for i := 0; i < 4; i++ {
		app, _ := seedApp(t)
		setAppStatus(t, app.ID, "running")
		tasks = append(tasks, seedTask(t, app.ID, "0 * * * *"))
	}
	enq := &testutil.MockTaskEnqueuer{}
	sweepHandler(enq, 2).HandleBackupScheduleSweep(ctx)

	assert.Len(t, enq.Tasks, 2, "only the cap's worth may be enqueued")
	running, skipped := 0, 0
	for _, task := range tasks {
		for _, r := range taskRuns(t, task.ID) {
			switch r.Status {
			case "running":
				running++
			case "skipped":
				skipped++
				assert.Contains(t, r.SkipReason.String, "limit 2")
			}
		}
	}
	assert.Equal(t, 2, running)
	assert.Equal(t, 2, skipped)
}

func TestScheduledTaskSweep_ReapsAbandonedRuns(t *testing.T) {
	ctx := context.Background()
	app, _ := seedApp(t)
	setAppStatus(t, app.ID, "running")
	task := seedTask(t, app.ID, "0 3 1 1 *") // not due
	run, err := testQueries.InsertScheduledTaskRun(ctx, generated.InsertScheduledTaskRunParams{TaskID: task.ID, Trigger: "schedule"})
	require.NoError(t, err)
	_, err = testPool.Exec(ctx, `UPDATE application_scheduled_task_runs SET started_at = NOW() - interval '1 hour' WHERE id = $1`, run.ID)
	require.NoError(t, err)

	sweepHandler(&testutil.MockTaskEnqueuer{}, 2).HandleBackupScheduleSweep(ctx)

	got, _ := testQueries.GetScheduledTaskRun(ctx, run.ID)
	assert.Equal(t, "failed", got.Status, "a run orphaned by a worker restart must not pin the cap forever")
}

// run dispatches via the real dispatcher and executes the queued payload.
func runTask(t *testing.T, h *worker.TaskHandler, task generated.ApplicationScheduledTask) generated.ApplicationScheduledTaskRun {
	t.Helper()
	enq := &testutil.MockTaskEnqueuer{}
	disp := service.NewScheduledTaskDispatcher(testQueries, enq, 5)
	run, skipped, err := disp.Dispatch(context.Background(), task, service.RunTriggerManual)
	require.NoError(t, err)
	require.False(t, skipped)
	require.Len(t, enq.Tasks, 1)
	var p service.ScheduledTaskRunPayload
	require.NoError(t, json.Unmarshal(enq.Tasks[0].Payload, &p))
	require.Equal(t, uuidString(run.ID), p.RunID)
	require.NoError(t, h.HandleScheduledTaskRun(context.Background(), asynq.NewTask(enq.Tasks[0].TypeName, enq.Tasks[0].Payload)))
	got, err := testQueries.GetScheduledTaskRun(context.Background(), run.ID)
	require.NoError(t, err)
	return got
}

func TestScheduledTaskRun_MapsExitCodes(t *testing.T) {
	cases := []struct {
		name       string
		exit       int
		execErr    error
		wantStatus string
	}{
		{"success", 0, nil, "succeeded"},
		{"failure", 3, nil, "failed"},
		{"timeout(1) exits 124", 124, nil, "timed_out"},
		{"exec error", -1, fmt.Errorf("no such container"), "failed"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			app, _ := seedApp(t)
			setAppStatus(t, app.ID, "running")
			task := seedTask(t, app.ID, "0 3 1 1 *")

			var gotCmd []string
			cleanups := 0
			rt := &testutil.MockContainerRuntime{ExecFunc: func(_ context.Context, _ string, cmd []string, _ io.Reader, stdout, _ io.Writer) (int, error) {
				if cmd[0] != "env" {
					cleanups++ // the post-timeout sweep for orphaned processes
					return 0, nil
				}
				gotCmd = cmd
				_, _ = stdout.Write([]byte("some output"))
				return c.exit, c.execErr
			}}
			h := newTestHandler(rt, nil)
			got := runTask(t, h, task)

			assert.Equal(t, c.wantStatus, got.Status)
			assert.True(t, got.FinishedAt.Valid)
			assert.Contains(t, got.Output, "some output")
			// The timeout lives INSIDE the container; the whole command is one argv to sh -c.
			wantCleanups := 0
			if c.wantStatus == "timed_out" {
				wantCleanups = 1
			}
			assert.Equal(t, wantCleanups, cleanups, "orphans are swept after a timeout and only then")
			assert.Equal(t, []string{"env", "BELUNE_TASK_RUN=" + uuidString(got.ID), "timeout", "30", "sh", "-c", "echo hi"}, gotCmd)
		})
	}
}

func TestScheduledTaskRun_PrunesHistoryPerPool(t *testing.T) {
	ctx := context.Background()
	app, _ := seedApp(t)
	setAppStatus(t, app.ID, "running")
	task := seedTask(t, app.ID, "0 3 1 1 *") // keep_runs = 3

	for i := 0; i < 5; i++ {
		_, err := testQueries.InsertSkippedScheduledTaskRun(ctx, generated.InsertSkippedScheduledTaskRunParams{
			TaskID: task.ID, Trigger: "schedule", SkipReason: pgtype.Text{String: "application is not running", Valid: true},
		})
		require.NoError(t, err)
	}
	h := newTestHandler(&testutil.MockContainerRuntime{}, nil)
	for i := 0; i < 5; i++ {
		runTask(t, h, task)
	}

	var ran, skipped int
	for _, r := range taskRuns(t, task.ID) {
		if r.Status == "skipped" {
			skipped++
		} else {
			ran++
		}
	}
	assert.Equal(t, 3, ran, "real runs are capped at keep_runs")
	assert.Equal(t, 3, skipped, "skips are pruned on their own budget so they cannot evict real runs")
}
