-- The request detail behind a WAF detection.
--
-- A waf_events row deliberately carries no request data: the URI and the matched
-- payload are attacker-controlled and can hold the operator's own credentials, so
-- 000070 keeps a rule id, severity, client, host and time and nothing else. That
-- makes a detection honest and also unjudgeable — rule 920420 ("Request content
-- type is not allowed by policy") fires on a benign text/plain POST, and from the
-- stored row alone that is indistinguishable from an attack.
--
-- The missing detail is already recorded, one table over. request_logs holds the
-- URI (with the deploy-hook token and ?token= redacted at Caddy's encoder, see
-- ConfigureAccessLogs) plus the method and status. What was missing is a key to
-- reach it by, and Caddy already writes one: every access entry for a request the
-- WAF inspected carries a top-level `uuid`, which is the same value that rides in
-- the transaction id Coraza logs. So the join needs a column, not a copy.
--
-- ⚠️ INVARIANT: that uuid exists in the access log only because the WAF route
-- interpolates {http.request.uuid} into its tx header (proxy/caddy/waf.go) —
-- referencing the placeholder is what makes Caddy mint and log it. Stop
-- interpolating it and this column goes quietly NULL for every new row, with the
-- join returning "no detail" and no error anywhere. TestWAFRouteCarriesRequestUUID
-- guards it.
ALTER TABLE request_logs
    -- Null for every request the WAF did not inspect, and for every row written
    -- before this migration. A detection older than request_log_retention_days has
    -- no row to find at all, which the API reports as absent detail rather than as
    -- an error.
    ADD COLUMN request_uuid TEXT,
    -- The second allowlisted request header, after user_agent, which this table
    -- has carried since 000001: it is what makes 920420 legible. Headers are an
    -- allowlist, never a denylist — Caddy redacts Cookie and Authorization for
    -- us, but not a custom X-Api-Token an application invents, so nothing is
    -- stored unless it is named here. Two are named; anything telling the
    -- operator otherwise is understating what is kept.
    ADD COLUMN content_type TEXT;

-- Partial: only WAF-inspected requests carry a uuid, so this indexes a small
-- fraction of a high-volume table. The join reads it by equality and nothing else.
CREATE INDEX idx_request_logs_uuid ON request_logs (request_uuid)
    WHERE request_uuid IS NOT NULL;
