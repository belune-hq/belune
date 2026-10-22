-- name: CreateDNSProvider :one
INSERT INTO dns_providers (name, provider, credentials_encrypted)
VALUES ($1, $2, $3)
RETURNING *;

-- name: UpdateDNSProvider :one
-- The provider type is not updatable: domains reference the row, and a Cloudflare
-- token re-labelled as Route 53 would feed one provider's credentials to another.
UPDATE dns_providers
SET name                  = $2,
    -- Keep the stored secret when no new one is supplied (NULL).
    credentials_encrypted = COALESCE($3, credentials_encrypted),
    updated_at            = NOW()
WHERE id = $1
RETURNING *;

-- name: GetDNSProvider :one
SELECT * FROM dns_providers WHERE id = $1;

-- name: ListDNSProviders :many
-- No credentials_encrypted: the list never needs the secret, and leaving the
-- column out means a handler cannot serialise it by accident.
SELECT id, name, provider, created_at, updated_at,
       (SELECT count(*) FROM domains d WHERE d.dns_provider_id = dns_providers.id) AS domain_count
FROM dns_providers
ORDER BY name;

-- name: CountDomainsByDNSProvider :one
SELECT count(*) FROM domains WHERE dns_provider_id = $1;

-- name: DeleteDNSProvider :exec
DELETE FROM dns_providers WHERE id = $1;

-- name: ListDNSChallengeDomains :many
-- Every dns_challenge domain of a RUNNING application with the provider it uses,
-- for the reconciler to turn into Caddy TLS automation policies. Running only,
-- because that is exactly the set buildExpected routes: a policy for a name with
-- no route would attract an issuance nothing serves, and the two sets must agree.
-- LEFT JOIN: a row from before the mode was withdrawn has no provider, and must
-- surface as a degradation rather than vanish from the list.
SELECT d.id AS domain_id, d.hostname, d.path,
       p.id AS provider_id, p.provider, p.credentials_encrypted
FROM domains d
JOIN applications a ON a.id = d.application_id AND a.status = 'running'
LEFT JOIN dns_providers p ON p.id = d.dns_provider_id
WHERE d.ssl_mode = 'dns_challenge'
ORDER BY d.hostname, d.path;
