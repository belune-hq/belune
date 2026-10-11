-- name: ListApplicationsByProject :many
SELECT * FROM applications
WHERE project_id = $1 AND parent_application_id IS NULL
ORDER BY created_at DESC;

-- name: GetApplication :one
SELECT * FROM applications WHERE id = $1;

-- name: GetApplicationWithProjectSlug :one
-- server_id rides along because every container operation on this application
-- has to resolve which host it runs on, and this join is already paid for.
SELECT a.*, p.slug as project_slug, p.server_id as server_id
FROM applications a
JOIN projects p ON p.id = a.project_id
WHERE a.id = $1;

-- name: GetApplicationLogAccess :one
-- Everything get_application_logs (internal/mcpserver) needs in one query:
-- the container-naming/placement fields GetApplicationWithProjectSlug
-- returns, plus the owning project's user_id/shared for authorization —
-- sparing a second round trip through GetApplicationOwnerUserID for the one
-- tool in that package that reaches outside the database, and the most
-- likely to be polled repeatedly. Selects only what's needed rather than
-- `a.*`, so the application's secret-adjacent columns (webhook secret,
-- encrypted git credentials, deploy-hook token) never enter process memory
-- for a call that has no use for them.
SELECT a.id, a.project_id, a.slug, p.slug as project_slug, p.server_id as server_id,
       p.user_id as project_user_id, p.shared as project_shared
FROM applications a
JOIN projects p ON p.id = a.project_id
WHERE a.id = $1;

-- name: CreateApplication :one
-- branch: the ref to build. NULL means the repository's default ref, which is
-- what every application did before branch selection existed.
-- auto_deploy_branch is kept in lockstep with it: one user-facing "Branch"
-- decides both what we build and which pushes deploy, so the two cannot drift.
INSERT INTO applications (project_id, name, slug, type, source_repo, source_image, dockerfile_path, build_type, cpu_limit, memory_limit, webhook_secret_encrypted, git_credentials_encrypted, health_check_path, git_integration_id, branch, auto_deploy_branch, root_directory)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17)
RETURNING *;

-- name: UpdateApplication :one
UPDATE applications SET
    name = $2, source_repo = $3, source_image = $4, dockerfile_path = $5,
    build_type_override = $6, builder_image = $7, custom_buildpacks = $8,
    status = $9, cpu_limit = $10, memory_limit = $11, git_credentials_encrypted = $12,
    health_check_path = $13, git_integration_id = $14,
    branch = $15, auto_deploy_branch = $16, root_directory = $17, updated_at = NOW()
WHERE id = $1
RETURNING *;

-- name: UpdateApplicationStatus :one
UPDATE applications SET status = $2, updated_at = NOW()
WHERE id = $1
RETURNING *;

-- name: DeleteApplication :exec
DELETE FROM applications WHERE id = $1;

-- name: ListApplicationsBySourceRepo :many
-- Either column counts as "has a secret": a row may still be un-backfilled.
SELECT * FROM applications
WHERE source_repo = $1
  AND (webhook_secret_encrypted IS NOT NULL OR webhook_secret IS NOT NULL);

-- name: UpdateApplicationWebhook :one
-- Writing the secret always clears the legacy plaintext column, so a row that
-- has been touched since the encryption migration never carries both.
UPDATE applications SET
    webhook_secret_encrypted = $2,
    webhook_secret = NULL,
    auto_deploy_branch = $3,
    updated_at = NOW()
WHERE id = $1 RETURNING *;

-- name: BackfillWebhookSecret :exec
-- One-time move of a plaintext secret into the encrypted column.
UPDATE applications SET webhook_secret_encrypted = $2, webhook_secret = NULL
WHERE id = $1;

-- name: ListApplicationsWithPlaintextWebhookSecret :many
SELECT * FROM applications
WHERE webhook_secret IS NOT NULL AND webhook_secret <> '';

-- name: SetApplicationDeployHook :one
UPDATE applications SET
    deploy_hook_token_hash = $2,
    deploy_hook_token_encrypted = $3,
    updated_at = NOW()
WHERE id = $1 RETURNING *;

