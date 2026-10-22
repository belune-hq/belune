-- name: ListDomainsByApplication :many
SELECT * FROM domains WHERE application_id = $1 ORDER BY created_at DESC;

-- name: CreateDomain :one
INSERT INTO domains (application_id, hostname, ssl_enabled, container_port, force_https, ssl_mode, ssl_provider, ssl_credentials_encrypted, certificate_id, advanced_config, path, strip_path, internal_path, dns_provider_id)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)
RETURNING *;

-- name: GetDomain :one
SELECT * FROM domains WHERE id = $1;

-- name: GetDomainByHostname :one
-- hostname stopped being unique in migration 000039: one host can now be split
-- across applications by path. This returns the oldest row for the host, which
-- is deterministic but path-blind — fine for the callers that only ask "is this
-- hostname known to us?" or that act on the host as a whole (TLS is per-host,
-- not per-path). It is NOT the right lookup for attributing a single request to
-- an application; that needs the URI too. See GetDomainForRequest.
SELECT * FROM domains WHERE hostname = $1 ORDER BY created_at ASC LIMIT 1;

-- name: GetDomainForRequest :one
-- Resolves one request (host + URI) to the domain serving it, by longest path
-- prefix — the same first-match-wins order Caddy itself applies, so the request
-- log agrees with the proxy about which app answered.
--
-- rtrim strips the trailing slash so '/' becomes '' and matches everything,
-- while '/api' matches '/api' exactly and '/api/...' but never '/apifoo'.
SELECT * FROM domains
WHERE hostname = $1
  AND (
    sqlc.arg(request_uri)::text = rtrim(path, '/')
    OR sqlc.arg(request_uri)::text LIKE rtrim(path, '/') || '/%'
  )
ORDER BY length(path) DESC
LIMIT 1;

-- name: DeleteDomain :exec
DELETE FROM domains WHERE id = $1;

-- name: GetDomainOwnerUserID :one
-- shared rides along so canAccessDomain can grant every Member access to a
-- shared project's domains, not only its owner.
-- project_id rides along so the caller can compose a project-pinned token's
-- pin, which is a separate axis from ownership — GetDatabaseOwnerUserID has
-- carried it for the WebSocket authorizer since v0.1.12, and REST needed it
-- for the same reason: without it canAccessOwned can only ask who owns the
-- resource, never which project it belongs to.
SELECT p.user_id, p.shared, a.project_id FROM domains d
JOIN applications a ON a.id = d.application_id
JOIN projects p ON p.id = a.project_id
WHERE d.id = $1;

-- name: UpdateDomain :one
UPDATE domains SET
    hostname = $2, ssl_enabled = $3, container_port = $4, force_https = $5,
    ssl_mode = $6, ssl_provider = $7, ssl_credentials_encrypted = $8,
    certificate_id = $9, advanced_config = $10, path = $11, strip_path = $12,
    internal_path = $13, dns_provider_id = $14
WHERE id = $1
RETURNING *;

-- name: GetDomainWithFeatures :one
-- One domain in the same shape ListDomainsByApplicationWithFeatures returns, so
-- a caller polling a single domain's tls_status gets the row it already knows
-- rather than a subtly different one missing route_features.
SELECT d.*, COALESCE(
    (SELECT json_agg(json_build_object(
        'id', f.id, 'feature_type', f.feature_type,
        'config', f.config, 'enabled', f.enabled
    )) FROM domain_route_features f WHERE f.domain_id = d.id),
    '[]'::json
) AS route_features
FROM domains d
WHERE d.id = $1;

-- name: ListDomainsByApplicationWithFeatures :many
SELECT d.*, COALESCE(
    (SELECT json_agg(json_build_object(
        'id', f.id, 'feature_type', f.feature_type,
        'config', f.config, 'enabled', f.enabled
    )) FROM domain_route_features f WHERE f.domain_id = d.id),
    '[]'::json
) AS route_features
FROM domains d
WHERE d.application_id = $1
ORDER BY d.created_at DESC;

-- name: ListProjectAppPrimaryDomain :many
-- The first-added domain (hostname + container_port) per parent application in a
-- project, for the project services table. hostname/port are NULL when an app
-- has no domain.
SELECT DISTINCT ON (a.id) a.id AS application_id, d.hostname, d.container_port
FROM applications a
LEFT JOIN domains d ON d.application_id = a.id
WHERE a.project_id = $1 AND a.parent_application_id IS NULL
ORDER BY a.id, d.created_at ASC;

-- name: ListDomainsForTLSProbe :many
-- Every *hostname* the probe worker checks, with the SSL mode that decides what a
-- healthy result even looks like.
--
-- DISTINCT ON hostname, because TLS belongs to the host, not to the row. Since
-- migration 000039 a host can have several domains rows (one per path), and they
-- share one certificate — Caddy selects by SNI, which knows nothing about paths.
-- Probing per row would open N TLS handshakes for one certificate and, worse,
-- make N separate ACME attempts against Let's Encrypt's rate limits for a name
-- that needs exactly one.
SELECT DISTINCT ON (hostname) id, hostname, ssl_mode, ssl_enabled, tls_status, tls_error
FROM domains
ORDER BY hostname, created_at ASC;

