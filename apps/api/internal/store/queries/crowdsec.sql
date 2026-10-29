-- name: GetCrowdSecConfig :one
-- No row is the normal first state: off, no key, default policy.
SELECT * FROM crowdsec_config WHERE id = 'default';

-- name: SetCrowdSecEnabled :one
INSERT INTO crowdsec_config (id, enabled)
VALUES ('default', $1)
ON CONFLICT (id) DO UPDATE SET enabled = EXCLUDED.enabled, updated_at = NOW()
RETURNING *;

-- name: SetCrowdSecBouncerKeyIfUnset :one
-- First writer wins. Two admins pressing Enable at once each generate a key; only
-- one may become THE key, and the loser must register the winner's, not its own —
-- a key that is registered but not stored is a bouncer nobody can authenticate as.
INSERT INTO crowdsec_config (id, bouncer_key_encrypted)
VALUES ('default', $1)
ON CONFLICT (id) DO UPDATE
SET bouncer_key_encrypted = COALESCE(crowdsec_config.bouncer_key_encrypted, EXCLUDED.bouncer_key_encrypted),
    updated_at = NOW()
RETURNING *;

-- name: ReplaceCrowdSecBouncerKey :one
-- Rotation: unlike SetCrowdSecBouncerKeyIfUnset this overwrites.
INSERT INTO crowdsec_config (id, bouncer_key_encrypted)
VALUES ('default', $1)
ON CONFLICT (id) DO UPDATE SET bouncer_key_encrypted = EXCLUDED.bouncer_key_encrypted, updated_at = NOW()
RETURNING *;

-- name: SetCrowdSecAutoban :one
INSERT INTO crowdsec_config (
    id, autoban_enabled, autoban_threshold, autoban_window_minutes,
    autoban_duration_minutes, autoban_max_active, autoban_categories
)
VALUES ('default', $1, $2, $3, $4, $5, $6)
ON CONFLICT (id) DO UPDATE
SET autoban_enabled          = EXCLUDED.autoban_enabled,
    autoban_threshold        = EXCLUDED.autoban_threshold,
    autoban_window_minutes   = EXCLUDED.autoban_window_minutes,
    autoban_duration_minutes = EXCLUDED.autoban_duration_minutes,
    autoban_max_active       = EXCLUDED.autoban_max_active,
    autoban_categories       = EXCLUDED.autoban_categories,
    updated_at               = NOW()
RETURNING *;

-- name: WAFAutobanCandidates :many
-- Addresses that tripped a ban-worthy rule category in at least `threshold`
-- DISTINCT requests since `since`. A request is a tx_id (one request trips many
-- rules, so counting events would let one crafted request reach any threshold);
-- an event with no tx_id counts as its own request rather than all being one.
-- Ordered worst first, so a cap on how many get banned spares the milder ones.
--
-- Blocking mode does not change this count, on two independent grounds, and
-- neither is redundant: (1) it counts DISTINCT tx_id, so extra rows of one
-- transaction add nothing, and (2) the anomaly verdict (949110) is tagged
-- anomaly-evaluation, not attack-*, so it has no category in the set at all.
-- Without (1), a count of hits rather than requests would move with how many
-- rules one request trips. Keep both.
SELECT client_ip,
       count(DISTINCT COALESCE(tx_id, id::text))::bigint AS requests,
       max(recorded_at)::timestamptz AS last_seen,
       array_agg(DISTINCT category ORDER BY category)::text[] AS categories
FROM waf_events
WHERE recorded_at >= sqlc.arg('since')
  AND category = ANY(sqlc.arg('categories')::text[])
GROUP BY client_ip
HAVING count(DISTINCT COALESCE(tx_id, id::text)) >= sqlc.arg('threshold')::int
ORDER BY requests DESC, client_ip
LIMIT sqlc.arg('row_limit');

-- name: ListCrowdSecUnbansSince :many
-- What an admin has lifted lately, from the audit log. The evaluator must not
-- re-ban an address within the window that banned it just because an admin
-- undid it: a ban that comes straight back is not reversible.
SELECT DISTINCT resource_id
FROM audit_logs
WHERE action = 'crowdsec_unban' AND resource_id IS NOT NULL AND created_at >= $1;
