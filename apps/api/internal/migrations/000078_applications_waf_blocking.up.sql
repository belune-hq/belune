-- Per-application WAF enforcement.
--
-- Until now the WAF only detected. Blocking is opt-in PER APPLICATION, never
-- install-wide: a stock Next.js app trips CRS 942290 (critical, PL1) on every
-- request through its PostHog cookie, so nobody can switch enforcement on across
-- an install that includes one. FALSE is today's behaviour, which is why this is
-- NOT NULL DEFAULT FALSE rather than nullable: there is no third state to mean,
-- and every existing application stays detection-only without a backfill.
--
-- The reconciler promotes the hosts of applications where this is true from the
-- WAF's static DetectionOnly engine to On, per host. A row that is missing,
-- false, or has no domains leaves its hosts detection-only.
ALTER TABLE applications
    ADD COLUMN waf_blocking BOOLEAN NOT NULL DEFAULT FALSE;
