-- WAF (Coraza) detection events: one row per rule match, written by the log
-- collector's line hook from Caddy's stderr. Detection only — nothing is blocked.
--
-- What is deliberately NOT here: the matched data and the request URI. Both are
-- attacker-controlled and can carry the user's own credentials (a password field
-- in a POSTed form, a token in a query string, a deploy-hook path). A rule id,
-- severity, client IP, host and time make a detection useful without persisting
-- a payload, and anything stored in a table is one more place for it to leak.
--
-- Not container_logs: Caddy's lines are deliberately not stored there (v0.1.19
-- removed it, because Caddy was 94% of that table and nothing read it back), and
-- this needs structured columns that enforcement will query by client_ip.
CREATE TABLE waf_events (
    id             BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    recorded_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    client_ip      INET NOT NULL,
    rule_id        INT NOT NULL,
    -- 'unknown' is what Coraza reports for a rule that declares no severity, which
    -- includes CRS's anomaly-score summary rule: the one that says a request
    -- would have been blocked.
    severity       TEXT NOT NULL
                   CHECK (severity IN ('emergency','alert','critical','error','warning','notice','info','debug','unknown')),
    -- The rule's attack-* tag (attack-xss, attack-sqli, ...), when it has one.
    category       TEXT,
    -- The rule's static message with its dynamic tail (which can echo request
    -- data) cut off, so it is a property of the rule and not of the request.
    message        TEXT NOT NULL DEFAULT '',
    -- host and application_id are NULL when attribution failed — the transaction
    -- id did not carry a host, or the host no longer maps to an application. The
    -- event is kept either way: a detection with no host is still a detection.
    host           TEXT,
    application_id UUID REFERENCES applications(id) ON DELETE SET NULL,
    -- One request can trip many rules; rows sharing tx_id are one request.
    tx_id          TEXT
);

-- Newest first is the only way this table is read, and the retention sweep deletes
-- from the other end of the same index.
CREATE INDEX idx_waf_events_recorded ON waf_events (recorded_at DESC);
-- Enforcement's access pattern: what has this address been doing lately.
CREATE INDEX idx_waf_events_client ON waf_events (client_ip, recorded_at DESC);
