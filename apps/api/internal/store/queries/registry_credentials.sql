-- name: CreateRegistryCredential :one
INSERT INTO registry_credentials (registry_host, username, password_encrypted)
VALUES ($1, $2, $3) RETURNING *;

-- name: UpdateRegistryCredential :one
UPDATE registry_credentials
SET registry_host = $2, username = $3,
    password_encrypted = COALESCE($4, password_encrypted), updated_at = now()
WHERE id = $1 RETURNING *;

-- name: GetRegistryCredential :one
SELECT * FROM registry_credentials WHERE id = $1;

-- name: GetRegistryCredentialByHost :one
SELECT * FROM registry_credentials WHERE registry_host = $1;

-- name: ListRegistryCredentials :many
-- Reads deliberately omit ciphertext: API callers only need its presence.
SELECT id, registry_host, username, created_at, updated_at
FROM registry_credentials ORDER BY registry_host;

-- name: DeleteRegistryCredential :one
DELETE FROM registry_credentials WHERE id = $1 RETURNING id;

-- name: RegistryCredentialExistsByHost :one
SELECT EXISTS(SELECT 1 FROM registry_credentials WHERE registry_host = $1);
