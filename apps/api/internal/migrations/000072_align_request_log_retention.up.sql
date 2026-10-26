-- Request logs are kept as long as WAF detections: 7 days.
--
-- Three places already named a default and only two agreed. 000022 seeded this
-- row at 3, while the sweep's own fallback for a missing row is 7 ("same period
-- as app logs", service/metrics.go) and app_log_retention_days is 7 — so which
-- window an install got depended on whether the seed row existed.
--
-- 7 is load-bearing rather than tidy. A WAF detection is kept for
-- waf.DefaultRetentionDays (7) and carries no request detail of its own: the URL,
-- method and status come from request_logs, joined on the request uuid Caddy puts
-- in the access log. At 3 days the evidence expired four days before the
-- detection it explains, which is most of that detection's life.
--
-- Guarded on the seeded value, so an install whose operator chose a different
-- window keeps it. A deliberate 3 is indistinguishable from the seed and does
-- move; raising a retention window never drops a row, so the cost is disk and the
-- operator can set it back.
UPDATE settings
SET value = '7'
WHERE key = 'request_log_retention_days' AND value = '3';
