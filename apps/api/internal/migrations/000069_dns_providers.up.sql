-- DNS provider credentials for the ACME DNS-01 challenge (ssl_mode=dns_challenge,
-- and wildcard certificates, which Let's Encrypt only issues over DNS-01).
--
-- Install-level rather than project-scoped: a provider token is scoped by the DNS
-- zones it can edit, not by Belune project, and the Caddy TLS automation policy
-- that consumes it is itself global. Managed by admins only, because attaching
-- one to a domain lets that domain obtain a certificate for any name in the
-- token's zones.
--
-- Same shape as backup_destinations (000023): a provider label, plus keyring-
-- encrypted JSON holding every provider-specific field, so a new provider is a
-- CHECK edit rather than a column per provider.
CREATE TABLE dns_providers (
    id                    UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    name                  TEXT NOT NULL UNIQUE,
    provider              TEXT NOT NULL
        CHECK (provider IN ('cloudflare', 'route53', 'digitalocean')),
    -- keyring-encrypted JSON of the provider's Caddy module fields
    -- (e.g. cloudflare: {api_token}; route53: {access_key_id, secret_access_key, ...})
    credentials_encrypted BYTEA NOT NULL,
    created_at            TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at            TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- Per-domain pointer to the provider, NOT a reuse of domains.ssl_credentials_encrypted
-- (000001): that column is per-domain, this design is per-provider (one token
-- serves a whole zone), and it has never been written, so there is nothing to
-- backfill. It stays, dead, because update.sh runs migrations against live
-- installs and dropping a column is not additive.
--
-- RESTRICT, not SET NULL: deleting a provider that a domain relies on must be
-- refused with a count, not silently turn the domain into one that can never
-- get a certificate.
ALTER TABLE domains
    ADD COLUMN dns_provider_id UUID REFERENCES dns_providers(id) ON DELETE RESTRICT;

CREATE INDEX idx_domains_dns_provider_id ON domains(dns_provider_id) WHERE dns_provider_id IS NOT NULL;