-- name: ClearApplicationDeployHook :one
UPDATE applications SET
    deploy_hook_token_hash = NULL,
    deploy_hook_token_encrypted = NULL,
    updated_at = NOW()
WHERE id = $1 RETURNING *;

-- name: GetApplicationByDeployHookToken :one
SELECT * FROM applications WHERE deploy_hook_token_hash = $1;

-- name: ListAllApplications :many
SELECT * FROM applications;

-- name: UpdateApplicationSlug :exec
UPDATE applications SET slug = $2 WHERE id = $1;

-- name: CountApplications :one
SELECT count(*) FROM applications;

-- name: GetApplicationOwnerUserID :one
-- shared rides along so canAccessApplication can grant every Member access to
-- a shared project's applications, not only its owner.
-- project_id rides along so the caller can compose a project-pinned token's
-- pin, which is a separate axis from ownership — GetDatabaseOwnerUserID has
-- carried it for the WebSocket authorizer since v0.1.12, and REST needed it
-- for the same reason: without it canAccessOwned can only ask who owns the
-- resource, never which project it belongs to.
SELECT p.user_id, p.shared, a.project_id FROM applications a
JOIN projects p ON p.id = a.project_id
WHERE a.id = $1;

-- name: UpdateApplicationPreviewConfig :one
UPDATE applications SET
    preview_branch_pattern = $2,
    preview_domain_template = $3,
    updated_at = NOW()
WHERE id = $1
RETURNING *;

-- name: CreatePreviewApplication :one
INSERT INTO applications (
    project_id, name, slug, type,
    source_repo, source_image, dockerfile_path, build_type,
    cpu_limit, memory_limit, git_credentials_encrypted, health_check_path,
    git_integration_id, parent_application_id, branch, root_directory
)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16)
RETURNING *;

-- name: GetPreviewByParentBranch :one
SELECT * FROM applications
WHERE parent_application_id = $1 AND branch = $2;

-- name: ListPreviewsByParent :many
SELECT * FROM applications
WHERE parent_application_id = $1
ORDER BY last_activity_at DESC;

-- name: ListStalePreviews :many
SELECT * FROM applications
WHERE parent_application_id IS NOT NULL
  AND last_activity_at < $1;

-- name: ListAllApplicationsWithProjectSlug :many
SELECT a.*, p.slug as project_slug, p.server_id as server_id
FROM applications a
JOIN projects p ON p.id = a.project_id;

-- name: ListStalePreviewsWithProjectSlug :many
SELECT a.*, p.slug as project_slug
FROM applications a
JOIN projects p ON p.id = a.project_id
WHERE a.parent_application_id IS NOT NULL
  AND a.last_activity_at < $1;

-- name: TouchApplicationActivity :exec
UPDATE applications SET last_activity_at = NOW()
WHERE id = $1;

-- name: GetDeploymentOwnerInfo :one
-- Owner-only even for a shared project: fanning notifications out to every
-- member is a new feature (alert_preferences is per-user), not something
-- sharing implies. Revisit with real project membership.
SELECT u.id AS user_id, u.email, u.first_name, a.name AS app_name, p.name AS project_name
FROM deployments d
JOIN applications a ON a.id = d.application_id
JOIN projects p ON p.id = a.project_id
JOIN users u ON u.id = p.user_id
WHERE d.id = $1;

-- name: GetProjectOwnerInfo :one
-- Owner-only for the same reason as GetDeploymentOwnerInfo above.
SELECT u.id AS user_id, u.email, u.first_name, p.name AS project_name
FROM projects p
JOIN users u ON u.id = p.user_id
WHERE p.id = $1;

-- name: UpdateApplicationSource :exec
UPDATE applications SET source_kind = $2, source_ref = $3, updated_at = NOW()
WHERE id = $1;

-- name: UpdateApplicationRuntime :exec
UPDATE applications SET readonly_rootfs = $2, container_caps = $3, updated_at = NOW()
WHERE id = $1;

-- name: SetApplicationResources :one
UPDATE applications
SET cpu_limit = $2, memory_limit = $3, updated_at = NOW()
WHERE id = $1
RETURNING *;

