-- WAF exclusions: places where the operator has told the rule set to stop looking.
-- Each is a SECURITY DOWNGRADE, which is why the table is as constrained as it is.
--
-- Three shapes share the table:
--   a target   collection + target_name   one named request field, for one rule
--   a path     path_pattern               one rule, on requests to one path
--   the rule   neither                    one rule, off
--
-- ⚠️ application_id is ON DELETE CASCADE and must never become SET NULL. NULL here
-- means "install-wide", so SET NULL would not orphan an application's exclusion, it
-- would PROMOTE it: a deleted app's carve-out would silently become one that
-- disables the rule for every application. (This codebase has had SET NULL advice
-- turn out wrong before; this is a new reason for the same answer.)
--
-- Hosts are deliberately not stored. They are the application's current domains,
-- resolved when the directives are rendered, so a renamed or deleted domain drops
-- out and nothing can reference a hostname that no longer exists.
--
-- rule_id is held to the request-side detection rules (910000-944999). 949110,
-- 959100 and 980130 evaluate and report the anomaly score, and 901/905 initialise
-- the transaction: excluding one removes the verdict or the engine rather than
-- quieting a false positive. The application refuses them with a reason; this
-- check is the backstop for a row that arrives any other way.
CREATE TABLE waf_exclusions (
    id             BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    rule_id        INTEGER NOT NULL CHECK (rule_id BETWEEN 910000 AND 944999),
    collection     TEXT,
    target_name    TEXT,
    path_pattern   TEXT,
    application_id UUID REFERENCES applications(id) ON DELETE CASCADE,
    note           TEXT NOT NULL DEFAULT '' CHECK (length(note) <= 500),
    created_by     UUID REFERENCES users(id) ON DELETE SET NULL,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CHECK ((collection IS NULL) = (target_name IS NULL)),
    CHECK (collection IS NULL OR path_pattern IS NULL)
);

-- The same carve-out twice is a mistake, not a second protection.
CREATE UNIQUE INDEX uq_waf_exclusions ON waf_exclusions (
    rule_id,
    COALESCE(collection, ''),
    COALESCE(target_name, ''),
    COALESCE(path_pattern, ''),
    COALESCE(application_id, '00000000-0000-0000-0000-000000000000'::uuid)
);

CREATE INDEX idx_waf_exclusions_application ON waf_exclusions (application_id);
