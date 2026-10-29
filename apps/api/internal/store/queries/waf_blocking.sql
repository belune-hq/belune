-- name: ListWAFBlockingApplications :many
-- Every application that serves at least one domain, with the hosts it serves and
-- whether it has opted in to WAF blocking. An application with no domains is not
-- listed: enforcement is per host, so it has nothing to apply to.
--
-- shared_hostnames are this application's hosts that another application which has
-- NOT opted in also serves (a host can be split by path, 000039). The WAF sees the
-- whole host, so such a host stays detection-only however this one is set; the
-- operator is shown that before it surprises them. sqlc.narg filters to one
-- application so the toggle's answer is read through the same shape as the list.
SELECT a.id, a.name, p.name AS project_name, a.waf_blocking,
       array_agg(DISTINCT d.hostname ORDER BY d.hostname)::text[] AS hostnames,
       COALESCE(array_agg(DISTINCT d.hostname ORDER BY d.hostname) FILTER (WHERE EXISTS (
           SELECT 1 FROM domains o
           JOIN applications oa ON oa.id = o.application_id
           WHERE o.hostname = d.hostname AND o.application_id <> a.id AND NOT oa.waf_blocking
       )), '{}')::text[] AS shared_hostnames
FROM applications a
JOIN projects p ON p.id = a.project_id
JOIN domains d ON d.application_id = a.id
WHERE (sqlc.narg('application_id')::uuid IS NULL OR a.id = sqlc.narg('application_id')::uuid)
GROUP BY a.id, a.name, p.name, a.waf_blocking
ORDER BY p.name, a.name, a.id;

-- name: SetApplicationWAFBlocking :one
-- One column, deliberately: this is not a full-row update, so it cannot clear
-- anything it did not mean to. updated_at and config_changed_at stay put — a
-- WAF mode is not container configuration and must not light the pending-change
-- badge or ask for a redeploy.
UPDATE applications SET waf_blocking = sqlc.arg('waf_blocking')
WHERE id = sqlc.arg('id')
RETURNING id, waf_blocking;
