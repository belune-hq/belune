-- name: GetQuota :one
SELECT * FROM quotas WHERE scope = $1 AND scope_id = $2;

-- name: UpsertQuota :one
INSERT INTO quotas (scope, scope_id, max_applications, max_cpu, max_memory_mb)
VALUES ($1, $2, $3, $4, $5)
ON CONFLICT (scope, scope_id) DO UPDATE SET
    max_applications = EXCLUDED.max_applications,
    max_cpu          = EXCLUDED.max_cpu,
    max_memory_mb    = EXCLUDED.max_memory_mb,
    updated_at       = NOW()
RETURNING *;

-- name: DeleteQuota :exec
DELETE FROM quotas WHERE scope = $1 AND scope_id = $2;

-- name: ListQuotas :many
-- project_ids is the PAT pin set, not a user-facing filter. This route has no
-- {projectId} param — the project arrives as {scopeId} — so
-- middleware.RequirePinnedProjectMatch never fires on it, and without this a pinned
-- token reads every project's limits and usage.
--
-- A pinned token sees ONLY project-scoped rows inside its pin: a user-scoped
-- quota governs every project that user owns, so returning one would reach past
-- the pin by another route. The same reasoning refuses a pinned token the
-- user scope on the single-quota endpoints.
--
-- Narrowed here rather than after the fetch so there is one enforcement point,
-- and a non-NULL even EMPTY array matches nothing — a token pinned only to
-- deleted projects sees no quotas at all.
SELECT * FROM quotas
WHERE (sqlc.narg('project_ids')::uuid[] IS NULL
       OR (scope = 'project' AND scope_id = ANY(sqlc.narg('project_ids'))))
ORDER BY scope, created_at DESC;

-- name: CountApplicationsByProject :one
SELECT COUNT(*) FROM applications WHERE project_id = $1;

-- name: CountApplicationsByUser :one
-- Deliberately owner-scoped, not shared-inclusive: quotas follow ownership,
-- not access. If a shared project counted against every member, one project
-- would consume N members' quotas simultaneously and sharing would silently
-- shrink everyone's capacity. Do not add "OR p.shared" here.
SELECT COUNT(*)
FROM applications a
JOIN projects p ON p.id = a.project_id
WHERE p.user_id = $1;

-- name: SumApplicationResourcesByProject :one
SELECT
    COALESCE(SUM(cpu_limit), 0)::DOUBLE PRECISION AS cpu_total,
    COALESCE(SUM(memory_limit), 0)::BIGINT        AS memory_total_bytes
FROM applications
WHERE project_id = $1;

-- name: SumApplicationResourcesByUser :one
-- Owner-scoped for the same reason as CountApplicationsByUser above — quotas
-- follow ownership, not access. Do not add "OR p.shared" here.
SELECT
    COALESCE(SUM(a.cpu_limit), 0)::DOUBLE PRECISION AS cpu_total,
    COALESCE(SUM(a.memory_limit), 0)::BIGINT        AS memory_total_bytes
FROM applications a
JOIN projects p ON p.id = a.project_id
WHERE p.user_id = $1;