-- name: UpdateDomainTLSStatus :exec
-- Keyed by hostname, not id: one probe of one certificate settles the TLS state
-- of every domains row sharing that host. Writing only the probed row would leave
-- its siblings frozen on 'unknown' for ever — the same live certificate, reported
-- as healthy on /api and unknown on /.
--
-- tls_error is authoritative and decides the status; tls_advisory only explains
-- (see migration 000037). Keeping them apart is what stops a DNS suspicion being
-- read back next sweep as a real ACME failure.
UPDATE domains SET
    tls_status = $2,
    tls_issuer = $3,
    tls_not_after = $4,
    tls_error = $5,
    tls_advisory = $6,
    tls_last_checked_at = NOW()
WHERE hostname = $1;

-- name: SetDomainTLSError :exec
-- Records a failure reason without disturbing the observed certificate metadata:
-- used by the Caddy log parser and by a failed SetupTLS, both of which know why
-- TLS is broken but not what (if anything) is currently being served.
--
-- Keyed by hostname for the same reason as UpdateDomainTLSStatus: Caddy reports a
-- certificate failure against a name, and that failure is true of every row
-- serving that name. Recording it on one row would leave a sibling path claiming
-- HTTPS is fine while the host has no usable certificate at all.
UPDATE domains SET
    tls_error = $2,
    tls_status = CASE WHEN tls_status IN ('active', 'expiring') THEN tls_status ELSE 'failed' END,
    tls_last_checked_at = NOW()
WHERE hostname = $1;

-- name: ListDomainsWithTLSStatus :many
-- The central "Domain TLS" table on the certificates page: every domain with the
-- certificate it serves and what the server last observed for it.
--
-- user_id NULL means "every domain on the install" and is what an admin passes;
-- a member passes their own id and gets the same rows narrowed to projects they
-- can reach. Same OR p.shared idiom as ListGlobalDeploymentsFiltered and
-- ListProjectsByUser, and for the same reason: sharing grants operational access,
-- so a shared project's domains are as visible as an owned one's.
--
-- The certificate join is why this matters to a member at all. ListDomainsByApplication
-- is SELECT * FROM domains, so it returns certificate_id as a bare UUID with nothing
-- to resolve it against — this is the only query that turns that into a name.
SELECT d.id, d.hostname, d.ssl_mode, d.tls_status, d.tls_issuer, d.tls_not_after,
       d.tls_last_checked_at, d.tls_error, d.tls_advisory, c.name AS certificate_name,
       a.name AS application_name, a.id AS application_id, p.id AS project_id
FROM domains d
JOIN applications a ON a.id = d.application_id
JOIN projects p ON p.id = a.project_id
LEFT JOIN certificates c ON c.id = d.certificate_id
WHERE (sqlc.narg('user_id')::uuid IS NULL
       OR p.user_id = sqlc.narg('user_id')
       OR p.shared)
  -- project_ids is the PAT pin set, not a user-facing filter. This route has
  -- no {projectId} param, so middleware.RequirePinnedProjectMatch never fires on
  -- it and a pinned token would otherwise read every domain its owner can
  -- reach — the same hole GetGlobalDeployments closes in its own handler. A
  -- non-NULL, even empty, array matches nothing (see ListAllProjectsLimit).
  AND (sqlc.narg('project_ids')::uuid[] IS NULL OR p.id = ANY(sqlc.narg('project_ids')))
ORDER BY d.hostname;

-- name: ListDomainsWithTLSStatusLimit :many
-- Bounded sibling of ListDomainsWithTLSStatus for the MCP
-- list_domain_tls_status tool: same NULL-means-unfiltered user_id handling,
-- plus a LIMIT so a large install's entire domain table isn't the single
-- largest response this server can produce. project_ids is the PAT pin set
-- (see ListAllProjectsLimit for why a non-NULL, even empty, array must match
-- nothing rather than everything), applied here in SQL for the same reason:
-- this route has no {projectId} param, so middleware.RequirePinnedProjectMatch
-- never fires on it and a pinned token would otherwise read every domain its
-- owner can reach. Additive: REST's ListDomainTLSStatus handler keeps using
-- the unbounded query above unchanged.
SELECT d.id, d.hostname, d.ssl_mode, d.tls_status, d.tls_issuer, d.tls_not_after,
       d.tls_last_checked_at, d.tls_error, d.tls_advisory, c.name AS certificate_name,
       a.name AS application_name, a.id AS application_id, p.id AS project_id
FROM domains d
JOIN applications a ON a.id = d.application_id
JOIN projects p ON p.id = a.project_id
LEFT JOIN certificates c ON c.id = d.certificate_id
WHERE (sqlc.narg('user_id')::uuid IS NULL
       OR p.user_id = sqlc.narg('user_id')
       OR p.shared)
  AND (sqlc.narg('project_ids')::uuid[] IS NULL OR p.id = ANY(sqlc.narg('project_ids')))
ORDER BY d.hostname
LIMIT sqlc.arg('row_limit');

-- name: ListDomainsByHostname :many
-- Every row serving a hostname — one per path since migration 000039.
-- Used to enforce that they agree about TLS: they share a single certificate,
-- so "shop.com/ is automatic but shop.com/api is off" is not a configuration,
-- it is a contradiction.
SELECT * FROM domains WHERE hostname = $1 ORDER BY created_at ASC;
