package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/hibiken/asynq"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/weiliang79/belune/internal/naming"
	"github.com/weiliang79/belune/internal/notify"
	"github.com/weiliang79/belune/internal/pkg/cronspec"
	"github.com/weiliang79/belune/internal/runtime"
	"github.com/weiliang79/belune/internal/service"
	"github.com/weiliang79/belune/internal/store/generated"
)

const (
	// scheduledTaskOutputCap bounds what one run stores. head + tail is kept
	// because the start shows what the job was doing and the end shows why it died.
	scheduledTaskOutputCap = 64 * 1024

	// timeoutExitCode is what GNU timeout(1) (Debian) exits with when it had to
	// kill the command.
	timeoutExitCode = 124
	// sigtermExitCode is what busybox timeout (Alpine) exits with instead: it
	// reports the signal that killed the command (128+SIGTERM). Found by running
	// both images — 124 alone would have called every Alpine timeout a failure.
	sigtermExitCode = 143
)

// scheduledRunEnv is the variable that marks every process of one run.
const scheduledRunEnv = "BELUNE_TASK_RUN"

// killTaggedProcesses SIGKILLs whatever a timed-out run left behind, found by
// its environment tag. The run id is a validated UUID, so it is safe to embed.
// Best-effort and silent: the run is already recorded as timed out. The dead
// processes may linger as zombies until the container's PID 1 reaps them, which
// many app images (node, php-fpm) never do; that is a PID-table entry, not a
// running process, and a restart clears it.
func (h *TaskHandler) killTaggedProcesses(ctx context.Context, rt runtime.ContainerRuntime, containerName, runID string) {
	script := fmt.Sprintf(`for d in /proc/[0-9]*; do p=${d#/proc/}; [ "$p" = "$$" ] && continue; tr '\0' '\n' <"$d/environ" 2>/dev/null | grep -qx '%s=%s' && kill -KILL "$p"; done; true`, scheduledRunEnv, runID)
	cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
	defer cancel()
	if _, err := rt.ContainerExec(cctx, containerName, []string{"sh", "-c", script}, nil, nil, nil); err != nil {
		slog.Warn("scheduled task: clean up timed-out processes", "container", containerName, "error", err)
	}
}

// timedOut reports whether exit means timeout(1) fired. 143 is also what a
// command SIGTERMed by something else exits with, so it only counts when the run
// lasted the whole limit.
func timedOut(exit int, elapsed, limit time.Duration) bool {
	if exit == timeoutExitCode {
		return true
	}
	return exit == sigtermExitCode && elapsed >= limit-time.Second
}

// sweepScheduledTasks is the FOURTH sweep in HandleBackupScheduleSweep. Same
// shape as the backup sweeps: a per-minute walk comparing sched.Next(baseline)
// to now, so a schedule can drift by up to 60s and `* * * * *` works.
func (h *TaskHandler) sweepScheduledTasks(ctx context.Context, now time.Time) {
	// First, so a run orphaned by a worker restart cannot pin the global cap.
	if n, err := h.Queries.ReapStaleScheduledTaskRuns(ctx); err != nil {
		slog.Warn("task sweep: reap stale runs", "error", err)
	} else if n > 0 {
		slog.Warn("task sweep: marked abandoned runs failed", "count", n)
	}

	tasks, err := h.Queries.ListEnabledScheduledTasks(ctx)
	if err != nil {
		slog.Warn("task sweep: list tasks", "error", err)
		return
	}
	max := 0
	if h.Config != nil {
		max = h.Config.ScheduledTaskMaxConcurrent
	}
	disp := service.NewScheduledTaskDispatcher(h.Queries, h.Enqueuer, max)

	for _, t := range tasks {
		if !scheduledTaskDue(t, now) {
			continue
		}
		if _, _, err := disp.Dispatch(ctx, t, service.RunTriggerSchedule); err != nil {
			// Baseline untouched, so the next sweep retries.
			slog.Warn("task sweep: dispatch", "task_id", formatUUID(t.ID), "error", err)
			continue
		}
		// A skip also consumes the occurrence: it is recorded on the run list, and
		// leaving the baseline would re-record the same skip every minute.
		if err := h.Queries.SetScheduledTaskLastRun(ctx, generated.SetScheduledTaskLastRunParams{
			ID: t.ID, LastRunAt: pgtype.Timestamptz{Time: now, Valid: true},
		}); err != nil {
			slog.Warn("task sweep: stamp last_run_at", "task_id", formatUUID(t.ID), "error", err)
		}
	}
}

