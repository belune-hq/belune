-- Process types and instances as data (v0.3.0, Phase A).
--
-- A process type is "a different command run from the same built image", one
-- container per type; an instance is one running container of a type. Phase A
-- only lays the tables and backfills them. It is INVISIBLE by design: nothing
-- restarts, nothing is renamed, `docker ps` is byte-identical.
--
-- Forward-only, no .down.sql: update.sh runs this against live installs and the
-- container has already restarted by the time a down migration could help.
--
-- RULES THAT KEEP THIS FROM ROTTING
--  1. NEVER parse container_name. It is an opaque handle on the row. Anything
--     that needs structure (which type, which index) reads LABELS. The orphan
--     reaper carries two legacy name formats precisely because names were once
--     load-bearing; do not repeat that.
--  2. Process type names are constrained to ^[a-z][a-z0-9-]{0,19}$ here, as a
--     CHECK, rather than only in the service layer: a rule the database holds
--     cannot be skipped by the next code path, and loosening a CHECK later is a
--     forward-only migration like any other.
--
-- NAMES FOR NEW CONTAINERS (written by A2/A4, recorded here so it has one home):
--     {appSlug}-{processType}-{idx}-{deploymentID[:8]}
--     acme-api-a1b2c3d4-web-1-9f8e7d6c
-- appSlug is already {projectSlug}-{baseSlug}-{uuid8}, so grouping and `docker
-- ps` readability survive. The deployment suffix is what lets 0.3.1 roll: old
-- web-1 and new web-1 must coexist while the new one is health-checked. Do NOT
-- offset idx instead (new web-1 becomes web-2) -- after rolling a 2-replica app
-- you would be left holding web-3 and web-4.
-- Known, pre-existing: project.slug and the base slug are each VARCHAR(255), so
-- appSlug can already exceed Docker's 255-character name limit; the suffixes
-- make that easier to hit.
--
-- HEALTH CHECK RESOLUTION (the columns land here, the resolver is Phase B):
--     process type sets a type -> use it
--     else routable            -> inherit the application's
--     else                     -> 'none'
-- The last line is load-bearing: a non-routable type must NEVER inherit 'http',
-- or every worker is permanently unhealthy against a port nothing listens on.
-- HTTP-specific knobs (path, container_port, expect_status) and the Docker timing
-- knobs stay application-level, which is why there are two columns here, not
-- eight. domains needs no change: it keeps binding to application_id and the
-- reconciler resolves application -> routable type -> instances.
--
-- 0.4.0 adds a nullable server_id to application_instances additively; nothing
-- below makes that a rewrite.

CREATE TABLE application_process_types (
    id                   UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    application_id       UUID NOT NULL REFERENCES applications(id) ON DELETE CASCADE,
    name                 TEXT NOT NULL
        CONSTRAINT application_process_types_name_check CHECK (name ~ '^[a-z][a-z0-9-]{0,19}$'),
    -- '' means "the image's default CMD", which is exactly what makes the
    -- backfilled row behave like today's container.
    command              TEXT NOT NULL DEFAULT '',
    -- Fixed at 1 in 0.3.0; the range is the 0.3.1 contract.
    replicas             INT  NOT NULL DEFAULT 1 CHECK (replicas BETWEEN 0 AND 20),
    -- An explicit flag, NOT the magic name 'web' -- someone will call it
    -- 'frontend'. The partial unique index below allows at most one per app.
    routable             BOOLEAN NOT NULL DEFAULT false,
    cpu_limit            DOUBLE PRECISION,   -- NULL = inherit from the application
    memory_limit         BIGINT,             -- NULL = inherit from the application
    health_check_type    TEXT,               -- NULL = resolve by the rule above
    health_check_command TEXT,               -- NULL = inherit from the application
    created_at           TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at           TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE UNIQUE INDEX idx_application_process_types_name
    ON application_process_types(application_id, name);
CREATE UNIQUE INDEX idx_application_process_types_routable
    ON application_process_types(application_id) WHERE routable;

CREATE TABLE application_instances (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    application_id  UUID NOT NULL REFERENCES applications(id) ON DELETE CASCADE,
    process_type_id UUID NOT NULL REFERENCES application_process_types(id) ON DELETE CASCADE,
    -- Nullable BY DESIGN: backfilled rows have no deployment to point at, and a
    -- replaced database arrives on upgrade without one. Do not "fix" to NOT NULL.
    deployment_id   UUID REFERENCES deployments(id) ON DELETE SET NULL,
    -- 1-based (web-1), matching Heroku's web.1. Deliberately NOT unique per type:
    -- during a 0.3.1 roll the old and new web-1 coexist, told apart by deployment.
    idx             INT  NOT NULL CHECK (idx >= 1),
    -- The ACTUAL docker name. Legacy rows keep the old one (applications.slug), so
    -- nothing is renamed and nothing 502s until the app is redeployed. Storing the
    -- fact instead of deriving it is the whole upgrade story.
    container_name  TEXT NOT NULL,
    container_id    TEXT,
    -- Same vocabulary as applications.status (000054) plus 'pending' for "no
    -- container yet", constrained for the reason 000052 gives: a typo in a new
    -- code path would otherwise be stored silently.
    status          TEXT NOT NULL DEFAULT 'pending'
        CONSTRAINT application_instances_status_check
        CHECK (status IN ('pending', 'running', 'unhealthy', 'stopped', 'error')),
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX idx_application_instances_application
    ON application_instances(application_id, idx);
CREATE INDEX idx_application_instances_process_type
    ON application_instances(process_type_id, idx);
CREATE INDEX idx_application_instances_container_id
    ON application_instances(container_id) WHERE container_id IS NOT NULL;

-- BACKFILL. Every application gets one routable 'web' type and one instance
-- carrying its CURRENT, LEGACY container name. naming.ContainerName returns the
-- application slug unchanged ("the DB slug is the full container name"), so no
-- name is constructed here.
--
-- Driven from applications alone -- no join to deployments -- so an app with no
-- deployment, and a stopped one, still get their rows; an inner join would leave
-- them with no upstream and A3's reconciler would 502 them on someone else's
-- install. Databases are a separate resource and get nothing.
--
-- Instance status follows applications.status: running/unhealthy/stopped/error
-- carry over (the container exists in that state), and 'inactive' -- never
-- deployed, no container -- becomes 'pending'.
INSERT INTO application_process_types (application_id, name, command, replicas, routable)
SELECT id, 'web', '', 1, true
FROM applications;

INSERT INTO application_instances (application_id, process_type_id, idx, container_name, status)
SELECT a.id,
       pt.id,
       1,
       a.slug,
       CASE a.status WHEN 'inactive' THEN 'pending' ELSE a.status END
FROM applications a
JOIN application_process_types pt ON pt.application_id = a.id AND pt.name = 'web';
