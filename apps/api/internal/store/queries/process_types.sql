-- ORDER BY idx lives HERE, not in Go: the proxy reconciler hands Caddy an upstream
-- list and EnsureRoute compares it with a whole-JSON equality, so an unordered list
-- is unequal on every pass and the route is removed and re-added for ever. A sort
-- in one caller is a sort the next caller forgets. The id tiebreak keeps the order
-- total while a 0.3.1 roll has two instances sharing an idx.

-- name: CreateProcessType :one
INSERT INTO application_process_types (
    application_id, name, command, replicas, routable,
    cpu_limit, memory_limit, health_check_type, health_check_command
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
RETURNING *;

-- name: GetProcessType :one
SELECT * FROM application_process_types WHERE id = $1;

-- name: GetProcessTypeByName :one
SELECT * FROM application_process_types
WHERE application_id = $1 AND name = $2;

-- name: ListProcessTypesByApplication :many
SELECT * FROM application_process_types
WHERE application_id = $1
ORDER BY routable DESC, name;

-- Full-row update: the handler must Get first and fall back to the stored value
-- for every field the request omits.
-- name: UpdateProcessType :one
UPDATE application_process_types
SET command              = $2,
    replicas             = $3,
    routable             = $4,
    cpu_limit            = $5,
    memory_limit         = $6,
    health_check_type    = $7,
    health_check_command = $8,
    updated_at           = NOW()
WHERE id = $1
RETURNING *;

-- name: DeleteProcessType :exec
DELETE FROM application_process_types WHERE id = $1;

-- name: CreateInstance :one
INSERT INTO application_instances (
    application_id, process_type_id, deployment_id, idx, container_name, container_id, status
) VALUES ($1, $2, $3, $4, $5, $6, $7)
RETURNING *;

-- name: GetInstance :one
SELECT * FROM application_instances WHERE id = $1;

-- name: ListInstancesByApplication :many
SELECT * FROM application_instances
WHERE application_id = $1
ORDER BY idx, id;

-- name: ListInstancesByProcessType :many
SELECT * FROM application_instances
WHERE process_type_id = $1
ORDER BY idx, id;

-- The resolver's read: an application's instances with the type each belongs to.
-- name: ListInstancesWithProcessTypeByApplication :many
SELECT i.id, i.application_id, i.process_type_id, i.deployment_id, i.idx,
       i.container_name, i.container_id, i.status,
       pt.name AS process_type_name, pt.routable AS process_type_routable
FROM application_instances i
JOIN application_process_types pt ON pt.id = i.process_type_id
WHERE i.application_id = $1
ORDER BY pt.routable DESC, pt.name, i.idx, i.id;

-- The reconciler's read: the routable type's instances, i.e. a route's upstreams.
-- name: ListRoutableInstancesByApplication :many
SELECT i.*
FROM application_instances i
JOIN application_process_types pt ON pt.id = i.process_type_id
WHERE i.application_id = $1 AND pt.routable
ORDER BY i.idx, i.id;

-- name: UpdateInstanceStatus :one
UPDATE application_instances
SET status = $2, updated_at = NOW()
WHERE id = $1
RETURNING *;

-- name: UpdateInstanceContainer :one
UPDATE application_instances
SET container_id = $2, status = $3, updated_at = NOW()
WHERE id = $1
RETURNING *;

-- name: DeleteInstance :exec
DELETE FROM application_instances WHERE id = $1;

-- The orphan reaper's claims: every name an instance row says a container may
-- carry, across all applications.
-- name: ListAllInstanceContainerNames :many
SELECT application_id, container_name FROM application_instances;

-- Resolving a Docker event to its instance row (A4). Scoped by application_id so
-- a name or id that collides across applications can never move another app's row.
-- The event watcher tries them in this order: container id, stored container name,
-- then the process-type + instance-idx labels.
-- name: GetInstanceByContainerID :one
SELECT * FROM application_instances
WHERE application_id = $1 AND container_id = $2
ORDER BY idx, id
LIMIT 1;

-- A lookup of a stored fact, never a parse of the name: A1's backfill stored the
-- legacy name every pre-upgrade container already carries.
-- name: GetInstanceByContainerName :one
SELECT * FROM application_instances
WHERE application_id = $1 AND container_name = $2
ORDER BY idx, id
LIMIT 1;

-- name: GetInstanceByProcessTypeAndIdx :one
SELECT i.*
FROM application_instances i
JOIN application_process_types pt ON pt.id = i.process_type_id
WHERE i.application_id = $1 AND pt.name = $2 AND i.idx = $3
ORDER BY i.id
LIMIT 1;