// scheduledTaskDue mirrors backupConfigDue: baseline is last_run_at, or
// created_at for a task that has never run (else a task created at 3:05pm with
// `0 3 * * *` would fire immediately). The Next(baseline).After(now) comparison
// is deliberately copied: after downtime it fires ONCE, never once per missed tick.
func scheduledTaskDue(t generated.ApplicationScheduledTask, now time.Time) bool {
	sched, err := cronspec.Parse(t.Cron, t.Timezone)
	if err != nil {
		slog.Warn("task sweep: invalid schedule", "task_id", formatUUID(t.ID), "cron", t.Cron, "timezone", t.Timezone, "error", err)
		return false
	}
	baseline := t.CreatedAt.Time
	if t.LastRunAt.Valid {
		baseline = t.LastRunAt.Time
	}
	return !sched.Next(baseline).After(now)
}

// HandleScheduledTaskRun execs the task's command inside the application's
// RUNNING container and records the outcome on the run row.
func (h *TaskHandler) HandleScheduledTaskRun(ctx context.Context, t *asynq.Task) error {
	var payload service.ScheduledTaskRunPayload
	if err := json.Unmarshal(t.Payload(), &payload); err != nil {
		return errors.Join(fmt.Errorf("unmarshal scheduled_task payload (permanent): %w", err), asynq.SkipRetry)
	}
	runID, err := parseUUID(payload.RunID)
	if err != nil {
		return errors.Join(fmt.Errorf("invalid run_id (permanent): %w", err), asynq.SkipRetry)
	}
	run, err := h.Queries.GetScheduledTaskRun(ctx, runID)
	if err != nil {
		// Task deleted while queued: the run cascaded away with it.
		return errors.Join(fmt.Errorf("get run (permanent): %w", err), asynq.SkipRetry)
	}
	if run.Status != "running" {
		return nil
	}
	task, err := h.Queries.GetScheduledTask(ctx, run.TaskID)
	if err != nil {
		return errors.Join(fmt.Errorf("get task (permanent): %w", err), asynq.SkipRetry)
	}
	appRow, err := h.Queries.GetApplicationWithProjectSlug(ctx, task.ApplicationID)
	if err != nil {
		h.finishScheduledRun(ctx, run.ID, "failed", nil, "", fmt.Sprintf("application lookup failed: %v", err), task)
		return errors.Join(fmt.Errorf("get application (permanent): %w", err), asynq.SkipRetry)
	}
	// Re-checked: the app may have stopped while this sat in the queue.
	if appRow.Status != "running" {
		h.finishScheduledRun(ctx, run.ID, "skipped", nil, "", "application is not running", task)
		return nil
	}
	rt, err := h.Runtimes.For(ctx, appRow.ServerID)
	if err != nil {
		h.finishScheduledRun(ctx, run.ID, "failed", nil, "", fmt.Sprintf("resolve runtime: %v", err), task)
		return nil
	}
	containerName := naming.ContainerName(appRow.ProjectSlug, appRow.Slug, formatUUID(task.ApplicationID))

	// The timeout is enforced INSIDE the container. ContainerExec blocks in
	// stdcopy on a hijacked connection that ignores ctx, and Docker has no
	// kill-exec API, so cancelling from here would leave the process running.
	// The command is passed as argv to `sh -c` so the whole pipeline — not just
	// its first word — is under timeout(1).
	//
	// env tags the whole process tree with the run id: timeout(1) signals only
	// its direct child, so a backgrounded or piped grandchild outlives a timeout
	// (GNU timeout also signals its group, busybox does not). The tag lets
	// killTaggedProcesses find those orphans afterwards.
	cmd := []string{"env", scheduledRunEnv + "=" + payload.RunID, "timeout", fmt.Sprintf("%d", task.TimeoutSeconds), "sh", "-c", task.Command}

	// Backstop only; see above for why it cannot be the primary mechanism.
	execCtx, cancel := context.WithTimeout(ctx, time.Duration(task.TimeoutSeconds)*time.Second+30*time.Second)
	defer cancel()

	out := newCappedOutput(scheduledTaskOutputCap)
	started := time.Now()
	exit, execErr := rt.ContainerExec(execCtx, containerName, cmd, nil, out, out)
	elapsed := time.Since(started)

	output := out.String()
	switch {
	case execErr != nil:
		h.finishScheduledRun(ctx, run.ID, "failed", nil, output, fmt.Sprintf("exec failed: %v", execErr), task)
		h.notifyScheduledTaskFailure(ctx, task, appRow.Name, appRow.ProjectID, "could not run: "+execErr.Error())
	case exit == 0:
		h.finishScheduledRun(ctx, run.ID, "succeeded", &exit, output, "", task)
	case timedOut(exit, elapsed, time.Duration(task.TimeoutSeconds)*time.Second):
		h.killTaggedProcesses(ctx, rt, containerName, payload.RunID)
		h.finishScheduledRun(ctx, run.ID, "timed_out", &exit, output, "", task)
		h.notifyScheduledTaskFailure(ctx, task, appRow.Name, appRow.ProjectID, fmt.Sprintf("timed out after %ds", task.TimeoutSeconds))
	default:
		h.finishScheduledRun(ctx, run.ID, "failed", &exit, output, "", task)
		h.notifyScheduledTaskFailure(ctx, task, appRow.Name, appRow.ProjectID, fmt.Sprintf("exited with code %d", exit))
	}
	return nil
}