-- name: SetApplicationContainerPort :exec
UPDATE applications SET container_port = $2, updated_at = NOW()
WHERE id = $1;

-- name: SetApplicationHealthTuning :exec
UPDATE applications
SET health_check_timeout_seconds = $2, health_check_expect_status = $3, updated_at = NOW()
WHERE id = $1;

-- name: SetApplicationHealthCheck :one
-- Writes the whole health-check configuration as one unit — the type plus both
-- mechanisms' fields — so the stored row is always internally consistent (an
-- http type never carries a stale command, and vice versa; the handler nulls
-- the fields that do not apply to the chosen type).
UPDATE applications SET
    health_check_type                 = $2,
    health_check_path                 = $3,
    health_check_expect_status        = $4,
    health_check_command              = $5,
    health_check_interval_seconds     = $6,
    health_check_retries              = $7,
    health_check_start_period_seconds = $8,
    health_check_timeout_seconds      = $9,
    updated_at = NOW()
WHERE id = $1
RETURNING *;

-- name: TouchApplicationConfigChanged :exec
-- Marks the application as having saved config that the running container does
-- not yet reflect. Idempotent in effect: re-stamping an already-set marker just
-- moves it later, which only makes the clearing condition stricter.
UPDATE applications SET config_changed_at = NOW()
WHERE id = $1;

-- name: TouchApplicationSourceChanged :exec
-- Only config_changed_at's stronger sibling is stamped: the indicator reports
-- the stronger of the two, so a source change need not also set the config
-- marker, and doing so would leave a stale "reload to apply" behind after the
-- deploy cleared only the source marker.
UPDATE applications SET source_changed_at = NOW()
WHERE id = $1;

-- name: ClearApplicationConfigChanged :exec
-- The skip-build path (reload, rollback): the container is recreated from the
-- image already present, so config is applied but the source is not.
-- Only clears a marker that predates the deployment, so an edit made while the
-- deploy was running keeps its marker rather than being silently swallowed.
UPDATE applications SET config_changed_at = NULL
WHERE applications.id = $1
  AND config_changed_at IS NOT NULL
  AND config_changed_at <= (SELECT started_at FROM deployments WHERE deployments.id = $2);

-- name: ClearApplicationSourceChanged :exec
-- The build/pull path additionally clears the source marker; the worker calls
-- this alongside ClearApplicationConfigChanged, since a new image applies both.
-- Same predates-the-deployment guard.
UPDATE applications SET source_changed_at = NULL
WHERE applications.id = $1
  AND source_changed_at IS NOT NULL
  AND source_changed_at <= (SELECT started_at FROM deployments WHERE deployments.id = $2);

-- name: TouchApplicationDeployed :exec
UPDATE applications SET last_deployed_at = NOW()
WHERE id = $1;

-- name: ChangeApplicationSource :one
-- Swaps an application between git and image in one statement, so it is never
-- observable in a half-changed state where type disagrees with the source
-- columns.
--
-- Every field belonging to the abandoned source is written, not left alone:
-- the caller passes NULL for the ones that no longer apply. Leaving a stale
-- source_repo on an image application is exactly the incoherence the source
-- validation now rejects, and it is what let a push webhook match an app it
-- could not build.
--
-- source_changed_at is stamped rather than config_changed_at: the running
-- container is still the pre-switch image, and only a real build or pull can
-- change that.
UPDATE applications SET
    type = $2,
    build_type = $3,
    source_repo = $4,
    source_image = $5,
    dockerfile_path = $6,
    build_type_override = NULL,
    builder_image = NULL,
    branch = $7,
    auto_deploy_branch = $8,
    git_integration_id = $9,
    git_credentials_encrypted = $10,
    webhook_secret_encrypted = $11,
    webhook_secret = NULL,
    root_directory = $12,
    source_changed_at = NOW(),
    updated_at = NOW()
WHERE id = $1
RETURNING *;

-- name: GetServerIDForApplication :one
-- Placement lookup for paths that hold only an application id. Cheaper than
-- refetching the row, and it keeps "which host" an explicit question rather
-- than something a caller assumes.
SELECT p.server_id FROM applications a
JOIN projects p ON p.id = a.project_id
WHERE a.id = $1;
