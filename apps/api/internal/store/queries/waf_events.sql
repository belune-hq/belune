-- name: BulkInsertWAFEvents :copyfrom
-- Bulk path for the recorder, which flushes batches: under attack this table
-- takes writes at the rate requests arrive, so one round trip per row is the
-- wrong shape (see BulkInsertContainerLogs, which had the same problem).
INSERT INTO waf_events (recorded_at, client_ip, rule_id, severity, category, message, host, application_id, tx_id)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9);

-- name: ListWAFEvents :many
-- Keyset pagination on id (newest first): an offset would re-scan the head of a
-- table that is being written to while it is read.
SELECT id, recorded_at, client_ip, rule_id, severity, category, message, host, application_id, tx_id
FROM waf_events
WHERE (sqlc.narg('before_id')::bigint IS NULL OR id < sqlc.narg('before_id')::bigint)
  AND (sqlc.narg('client_ip')::inet IS NULL OR client_ip = sqlc.narg('client_ip')::inet)
  AND (sqlc.narg('rule_id')::int IS NULL OR rule_id = sqlc.narg('rule_id')::int)
  -- NULL means every event still kept, which is what every caller got before the
  -- list could be scoped. It is set when the list sits under a windowed count, so
  -- that expanding a client showing three hits cannot list forty.
  AND (sqlc.narg('since')::timestamptz IS NULL OR recorded_at >= sqlc.narg('since')::timestamptz)
ORDER BY id DESC
LIMIT sqlc.arg('row_limit');

-- name: WAFEventTotals :one
SELECT count(*)::bigint AS events,
       count(DISTINCT client_ip)::bigint AS clients,
       count(DISTINCT tx_id)::bigint AS requests
FROM waf_events
WHERE recorded_at >= sqlc.arg('since');

-- name: TopWAFRules :many
-- message is per rule, so max() only picks the one value deterministically.
SELECT rule_id, max(message)::text AS message, COALESCE(max(category), '')::text AS category, count(*)::bigint AS events
FROM waf_events
WHERE recorded_at >= sqlc.arg('since')
GROUP BY rule_id
ORDER BY events DESC, rule_id
LIMIT sqlc.arg('row_limit');

-- name: TopWAFClients :many
SELECT client_ip, count(*)::bigint AS events, max(recorded_at)::timestamptz AS last_seen
FROM waf_events
WHERE recorded_at >= sqlc.arg('since')
GROUP BY client_ip
ORDER BY events DESC, client_ip
LIMIT sqlc.arg('row_limit');

-- name: WAFClientSummary :many
-- One row per client that tripped a rule in the window: how many rules it
-- tripped, across how many requests and URLs, and whether any one of those
-- requests would have been refused. This is the shape the detections table is
-- read in — a thousand rows of one scanner are one line here.
--
-- The URL count comes from the request log, through the uuid Caddy writes into
-- the access entry and the transaction id alike (000073), via LATERAL ... LIMIT 1
-- and NOT a plain join: nothing enforces one request_logs row per uuid, so a
-- second matching row would multiply count(*) and report twice the hits that
-- happened. Newest row wins, as in ListRequestDetailsByUUID.
--
-- No outer-joined column is selected, only aggregated, so sqlc has no source
-- column here to infer the wrong nullability from.
SELECT e.client_ip,
       count(*)::bigint                   AS events,
       count(DISTINCT e.tx_id)::bigint    AS requests,
       count(DISTINCT e.rule_id)::bigint  AS rules,
       count(DISTINCT req.path)::bigint   AS urls,
       max(e.recorded_at)::timestamptz    AS last_seen,
       COALESCE(bool_or(e.rule_id = sqlc.arg('anomaly_rule')::int), false)::boolean AS would_block
FROM waf_events e
LEFT JOIN LATERAL (
    SELECT r.path FROM request_logs r
    WHERE r.request_uuid = e.tx_id
    ORDER BY r.recorded_at DESC
    LIMIT 1
) req ON true
WHERE e.recorded_at >= sqlc.arg('since')
  AND (sqlc.narg('client_ip')::inet IS NULL OR e.client_ip = sqlc.narg('client_ip')::inet)
  AND (sqlc.narg('rule_id')::int IS NULL OR e.rule_id = sqlc.narg('rule_id')::int)
GROUP BY e.client_ip
-- Loudest first. Recency is the window's job, not the sort's: a one-hit scanner
-- from a minute ago must not push a sustained attack off a capped list.
ORDER BY events DESC, last_seen DESC, e.client_ip
LIMIT sqlc.arg('row_limit');

-- name: CountWAFEvents :one
SELECT count(*)::bigint FROM waf_events;

-- name: DeleteOldWAFEvents :execrows
DELETE FROM waf_events WHERE recorded_at < sqlc.arg('before');

-- name: TrimWAFEvents :execrows
-- Keep the newest `keep` rows. A detection table grows without bound exactly
-- when the install is under attack, so age alone is not a limit: a flood can
-- write more in an hour than a week of retention would ever expect. With fewer
-- than `keep` rows the subquery is NULL and nothing is deleted.
DELETE FROM waf_events
WHERE id <= (SELECT id FROM waf_events ORDER BY id DESC OFFSET sqlc.arg('keep')::int LIMIT 1);

-- name: ListApplicationIDsForHost :many
-- The applications serving a host. A host can be split across applications by
-- path (migration 000039), and an event carries no path — the URI is attacker
-- text and deliberately not kept — so the caller attributes the event only when
-- this returns exactly one row. LIMIT 2 is enough to tell one from several.
SELECT DISTINCT application_id FROM domains WHERE hostname = $1 LIMIT 2;
