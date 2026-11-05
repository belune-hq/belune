-- name: InsertContainerLog :exec
INSERT INTO container_logs (source_type, source_id, level, stream, message, recorded_at, deployment_id, container_id, process_type)
VALUES ($1, $2, $3, $4, $5, COALESCE(sqlc.narg('recorded_at')::timestamptz, NOW()), sqlc.narg('deployment_id'), sqlc.narg('container_id'), sqlc.narg('process_type'));

-- name: BulkInsertContainerLogs :copyfrom
-- Bulk path for the log collector, which flushes batches of lines. Inserting
-- them one statement at a time cost a round trip per line and dominated the
-- write cost of the busiest table in the system.
--
-- CopyFrom takes plain column values, so recorded_at has no COALESCE fallback
-- here: the caller always supplies it, using the timestamp Docker reported for
-- the line and its own clock only when that is missing. That is closer to when
-- the line was actually observed than evaluating NOW() at flush time anyway.
INSERT INTO container_logs (source_type, source_id, level, stream, message, recorded_at, deployment_id, container_id, process_type)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9);

-- name: SearchContainerLogs :many
SELECT id, source_type, source_id, level, stream, message, recorded_at, deployment_id, container_id, process_type FROM container_logs
WHERE source_type = sqlc.arg('source_type')
  AND source_id = sqlc.arg('source_id')
  AND (sqlc.narg('level')::text IS NULL OR level = sqlc.narg('level')::text)
  AND (sqlc.narg('stream')::text IS NULL OR stream = sqlc.narg('stream')::text)
  AND (sqlc.narg('q')::text IS NULL OR message ILIKE '%' || sqlc.narg('q')::text || '%')
  AND (sqlc.narg('since')::timestamptz IS NULL OR recorded_at >= sqlc.narg('since')::timestamptz)
  AND (sqlc.narg('until')::timestamptz IS NULL OR recorded_at <= sqlc.narg('until')::timestamptz)
  -- Session filter: one container generation, the unassigned (NULL) bucket, or
  -- no filter at all. 'unassigned' lets the caller ask for the "earlier" bucket
  -- of rows collected before sessions existed, which a nullable container_id arg
  -- alone could not distinguish from "any".
  AND (CASE
    WHEN sqlc.narg('container_id')::text IS NOT NULL THEN container_id = sqlc.narg('container_id')::text
    WHEN sqlc.narg('unassigned')::boolean THEN container_id IS NULL
    ELSE TRUE
  END)
  -- Process type filter. Matches the column, so it reaches lines from earlier
  -- container generations too; NULL rows (unknown) never match a named type.
  AND (sqlc.narg('process_type')::text IS NULL OR process_type = sqlc.narg('process_type')::text)
ORDER BY recorded_at DESC
LIMIT sqlc.arg('limit') OFFSET sqlc.arg('offset');

-- name: ListContainerLogSessions :many
-- One row per container generation that has produced log lines for this source,
-- most recent first.
--
-- Keyed by container_id rather than deployment_id so every source type gets
-- sessions: a database has no deployment row but is still replaced by a new
-- container on a major-version upgrade (and again if that rolls back), and those
-- runs need separating just as an application's redeploys do.
--
-- The deployment is joined only for labelling — it is what lets an application's
-- session read "#a1b2c3 · rollback · 2h ago" instead of a bare time range.
-- Grouping by both keys is safe because a container belongs to exactly one
-- deployment. The LEFT JOIN keeps sessions with no deployment (databases) and
-- the NULL "earlier" bucket (rows collected before sessions existed).
--
-- Bounded: an application accumulates a session per deploy for its whole life,
-- and every row here becomes an entry in the viewer's session picker. The cap
-- keeps both the response and that dropdown finite. Truncation is safe — the
-- viewer falls back to a short id for any session it cannot label, and "All
-- deployments" still reads every line regardless of what the picker lists.
SELECT cl.container_id,
       cl.deployment_id,
       -- MAX, not a GROUP BY key: a container whose first lines predate the column
       -- has NULL and non-NULL rows under one container_id, and grouping on the
       -- type would split that one session in two.
       MAX(cl.process_type)             AS process_type,
       MIN(cl.recorded_at)::timestamptz AS first_at,
       MAX(cl.recorded_at)::timestamptz AS last_at,
       COUNT(*)                         AS line_count,
       d.triggered_by,
       d.status,
       d.commit_sha,
       d.started_at
FROM container_logs cl
LEFT JOIN deployments d ON d.id = cl.deployment_id
WHERE cl.source_type = $1 AND cl.source_id = $2
GROUP BY cl.container_id, cl.deployment_id, d.triggered_by, d.status, d.commit_sha, d.started_at
ORDER BY MAX(cl.recorded_at) DESC
LIMIT sqlc.arg('limit');

-- name: GetLatestContainerLogTime :one
SELECT MAX(recorded_at)::timestamptz FROM container_logs
WHERE source_type = $1 AND source_id = $2;

-- name: DeleteOldContainerLogs :exec
DELETE FROM container_logs WHERE recorded_at < NOW() - ($1 || ' days')::INTERVAL;
