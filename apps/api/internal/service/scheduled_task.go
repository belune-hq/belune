package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/hibiken/asynq"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/weiliang79/belune/internal/store/generated"
)

// ScheduledTaskType is the asynq task type that executes one scheduled-task run.
// worker.TypeScheduledTask aliases it so the queue's names stay listed in
// worker/tasks.go while the dispatcher (which the worker imports, not the other
// way round) can enqueue it.
const ScheduledTaskType = "scheduled_task:run"

const (
	RunTriggerSchedule = "schedule"
	RunTriggerManual   = "manual"
)

// ErrScheduledTaskNotRunnable means a MANUAL run was refused (app stopped, a run
// already in flight, or the global cap). Scheduled dispatches record a 'skipped'
// row instead — an invisible skip is indistinguishable from a cron that never
// fired — but a person who clicked "Run now" is owed an answer, not a history row.
type ErrScheduledTaskNotRunnable struct{ Reason string }

func (e *ErrScheduledTaskNotRunnable) Error() string { return e.Reason }

// ScheduledTaskRunPayload is the asynq payload: the run row already exists, so a
// task waiting in the queue is already counted against the concurrency cap.
type ScheduledTaskRunPayload struct {
	RunID string `json:"run_id"`
}

// ScheduledTaskDispatcher owns the one way a scheduled-task run reaches the
// queue, for the per-minute sweep and the manual "Run now" endpoint alike.
type ScheduledTaskDispatcher struct {
	queries       *generated.Queries
	enq           TaskEnqueuer
	maxConcurrent int
}

func NewScheduledTaskDispatcher(q *generated.Queries, enq TaskEnqueuer, maxConcurrent int) *ScheduledTaskDispatcher {
	if maxConcurrent < 1 {
		maxConcurrent = 2
	}
	return &ScheduledTaskDispatcher{queries: q, enq: enq, maxConcurrent: maxConcurrent}
}

// Dispatch admits one run of task. The cap is enforced HERE, before enqueueing:
// a run that enqueues and then waits is still holding one of the worker's few
// slots. The run row is inserted as 'running' first so queued-but-not-started
// runs count toward the cap and the in-flight check (the sweep may dispatch
// several tasks inside one pass, before any worker has picked one up).
//
// The returned bool is true when the run was recorded as skipped (scheduled
// trigger only). On an enqueue failure the row is removed and an error returned,
// so the caller can leave the schedule baseline untouched and retry next sweep.
func (d *ScheduledTaskDispatcher) Dispatch(ctx context.Context, task generated.ApplicationScheduledTask, trigger string) (generated.ApplicationScheduledTaskRun, bool, error) {
	var zero generated.ApplicationScheduledTaskRun

	reason, err := d.refusal(ctx, task)
	if err != nil {
		return zero, false, err
	}
	if reason != "" {
		if trigger == RunTriggerManual {
			return zero, false, &ErrScheduledTaskNotRunnable{Reason: reason}
		}
		run, err := d.queries.InsertSkippedScheduledTaskRun(ctx, generated.InsertSkippedScheduledTaskRunParams{
			TaskID: task.ID, Trigger: trigger, SkipReason: pgtype.Text{String: reason, Valid: true},
		})
		if err != nil {
			return zero, false, fmt.Errorf("recording skipped run: %w", err)
		}
		return run, true, nil
	}

	run, err := d.queries.InsertScheduledTaskRun(ctx, generated.InsertScheduledTaskRunParams{TaskID: task.ID, Trigger: trigger})
	if err != nil {
		return zero, false, fmt.Errorf("inserting run: %w", err)
	}
	payload, err := json.Marshal(ScheduledTaskRunPayload{RunID: uuidToString(run.ID)})
	if err != nil {
		_ = d.queries.DeleteScheduledTaskRun(ctx, run.ID)
		return zero, false, fmt.Errorf("marshal payload: %w", err)
	}
	// MaxRetry(0): a retry would re-exec a command that may not be idempotent.
	if _, err := d.enq.Enqueue(asynq.NewTask(ScheduledTaskType, payload), asynq.Queue("low"), asynq.MaxRetry(0)); err != nil {
		_ = d.queries.DeleteScheduledTaskRun(ctx, run.ID)
		return zero, false, fmt.Errorf("enqueue: %w", err)
	}
	return run, false, nil
}

// refusal returns why task must not run right now, or "" when it may.
func (d *ScheduledTaskDispatcher) refusal(ctx context.Context, task generated.ApplicationScheduledTask) (string, error) {
	app, err := d.queries.GetApplication(ctx, task.ApplicationID)
	if err != nil {
		return "", fmt.Errorf("loading application: %w", err)
	}
	// A stopped app is either deliberately stopped or broken; neither wants its
	// cron firing, so skipping is correct rather than a limitation.
	if app.Status != "running" {
		return "application is not running", nil
	}
	inFlight, err := d.queries.ScheduledTaskRunInFlight(ctx, task.ID)
	if err != nil {
		return "", fmt.Errorf("checking in-flight run: %w", err)
	}
	if inFlight {
		return "previous run is still in progress", nil
	}
	running, err := d.queries.CountRunningScheduledTaskRuns(ctx)
	if err != nil {
		return "", fmt.Errorf("counting running runs: %w", err)
	}
	if int(running) >= d.maxConcurrent {
		return fmt.Sprintf("too many scheduled tasks running at once (limit %d)", d.maxConcurrent), nil
	}
	return "", nil
}

// IsNotRunnable reports whether err is a manual-run refusal.
func IsNotRunnable(err error) (*ErrScheduledTaskNotRunnable, bool) {
	var e *ErrScheduledTaskNotRunnable
	if errors.As(err, &e) {
		return e, true
	}
	return nil, false
}
