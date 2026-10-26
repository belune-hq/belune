-- CrowdSec: one install-level configuration row, nothing else.
--
-- Bans themselves are not stored here. The CrowdSec local API (the `crowdsec`
-- compose service) is the only place a ban lives, because it is also the only
-- thing the proxy's bouncer reads: a second copy in Postgres would be a ledger
-- that can disagree with what is actually enforced. Who banned or unbanned what,
-- and when, is the audit log's job (crowdsec_ban, crowdsec_unban, ...).
--
-- Singleton, enforced by the primary key: the bouncer is one credential for the
-- whole install, like the Caddy that carries it. The row is created on first
-- write, never seeded, so a missing row means "all defaults, off".
CREATE TABLE crowdsec_config (
    id                    TEXT PRIMARY KEY DEFAULT 'default' CHECK (id = 'default'),
    -- The operator's switch. Capability (does Caddy have the bouncer module) and
    -- health (is the local API answering) are deliberately not stored: they are
    -- observed, and a stored copy would outlive the thing it described.
    enabled               BOOLEAN NOT NULL DEFAULT false,
    -- The key the proxy's bouncer authenticates to the local API with, generated
    -- by Belune on first enable and registered in the local API under a fixed
    -- bouncer name. keyring-encrypted; NULL until first enable. It can read the
    -- ban list and nothing else, but it is still a credential.
    bouncer_key_encrypted BYTEA,

    -- Turning WAF detections into bans. Off, and opt-in twice: the switch here,
    -- and the WAF itself (no detections, nothing to act on).
    autoban_enabled          BOOLEAN NOT NULL DEFAULT false,
    -- Distinct requests, not events: one request trips many rules, and a single
    -- crafted request must not be able to ban an address on its own.
    autoban_threshold        INT NOT NULL DEFAULT 10
        CHECK (autoban_threshold BETWEEN 3 AND 1000),
    autoban_window_minutes   INT NOT NULL DEFAULT 10
        CHECK (autoban_window_minutes BETWEEN 1 AND 1440),
    -- Every automatic ban expires. There is no permanent one.
    autoban_duration_minutes INT NOT NULL DEFAULT 60
        CHECK (autoban_duration_minutes BETWEEN 5 AND 10080),
    -- How many automatic bans may be in force at once. A false-positive storm
    -- stops here instead of banning every visitor of a site that happens to
    -- trip a rule.
    autoban_max_active       INT NOT NULL DEFAULT 25
        CHECK (autoban_max_active BETWEEN 1 AND 500),
    -- Which WAF rule categories count. A closed list, enforced here as well as in
    -- code: attack-protocol is absent on purpose. Its rules (920xxx) are
    -- CRITICAL severity too — severity does not separate an attack from a
    -- benign text/plain POST (920420 fired on exactly that) — so the category,
    -- not the severity, is the key, and the protocol category is never one.
    autoban_categories       TEXT[] NOT NULL DEFAULT ARRAY[
        'attack-injection-java', 'attack-injection-php', 'attack-lfi',
        'attack-rce', 'attack-rfi', 'attack-sqli', 'attack-xss'
    ]
        CHECK (autoban_categories <@ ARRAY[
            'attack-injection-java', 'attack-injection-php', 'attack-lfi',
            'attack-rce', 'attack-rfi', 'attack-sqli', 'attack-ssrf',
            'attack-ssti', 'attack-xss'
        ]),

    updated_at            TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
