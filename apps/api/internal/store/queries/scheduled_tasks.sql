-- name: CreateScheduledTask :one
INSERT INTO application_scheduled_tasks (
    application_id, name, command, cron, timezone, enabled, timeout_seconds, keep_runs
)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
RETURNING *;

-- name: UpdateScheduledTask :one
UPDATE application_scheduled_tasks
SET name            = $2,
    command         = $3,
    cron            = $4,
    timezone        = $5,
    enabled         = $6,
    timeout_seconds = $7,
    keep_runs       = $8,
    updated_at      = NOW()
WHERE id = $1
RETURNING *;

-- name: GetScheduledTask :one
SELECT * FROM application_scheduled_tasks WHERE id = $1;

-- name: ListScheduledTasksForApplication :many
SELECT * FROM application_scheduled_tasks
WHERE application_id = $1
ORDER BY created_at;

-- name: DeleteScheduledTask :exec
DELETE FROM application_scheduled_tasks WHERE id = $1;

-- name: ListEnabledScheduledTasks :many
SELECT * FROM application_scheduled_tasks
WHERE enabled
ORDER BY created_at;

-- name: SetScheduledTaskLastRun :exec
-- Deliberately leaves updated_at alone: this is bookkeeping, not an edit.
UPDATE application_scheduled_tasks SET last_run_at = $2 WHERE id = $1;

-- name: InsertScheduledTaskRun :one
INSERT INTO application_scheduled_task_runs (task_id, trigger)
VALUES ($1, $2)
RETURNING *;

-- name: InsertSkippedScheduledTaskRun :one
INSERT INTO application_scheduled_task_runs (task_id, trigger, status, skip_reason, finished_at)
VALUES ($1, $2, 'skipped', $3, NOW())
RETURNING *;

-- name: GetScheduledTaskRun :one
SELECT * FROM application_scheduled_task_runs WHERE id = $1;

-- name: FinishScheduledTaskRun :exec
UPDATE application_scheduled_task_runs
SET status = $2, exit_code = $3, output = $4, skip_reason = $5, finished_at = NOW()
WHERE id = $1;

-- name: DeleteScheduledTaskRun :exec
DELETE FROM application_scheduled_task_runs WHERE id = $1;

-- name: ListScheduledTaskRuns :many
SELECT * FROM application_scheduled_task_runs
WHERE task_id = $1
ORDER BY started_at DESC
LIMIT $2;

-- name: LatestScheduledTaskRuns :many
-- The newest run of each task of an application, for the list badge.
SELECT DISTINCT ON (r.task_id) r.*
FROM application_scheduled_task_runs r
JOIN application_scheduled_tasks t ON t.id = r.task_id
WHERE t.application_id = $1
ORDER BY r.task_id, r.started_at DESC;

-- name: CountRunningScheduledTaskRuns :one
SELECT count(*) FROM application_scheduled_task_runs WHERE status = 'running';

-- name: ScheduledTaskRunInFlight :one
SELECT EXISTS (
    SELECT 1 FROM application_scheduled_task_runs WHERE task_id = $1 AND status = 'running'
);

-- name: ReapStaleScheduledTaskRuns :execrows
-- A worker that died mid-run (restart, OOM) leaves 'running' forever, which
-- would pin the global cap and block the task's next run. The grace covers the
-- in-container timeout plus the worker's own backstop.
UPDATE application_scheduled_task_runs r
SET status = 'failed', finished_at = NOW(),
    output = CASE WHEN r.output = '' THEN 'Run abandoned: the worker stopped before it finished.' ELSE r.output END
FROM application_scheduled_tasks t
WHERE r.task_id = t.id
  AND r.status = 'running'
  AND r.started_at < NOW() - make_interval(secs => t.timeout_seconds + 120);

-- name: PruneScheduledTaskRuns :exec
-- Skipped rows are pruned on their own budget (is_skipped selects the pool) so a
-- stopped app's per-minute skips cannot evict the run output someone wants to read.
DELETE FROM application_scheduled_task_runs r
WHERE r.task_id = @task_id
  AND r.status <> 'running'
  AND (r.status = 'skipped') = @is_skipped::boolean
  AND r.id NOT IN (
    SELECT k.id FROM application_scheduled_task_runs k
    WHERE k.task_id = @task_id
      AND k.status <> 'running'
      AND (k.status = 'skipped') = @is_skipped::boolean
    ORDER BY k.started_at DESC
    LIMIT @keep::int
  );

-- name: CountFailedScheduledTasks :one
-- Distinct tasks whose latest FINISHED, non-skipped run failed — not failed
-- runs, or one task failing every minute would drown the number. Running and
-- skipped runs are ignored so neither can mask an unfixed failure.
WITH latest AS (
    SELECT DISTINCT ON (r.task_id) r.status
    FROM application_scheduled_task_runs r
    JOIN application_scheduled_tasks t ON t.id = r.task_id
    JOIN applications a ON a.id = t.application_id
    JOIN projects p ON p.id = a.project_id
    WHERE t.enabled
      AND r.status IN ('succeeded', 'failed', 'timed_out')
      AND (sqlc.narg('user_id')::uuid IS NULL OR p.user_id = sqlc.narg('user_id') OR p.shared)
    ORDER BY r.task_id, r.started_at DESC
)
SELECT count(*)::bigint FROM latest WHERE status IN ('failed', 'timed_out');
