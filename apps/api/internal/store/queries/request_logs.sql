-- name: InsertRequestLog :exec
-- request_uuid is set only for requests the WAF inspected (Caddy mints the uuid
-- when the WAF route interpolates it), and is what a detection joins on.
INSERT INTO request_logs (application_id, method, path, status_code, latency_ms, hostname, request_size, response_size, client_ip, user_agent, request_uuid, content_type)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12);

-- name: ListRequestDetailsByUUID :many
-- The request detail behind a page of WAF detections, looked up by the uuid Caddy
-- writes into both the access log and the transaction id Coraza reports (000073).
--
-- Deliberately a second query rather than a LEFT JOIN on ListWAFEvents: sqlc
-- infers nullability from the source column, so every outer-joined column of this
-- NOT NULL table generated as a non-nullable Go string, and the first unmatched
-- detection would have failed to scan at runtime with no test to catch it. Their
-- own non-outer select keeps the generated types honest, for one extra round trip
-- per page.
--
-- DISTINCT ON because nothing enforces one row per uuid; the newest wins.
SELECT DISTINCT ON (request_uuid)
       request_uuid, method, path, status_code, content_type, user_agent
FROM request_logs
WHERE request_uuid = ANY(sqlc.arg('uuids')::text[])
ORDER BY request_uuid, recorded_at DESC;

-- name: ListRequestLogsByApplication :many
SELECT * FROM request_logs
WHERE application_id = $1
ORDER BY recorded_at DESC
LIMIT $2 OFFSET $3;

-- name: ListRequestLogs :many
SELECT * FROM request_logs
ORDER BY recorded_at DESC
LIMIT $1 OFFSET $2;

-- name: ListRequestLogsFiltered :many
SELECT * FROM request_logs
WHERE (sqlc.narg('application_id')::uuid IS NULL OR application_id = sqlc.narg('application_id'))
  AND (sqlc.narg('status_min')::smallint IS NULL OR status_code >= sqlc.narg('status_min'))
  AND (sqlc.narg('status_max')::smallint IS NULL OR status_code < sqlc.narg('status_max'))
  AND (sqlc.narg('from')::timestamptz IS NULL OR recorded_at >= sqlc.narg('from'))
  AND (sqlc.narg('to')::timestamptz IS NULL OR recorded_at <= sqlc.narg('to'))
  AND (sqlc.narg('search')::text IS NULL
       OR path ILIKE '%' || sqlc.narg('search') || '%'
       OR client_ip ILIKE '%' || sqlc.narg('search') || '%')
  -- project_ids is the PAT pin set, not a user-facing filter. None of the
  -- /api/requests routes has a {projectId} param, so
  -- middleware.RequirePinnedProjectMatch never fires on them and a pinned token
  -- would otherwise read request logs for every project its owner can reach —
  -- the same hole ListDomainsWithTLSStatus closes in its own query.
  --
  -- Via a subquery rather than a join on purpose: these are SELECT * and their
  -- generated row type is written straight to the response, so a join would
  -- change the wire shape and the published OpenAPI spec with it.
  --
  -- A non-NULL, even EMPTY, array matches nothing, which is correct: a token
  -- pinned only to projects that have since been deleted reaches none of them.
  AND (sqlc.narg('project_ids')::uuid[] IS NULL
       OR application_id IN (SELECT a.id FROM applications a
                             WHERE a.project_id = ANY(sqlc.narg('project_ids'))))
ORDER BY recorded_at DESC
LIMIT $1 OFFSET $2;

-- name: GetRequestSummary :one
-- Scalar aggregates over the same filter window the list uses: status-class
-- counts and latency percentiles. Error rate is derived in Go from class_5xx.
SELECT
  count(*)                                                          AS total,
  count(*) FILTER (WHERE status_code >= 200 AND status_code < 300)  AS class_2xx,
  count(*) FILTER (WHERE status_code >= 300 AND status_code < 400)  AS class_3xx,
  count(*) FILTER (WHERE status_code >= 400 AND status_code < 500)  AS class_4xx,
  count(*) FILTER (WHERE status_code >= 500)                        AS class_5xx,
  COALESCE(percentile_cont(0.5) WITHIN GROUP (ORDER BY latency_ms), 0)::float8  AS p50_ms,
  COALESCE(percentile_cont(0.95) WITHIN GROUP (ORDER BY latency_ms), 0)::float8 AS p95_ms
FROM request_logs
WHERE (sqlc.narg('application_id')::uuid IS NULL OR application_id = sqlc.narg('application_id'))
  AND (sqlc.narg('status_min')::smallint IS NULL OR status_code >= sqlc.narg('status_min'))
  AND (sqlc.narg('status_max')::smallint IS NULL OR status_code < sqlc.narg('status_max'))
  AND (sqlc.narg('from')::timestamptz IS NULL OR recorded_at >= sqlc.narg('from'))
  AND (sqlc.narg('to')::timestamptz IS NULL OR recorded_at <= sqlc.narg('to'))
  AND (sqlc.narg('search')::text IS NULL
       OR path ILIKE '%' || sqlc.narg('search') || '%'
       OR client_ip ILIKE '%' || sqlc.narg('search') || '%')
  -- The PAT pin set — see ListRequestLogsFiltered above for why. The summary
  -- and the series need it as much as the list does: without it a pinned token
  -- cannot read another project's requests but can still count them, time them
  -- and watch them by the minute.
  AND (sqlc.narg('project_ids')::uuid[] IS NULL
       OR application_id IN (SELECT a.id FROM applications a
                             WHERE a.project_id = ANY(sqlc.narg('project_ids'))));

-- name: GetRequestPerMinute :many
-- Per-minute request counts for the requests/min sparkline, most-recent first
-- (capped at 60 buckets to bound the payload). Same filters as the list.
SELECT date_trunc('minute', recorded_at)::timestamptz AS bucket, count(*) AS count
FROM request_logs
WHERE (sqlc.narg('application_id')::uuid IS NULL OR application_id = sqlc.narg('application_id'))
  AND (sqlc.narg('status_min')::smallint IS NULL OR status_code >= sqlc.narg('status_min'))
  AND (sqlc.narg('status_max')::smallint IS NULL OR status_code < sqlc.narg('status_max'))
  AND (sqlc.narg('from')::timestamptz IS NULL OR recorded_at >= sqlc.narg('from'))
  AND (sqlc.narg('to')::timestamptz IS NULL OR recorded_at <= sqlc.narg('to'))
  AND (sqlc.narg('search')::text IS NULL
       OR path ILIKE '%' || sqlc.narg('search') || '%'
       OR client_ip ILIKE '%' || sqlc.narg('search') || '%')
  -- The PAT pin set — see ListRequestLogsFiltered above for why. The summary
  -- and the series need it as much as the list does: without it a pinned token
  -- cannot read another project's requests but can still count them, time them
  -- and watch them by the minute.
  AND (sqlc.narg('project_ids')::uuid[] IS NULL
       OR application_id IN (SELECT a.id FROM applications a
                             WHERE a.project_id = ANY(sqlc.narg('project_ids'))))
GROUP BY bucket
ORDER BY bucket DESC
LIMIT 60;

-- name: DeleteOldRequestLogs :exec
DELETE FROM request_logs WHERE recorded_at < NOW() - ($1 || ' days')::INTERVAL;
