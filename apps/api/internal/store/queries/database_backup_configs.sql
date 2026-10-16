-- name: CreateDatabaseBackupConfig :one
INSERT INTO database_backup_configs (
    database_id, destination_id, prefix, schedule, keep_latest, enabled, target_database, timezone
)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
RETURNING *;

-- name: UpdateDatabaseBackupConfig :one
UPDATE database_backup_configs
SET destination_id  = $2,
    prefix          = $3,
    schedule        = $4,
    keep_latest     = $5,
    enabled         = $6,
    target_database = $7,
    timezone        = $8,
    updated_at      = NOW()
WHERE id = $1
RETURNING *;

-- name: GetDatabaseBackupConfig :one
SELECT * FROM database_backup_configs WHERE id = $1;

-- name: ListDatabaseBackupConfigs :many
SELECT * FROM database_backup_configs
WHERE database_id = $1
ORDER BY created_at;

-- name: ListEnabledDatabaseBackupConfigs :many
SELECT * FROM database_backup_configs
WHERE enabled
ORDER BY created_at;

-- name: DeleteDatabaseBackupConfig :exec
DELETE FROM database_backup_configs WHERE id = $1;

-- name: TouchDatabaseBackupConfigRun :exec
UPDATE database_backup_configs
SET last_run_at = $2
WHERE id = $1;
