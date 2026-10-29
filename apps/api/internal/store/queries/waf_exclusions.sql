-- name: ListWAFExclusions :many
-- hostnames are the application's domains NOW: they are not stored with the
-- exclusion. An application-scoped row whose application has none comes back with
-- an empty list, and the renderer treats that as inert, never as install-wide.
SELECT
    e.id, e.rule_id, e.collection, e.target_name, e.path_pattern,
    e.application_id, e.note, e.created_by, e.created_at,
    COALESCE(a.name, '')::text AS application_name,
    COALESCE(
        (SELECT array_agg(d.hostname ORDER BY d.hostname)
           FROM domains d WHERE d.application_id = e.application_id),
        '{}'
    )::text[] AS hostnames
FROM waf_exclusions e
LEFT JOIN applications a ON a.id = e.application_id
ORDER BY e.id;

-- name: CountWAFExclusions :one
SELECT count(*) FROM waf_exclusions;

-- name: CreateWAFExclusion :one
-- The ceiling is checked in the statement, so a full table inserts nothing rather
-- than being counted first and raced.
INSERT INTO waf_exclusions (rule_id, collection, target_name, path_pattern, application_id, note, created_by)
SELECT $1, $2, $3, $4, $5, $6, $7
WHERE (SELECT count(*) FROM waf_exclusions) < $8::int
RETURNING *;

-- name: DeleteWAFExclusion :one
DELETE FROM waf_exclusions WHERE id = $1 RETURNING *;

-- name: GetWAFExclusion :one
SELECT * FROM waf_exclusions WHERE id = $1;

-- name: ListWAFBlockingHostnames :many
-- The hostnames whose WAF engine is promoted to blocking. A hostname can be
-- split across applications by path (migration 000039) and the WAF sees the
-- whole host, not the path, so a host blocks only when EVERY application serving
-- it has opted in: bool_and, not bool_or. An ambiguous host stays detection-only,
-- which is the direction a mistake costs a missed block rather than a refused
-- visitor.
SELECT d.hostname::text AS hostname
FROM domains d
JOIN applications a ON a.id = d.application_id
GROUP BY d.hostname
HAVING bool_and(a.waf_blocking)
ORDER BY d.hostname;
