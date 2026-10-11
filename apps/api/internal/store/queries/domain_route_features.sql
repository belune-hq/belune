-- name: ListRouteFeaturesByDomain :many
SELECT * FROM domain_route_features
WHERE domain_id = $1
ORDER BY feature_type;

-- name: UpsertRouteFeature :one
INSERT INTO domain_route_features (domain_id, feature_type, config, enabled)
VALUES ($1, $2, $3, $4)
ON CONFLICT (domain_id, feature_type) DO UPDATE
SET config = EXCLUDED.config, enabled = EXCLUDED.enabled, updated_at = NOW()
RETURNING *;

-- name: DeleteRouteFeature :execrows
-- ⚠️ Keyed on the DOMAIN as well as the feature, and that is the whole point.
-- Keyed on id alone this deleted any route feature on the install: the handler
-- authorizes the {domainId} in the path and nothing tied the {featureId} to
-- it, so a project-pinned token could strip an ip_allowlist or basic_auth
-- control off another project's live public route by naming its own domain.
-- Reproduced before the fix.
--
-- :execrows so the handler can tell "deleted" from "belongs to someone else"
-- and answer 404 rather than reporting success for a row it never touched.
DELETE FROM domain_route_features WHERE id = $1 AND domain_id = $2;

-- name: DeleteRouteFeaturesByDomain :exec
DELETE FROM domain_route_features WHERE domain_id = $1;
