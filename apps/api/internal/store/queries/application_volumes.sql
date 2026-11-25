-- name: CreateApplicationVolume :one
INSERT INTO application_volumes (application_id, name, mount_path, external_name)
VALUES ($1, $2, $3, $4)
RETURNING *;

-- name: GetApplicationVolume :one
SELECT * FROM application_volumes WHERE id = $1;

-- name: ListApplicationVolumes :many
SELECT * FROM application_volumes
WHERE application_id = $1
ORDER BY mount_path;

-- name: DeleteApplicationVolume :exec
DELETE FROM application_volumes WHERE id = $1;

-- name: ListApplicationVolumeOwners :many
-- Maps each application volume to its owning app so the admin Docker page can
-- link Docker volumes back to the application. Volumes carry no application-id
-- label, but the Docker volume name is reconstructable from (application_id,
-- name) via naming.VolumeNameFor, unless external_name records an adopted name.
SELECT av.application_id, av.name, av.external_name, a.name AS application_name, a.project_id
FROM application_volumes av
JOIN applications a ON a.id = av.application_id;

-- name: RegisterAdoptedVolume :exec
INSERT INTO adopted_volumes (name) VALUES ($1) ON CONFLICT (name) DO NOTHING;

-- name: ListAdoptedVolumes :many
SELECT * FROM adopted_volumes ORDER BY name;

-- name: GetExternalVolumeAttachment :one
SELECT * FROM application_volumes WHERE external_name = $1;

-- name: AcquireVolumeClaimsLock :exec
SELECT pg_advisory_xact_lock(sqlc.arg(lock_id)::bigint);
