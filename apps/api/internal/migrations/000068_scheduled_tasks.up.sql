-- Scheduled (cron) tasks: a command exec'd into an application's RUNNING
-- container on a schedule. Output lives on the run row rather than in
-- container_logs, whose source_type CHECK would need altering on a live table.
CREATE TABLE application_scheduled_tasks (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    application_id  UUID NOT NULL REFERENCES applications(id) ON DELETE CASCADE,
    name            TEXT NOT NULL,
    command         TEXT NOT NULL,
    cron            TEXT NOT NULL,
    timezone        TEXT NOT NULL DEFAULT 'UTC',
    enabled         BOOLEAN NOT NULL DEFAULT true,
    timeout_seconds INT NOT NULL DEFAULT 300 CHECK (timeout_seconds BETWEEN 1 AND 86400),
    keep_runs       INT NOT NULL DEFAULT 20 CHECK (keep_runs BETWEEN 1 AND 1000),
    -- Stamped by SCHEDULED dispatches only, so a manual run never shifts the schedule.
    last_run_at     TIMESTAMPTZ,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_scheduled_tasks_application ON application_scheduled_tasks(application_id);

-- Unlike backup_runs.trigger (000057, unconstrained), trigger is CHECKed here:
-- the sweep and the UI both branch on it.
CREATE TABLE application_scheduled_task_runs (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    task_id     UUID NOT NULL REFERENCES application_scheduled_tasks(id) ON DELETE CASCADE,
    trigger     TEXT NOT NULL DEFAULT 'schedule' CHECK (trigger IN ('schedule','manual')),
    status      TEXT NOT NULL DEFAULT 'running'
                CHECK (status IN ('running','succeeded','failed','skipped','timed_out')),
    exit_code   INT,
    output      TEXT NOT NULL DEFAULT '',
    skip_reason TEXT,
    started_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),  -- dispatch time; the run may queue after this
    picked_up_at TIMESTAMPTZ,                         -- when a worker began the exec; the reaper measures from here
    finished_at TIMESTAMPTZ
);

CREATE INDEX idx_scheduled_task_runs_task ON application_scheduled_task_runs(task_id, started_at DESC);
CREATE INDEX idx_scheduled_task_runs_running ON application_scheduled_task_runs(status) WHERE status = 'running';

-- Backup schedules were silently interpreted in the server's local zone (UTC in
-- practice). DEFAULT 'UTC' keeps every existing schedule firing exactly when it
-- did before.
ALTER TABLE database_backup_configs ADD COLUMN timezone TEXT NOT NULL DEFAULT 'UTC';
ALTER TABLE application_volume_backup_configs ADD COLUMN timezone TEXT NOT NULL DEFAULT 'UTC';

-- Before this column, the only way to schedule in a zone was a CRON_TZ=/TZ=
-- prefix inside the expression, which robfig accepted. The parser now takes the
-- zone from its own column and rejects the prefix, so an existing prefixed
-- schedule would stop firing silently. Lift the zone out: the firing time is
-- identical, and the rewrite only happens for names Postgres knows verbatim
-- (Go's LoadLocation is case-sensitive too); anything else was already failing
-- to parse before this change and is left alone.
UPDATE database_backup_configs c
SET timezone = m[1], schedule = m[2]
FROM (SELECT id, regexp_match(schedule, '^\s*(?:CRON_TZ|TZ)=(\S+)\s+(.*\S)\s*$') AS m FROM database_backup_configs) x
WHERE c.id = x.id AND x.m IS NOT NULL
  AND EXISTS (SELECT 1 FROM pg_timezone_names n WHERE n.name = x.m[1]);

UPDATE application_volume_backup_configs c
SET timezone = m[1], schedule = m[2]
FROM (SELECT id, regexp_match(schedule, '^\s*(?:CRON_TZ|TZ)=(\S+)\s+(.*\S)\s*$') AS m FROM application_volume_backup_configs) x
WHERE c.id = x.id AND x.m IS NOT NULL
  AND EXISTS (SELECT 1 FROM pg_timezone_names n WHERE n.name = x.m[1]);
