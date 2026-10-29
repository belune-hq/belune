-- What Coraza did with the request, and the score that decided it.
--
-- Until now every stored detection was a "would have": the engine ran
-- DetectionOnly, so a 949110 row meant "a blocking WAF would have refused
-- this" and nothing was ever refused. Blocking mode (per host, 0.2.7) makes
-- that reading false for exactly the rows that matter most, and waf_events
-- could not say which requests were actually stopped. This is the column that
-- can, and it ships a step before the feature that needs it.
--
-- disposition is read from the head of the log line by waf.parseMatch as a
-- strict prefix against a closed set, never by searching the message: that
-- stretch is the one unquoted region CRS expands request data into, so a
-- substring match would let a payload label its own detection "blocked".
-- NULL is the honest value for every row written before this migration and for
-- any line whose prefix was neither literal. No default, on purpose: a default
-- of 'detected' would assert something about rows nobody classified.
--
-- ⚠️ It is per ROW, and "was this request blocked" is per TRANSACTION. Only the
-- deny-ing rule logs "Access denied" (949110 in a CRS setup); the signature
-- rules that fed its score logged "Warning." under the same tx_id. Readers must
-- ask EXISTS(... tx_id = x AND disposition = 'blocked') over the unfiltered
-- rows, never aggregate the rows they happened to select.
--
-- anomaly_score is the "(Total Score: N)" the scoring rule's message carries and
-- staticMessage cuts off. NULL for every other rule. Both columns are nullable
-- and bounded because BulkInsertWAFEvents is COPY, which fails whole: a value
-- Postgres refuses discards the entire batch of detections.
ALTER TABLE waf_events
    ADD COLUMN disposition TEXT CHECK (disposition IN ('blocked', 'detected')),
    ADD COLUMN anomaly_score INTEGER CHECK (anomaly_score BETWEEN 0 AND 999999);

-- "Was this request blocked" is asked by tx_id for every row on a page, and the
-- table had no index on it. Partial: a row without a transaction id has nothing
-- to look up.
CREATE INDEX idx_waf_events_tx ON waf_events (tx_id) WHERE tx_id IS NOT NULL;

-- Measured on a real install with blocking on: a blocked request logs each rule
-- once. The signature rules that fed the score log "Warning." and the denying rule
-- (949110) logs "Access denied", one line each under the same tx_id, so a blocked
-- request is not duplicated in this table. (An earlier version of this comment
-- said the denying rule was logged twice; that was a standalone-rig observation
-- that did not survive Belune's own pipeline.)
--
-- ⛔ There is deliberately NO unique index on (tx_id, rule_id). Rows are inserted
-- with COPY, which fails whole: one unique violation would discard the entire
-- batch of detections (#122 with a different trigger).