// finishScheduledRun records the outcome and prunes history. note is appended to
// the output (an exec error) or, for 'skipped', stored as the skip reason.
func (h *TaskHandler) finishScheduledRun(ctx context.Context, runID pgtype.UUID, status string, exit *int, output, note string, task generated.ApplicationScheduledTask) {
	p := generated.FinishScheduledTaskRunParams{ID: runID, Status: status, Output: output}
	if exit != nil {
		p.ExitCode = pgtype.Int4{Int32: int32(*exit), Valid: true}
	}
	if status == "skipped" {
		p.SkipReason = pgtype.Text{String: note, Valid: true}
	} else if note != "" {
		if p.Output != "" {
			p.Output += "\n"
		}
		p.Output += note
	}
	// Detached: the run must be recorded even if the task ctx is already done.
	wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	if err := h.Queries.FinishScheduledTaskRun(wctx, p); err != nil {
		slog.Warn("scheduled task: finish run", "run_id", formatUUID(runID), "error", err)
	}
	for _, skipped := range []bool{false, true} {
		if err := h.Queries.PruneScheduledTaskRuns(wctx, generated.PruneScheduledTaskRunsParams{
			TaskID: task.ID, IsSkipped: skipped, Keep: task.KeepRuns,
		}); err != nil {
			slog.Warn("scheduled task: prune runs", "task_id", formatUUID(task.ID), "error", err)
		}
	}
}

// notifyScheduledTaskFailure fires on failure ONLY, never success, like the
// backups. The body names the task but never its command, which may carry a
// pasted secret.
func (h *TaskHandler) notifyScheduledTaskFailure(ctx context.Context, task generated.ApplicationScheduledTask, appName string, projectID pgtype.UUID, what string) {
	h.notifyApplicationOwner(ctx, task.ApplicationID, projectID, notify.EventScheduledTaskFailed,
		"Scheduled task failed",
		fmt.Sprintf("Task %q of %s %s.", task.Name, appName, what))
}

// cappedOutput keeps the first and last cap/2 bytes of a stream and notes how
// much was dropped, so a chatty job cannot bloat a row.
type cappedOutput struct {
	half    int
	head    bytes.Buffer
	tail    []byte
	dropped int64
}

func newCappedOutput(cap int) *cappedOutput { return &cappedOutput{half: cap / 2} }

func (c *cappedOutput) Write(p []byte) (int, error) {
	n := len(p)
	if room := c.half - c.head.Len(); room > 0 {
		take := min(room, len(p))
		c.head.Write(p[:take])
		p = p[take:]
	}
	if len(p) == 0 {
		return n, nil
	}
	c.tail = append(c.tail, p...)
	if over := len(c.tail) - c.half; over > 0 {
		c.dropped += int64(over)
		c.tail = append(c.tail[:0], c.tail[over:]...)
	}
	return n, nil
}

func (c *cappedOutput) String() string {
	if c.dropped == 0 {
		return c.head.String() + string(c.tail)
	}
	return fmt.Sprintf("%s\n… %d bytes omitted …\n%s", c.head.String(), c.dropped, c.tail)
}
