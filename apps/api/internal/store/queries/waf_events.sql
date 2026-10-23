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
