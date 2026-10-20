package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strconv"

	"github.com/hibiken/asynq"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/belune-hq/belune/internal/naming"
	"github.com/belune-hq/belune/internal/runtime"
	"github.com/belune-hq/belune/internal/status"
	"github.com/belune-hq/belune/internal/store/generated"
)

type provisionDBPayload struct {
	DatabaseID string `json:"database_id"`
}

// reconfigureDBPayload toggles SSH-tunnel external access for a database by
// recreating its container with (enable) or without (disable) a loopback host
// port binding.
type reconfigureDBPayload struct {
	DatabaseID string `json:"database_id"`
	Enable     bool   `json:"enable"`
}

// reloadDBPayload recreates a database container from its stored record without
// changing its configuration.
type reloadDBPayload struct {
	DatabaseID string `json:"database_id"`
}

func (h *TaskHandler) HandleProvisionDBTask(ctx context.Context, t *asynq.Task) error {
	var payload provisionDBPayload
	if err := json.Unmarshal(t.Payload(), &payload); err != nil {
		return fmt.Errorf("unmarshal provision_db payload: %w", err)
	}

	slog.Info("handling provision_db task", "database_id", payload.DatabaseID)

	dbID, err := parseUUID(payload.DatabaseID)
	if err != nil {
		return errors.Join(fmt.Errorf("invalid database_id (permanent): %w", err), asynq.SkipRetry)
	}

	db, err := h.Queries.GetDatabase(ctx, dbID)
	if err != nil {
		return fmt.Errorf("get database: %w", err)
	}

	// One resolve per task: every step below acts on the same host.
	rt, err := h.runtimeForDatabase(ctx, dbID)
	if err != nil {
		h.failDatabase(ctx, dbID, fmt.Sprintf("resolve server: %v", err))
		return err
	}

	creds, err := h.decryptDBCredentials(db)
	if err != nil {
		h.failDatabase(ctx, dbID, fmt.Sprintf("credentials: %v", err))
		return fmt.Errorf("credentials: %w", err)
	}

	// New databases start internal-only (no host port); external access is an
	// explicit opt-in handled by the reconfigure task.
	if err := h.provisionDBContainer(ctx, rt, db, creds, 0); err != nil {
		h.failDatabase(ctx, dbID, err.Error())
		return err
	}

	slog.Info("database provisioned", "database_id", payload.DatabaseID, "container", db.Slug, "type", db.Type)
	return nil
}

// HandleReconfigureDBTask recreates a database container to add or remove the
// loopback host-port binding used for SSH-tunnel external access. The named
// volume is reattached, so data survives the recreate (brief downtime).
func (h *TaskHandler) HandleReconfigureDBTask(ctx context.Context, t *asynq.Task) error {
	var payload reconfigureDBPayload
	if err := json.Unmarshal(t.Payload(), &payload); err != nil {
		return fmt.Errorf("unmarshal reconfigure_db payload: %w", err)
	}

	slog.Info("handling reconfigure_db task", "database_id", payload.DatabaseID, "enable", payload.Enable)

	dbID, err := parseUUID(payload.DatabaseID)
	if err != nil {
		return errors.Join(fmt.Errorf("invalid database_id (permanent): %w", err), asynq.SkipRetry)
	}

	db, err := h.Queries.GetDatabase(ctx, dbID)
	if err != nil {
		return fmt.Errorf("get database: %w", err)
	}

	// One resolve per task: every step below acts on the same host.
	rt, err := h.runtimeForDatabase(ctx, dbID)
	if err != nil {
		h.failDatabase(ctx, dbID, fmt.Sprintf("resolve server: %v", err))
		return err
	}

	creds, err := h.decryptDBCredentials(db)
	if err != nil {
		h.failDatabase(ctx, dbID, fmt.Sprintf("credentials: %v", err))
		return fmt.Errorf("credentials: %w", err)
	}

	// Prior host port, for rollback if the new configuration fails to come up.
	// db was read before any change, so this is the pre-toggle value.
	var oldHostPort int32
	if db.HostPort.Valid {
		oldHostPort = db.HostPort.Int32
	}

	// Allocate the loopback port up front so a failure leaves the old container
	// untouched. A fresh free port is taken each time external access is enabled.
	var hostPort int32
	if payload.Enable {
		p, perr := freeLoopbackPort()
		if perr != nil {
			h.failDatabase(ctx, dbID, fmt.Sprintf("allocate host port: %v", perr))
			return fmt.Errorf("allocate host port: %w", perr)
		}
		hostPort = p
	}

	// Remove the existing container (best-effort; the volume persists).
	h.removeDBContainer(ctx, rt, db.Slug)

	if err := h.provisionDBContainer(ctx, rt, db, creds, hostPort); err != nil {
		// A failed external-access toggle must not take the database offline.
		// Roll back to the prior configuration so the database comes back up;
		// the toggle simply doesn't take effect. Only if rollback also fails is
		// the database genuinely broken (and its volume data is still intact).
		slog.Warn("reconfigure failed; rolling back to previous state",
			"database_id", payload.DatabaseID, "error", err)
		h.removeDBContainer(ctx, rt, db.Slug)
		if rbErr := h.provisionDBContainer(ctx, rt, db, creds, oldHostPort); rbErr != nil {
			h.failDatabase(ctx, dbID, fmt.Sprintf("reconfigure failed and rollback failed: %v", rbErr))
			return fmt.Errorf("reconfigure rollback failed: %w", rbErr)
		}
		slog.Warn("reconfigure rolled back; database restored to previous state",
			"database_id", payload.DatabaseID)
		// Returning nil: the database is healthy in its prior state, so there is
		// nothing to retry. The unchanged external_access state signals to the
		// UI that the toggle did not take.
		return nil
	}

	slog.Info("database reconfigured", "database_id", payload.DatabaseID, "external_access", payload.Enable, "host_port", hostPort)
	return nil
}

// HandleReloadDBTask recreates a database container from its stored record,
// reattaching the data volume and preserving the current external-access
// (loopback host-port) binding. It is the recovery path when the container has
// drifted from the record or been removed entirely: removeDBContainer is
// best-effort (a missing container is not an error), and provisionDBContainer
// recreates the container and stamps the row running on success.
func (h *TaskHandler) HandleReloadDBTask(ctx context.Context, t *asynq.Task) error {
	var payload reloadDBPayload
	if err := json.Unmarshal(t.Payload(), &payload); err != nil {
		return fmt.Errorf("unmarshal reload_db payload: %w", err)
	}

	slog.Info("handling reload_db task", "database_id", payload.DatabaseID)

	dbID, err := parseUUID(payload.DatabaseID)
	if err != nil {
		return errors.Join(fmt.Errorf("invalid database_id (permanent): %w", err), asynq.SkipRetry)
	}

	db, err := h.Queries.GetDatabase(ctx, dbID)
	if err != nil {
		return fmt.Errorf("get database: %w", err)
	}

	// One resolve per task: every step below acts on the same host.
	rt, err := h.runtimeForDatabase(ctx, dbID)
	if err != nil {
		h.failDatabase(ctx, dbID, fmt.Sprintf("resolve server: %v", err))
		return err
	}

	creds, err := h.decryptDBCredentials(db)
	if err != nil {
		h.failDatabase(ctx, dbID, fmt.Sprintf("credentials: %v", err))
		return fmt.Errorf("credentials: %w", err)
	}

	// Preserve the current external-access binding: reload recreates the same
	// configuration, it does not toggle the loopback host port.
	var hostPort int32
	if db.HostPort.Valid {
		hostPort = db.HostPort.Int32
	}

	// Remove any existing/drifted container first (the volume persists), then
	// recreate. provisionDBContainer also removes by name, so a container that is
	// already gone just falls through to a fresh create.
	h.removeDBContainer(ctx, rt, db.Slug)

	if err := h.provisionDBContainer(ctx, rt, db, creds, hostPort); err != nil {
		h.failDatabase(ctx, dbID, err.Error())
		return err
	}

	slog.Info("database reloaded", "database_id", payload.DatabaseID, "container", db.Slug)
	return nil
}

func (h *TaskHandler) decryptDBCredentials(db generated.Database) (map[string]string, error) {
	credsJSON, err := h.Keyring.Decrypt(db.CredentialsEncrypted)
	if err != nil {
		return nil, fmt.Errorf("decrypt: %w", err)
	}
	var creds map[string]string
	if err := json.Unmarshal(credsJSON, &creds); err != nil {
		return nil, fmt.Errorf("unmarshal: %w", err)
	}
	return creds, nil
}

// postgresDataDir returns the volume mount target for a Postgres image tag.
//
// Postgres 18+ Docker images store data in a major-version-specific subdirectory
// and reject a volume mounted directly at /var/lib/postgresql/data (it boots into
// a fresh PGDATA and errors that the legacy path is an "unused mount/volume").
// For 18+ we mount the parent /var/lib/postgresql so the image's default
// /var/lib/postgresql/<major>/docker lands inside the volume; older tags keep the
// historical /var/lib/postgresql/data so existing volumes are unaffected.
// See https://github.com/docker-library/postgres/pull/1259.
func postgresDataDir(version string) string {
	end := 0
	for end < len(version) && version[end] >= '0' && version[end] <= '9' {
		end++
	}
	// Numeric tag ("18", "18.1", "18-alpine"): use the new layout from 18 up.
	if major, err := strconv.Atoi(version[:end]); err == nil {
		if major >= 18 {
			return "/var/lib/postgresql"
		}
		return "/var/lib/postgresql/data"
	}
	// Non-numeric tag ("latest", "alpine", "bookworm"): assume a current (18+)
	// image and use the new layout.
	return "/var/lib/postgresql"
}

// dbContainerSpec returns the per-type image, data dir, container port, env, and
// command for a managed database.
func dbContainerSpec(dbType, version string, creds map[string]string) (image, dataDir string, port int32, env map[string]string, cmd []string, err error) {
	switch dbType {
	case "postgres":
		return fmt.Sprintf("postgres:%s", version), postgresDataDir(version), 5432, map[string]string{
			"POSTGRES_USER":     creds["user"],
			"POSTGRES_PASSWORD": creds["password"],
			"POSTGRES_DB":       creds["database"],
		}, nil, nil
	case "mysql":
		return fmt.Sprintf("mysql:%s", version), "/var/lib/mysql", 3306, map[string]string{
			"MYSQL_ROOT_PASSWORD": creds["root_password"],
			"MYSQL_DATABASE":      creds["database"],
			"MYSQL_USER":          creds["user"],
			"MYSQL_PASSWORD":      creds["password"],
		}, nil, nil
	case "redis":
		return fmt.Sprintf("redis:%s", version), "/data", 6379, map[string]string{},
			[]string{"redis-server", "--requirepass", creds["password"]}, nil
	case "mongo":
		return fmt.Sprintf("mongo:%s", version), "/data/db", 27017, map[string]string{
			"MONGO_INITDB_ROOT_USERNAME": creds["username"],
			"MONGO_INITDB_ROOT_PASSWORD": creds["password"],
		}, nil, nil
	default:
		return "", "", 0, nil, nil, fmt.Errorf("unsupported database type: %s", dbType)
	}
}

// provisionDBContainer (re)creates and starts a database's container, optionally
// binding the database port to 127.0.0.1:hostPort (hostPort > 0). It is
// idempotent on the network/volume/image so it can be used for both first-time
// provisioning and reconfigure recreates. On success the database row is stamped
// running with its connection info and host port.
func (h *TaskHandler) provisionDBContainer(ctx context.Context, rt runtime.ContainerRuntime, db generated.Database, creds map[string]string, hostPort int32) error {
	containerName := db.Slug
	volumeName := fmt.Sprintf("%s-vol", db.Slug)

	var image, dataDir string
	var port int32
	var env map[string]string
	var cmd []string
	if db.Type == "other" {
		// "other": image/port/data-dir come from the user; the credentials map is
		// passed verbatim as container env. No per-engine command.
		image = db.Image.String
		dataDir = db.DataDir.String
		port = db.ContainerPort.Int32
		env = creds
	} else {
		var err error
		image, dataDir, port, env, cmd, err = dbContainerSpec(db.Type, db.Version, creds)
		if err != nil {
			return err
		}
	}

	project, err := h.Queries.GetProject(ctx, db.ProjectID)
	if err != nil {
		return fmt.Errorf("get project for network setup: %w", err)
	}
	projectNetwork := naming.ProjectNetworkName(project.Slug)
	if netErr := rt.CreateNetwork(ctx, projectNetwork); netErr != nil {
		slog.Debug("could not create project network (may already exist)", "network", projectNetwork, "error", netErr)
	}

	// Pin the image to a resolved @sha256 digest so recreates (reconfigure,
	// restart, reload) reuse the exact image instead of silently following a
	// moved mutable tag (e.g. postgres:18 -> 18.1). If already pinned, deploy the
	// digest directly; otherwise pull the tag, resolve its digest, and persist
	// the pin. Upgrade clears the pin so the new target tag is re-resolved.
	pinned := db.ImageDigest.Valid && db.ImageDigest.String != ""
	if pinned {
		image = db.ImageDigest.String
	}
	if err := rt.PullImage(ctx, image); err != nil {
		return fmt.Errorf("pull image: %w", err)
	}
	if !pinned {
		if digest, derr := rt.ResolveImageDigest(ctx, image); derr != nil {
			slog.Warn("could not resolve database image digest; using tag", "image", image, "error", derr)
		} else if digest != "" {
			if uderr := h.Queries.UpdateDatabaseImageDigest(ctx, generated.UpdateDatabaseImageDigestParams{
				ID:          db.ID,
				ImageDigest: pgtype.Text{String: digest, Valid: true},
			}); uderr != nil {
				slog.Warn("could not persist database image digest", "database_id", db.ID.String(), "error", uderr)
			}
			image = digest
		}
	}
	// Label as persistent data so PruneVolumes never reaps the database's data
	// while its container is momentarily absent (stop/reconfigure/recreate).
	if err := rt.CreateDataVolume(ctx, volumeName); err != nil {
		return fmt.Errorf("create volume: %w", err)
	}

	ports := map[string]string{}
	hostBindIP := ""
	hostPortCol := pgtype.Int4{}
	if hostPort > 0 {
		// Loopback only — reachable from the host (e.g. an SSH tunnel), never
		// the public internet.
		ports[fmt.Sprintf("%d", hostPort)] = fmt.Sprintf("%d", port)
		hostBindIP = "127.0.0.1"
		hostPortCol = pgtype.Int4{Int32: hostPort, Valid: true}
	}

	// Remove any leftover container with this name first, so provisioning is
	// idempotent: an asynq retry after a partial failure (or a reconfigure/
	// upgrade recreate) won't fail with "name in use".
	h.removeDBContainer(ctx, rt, containerName)

	containerID, err := rt.CreateContainer(ctx, runtime.ContainerConfig{
		Name:        containerName,
		Image:       image,
		Env:         env,
		Cmd:         cmd,
		Ports:       ports,
		HostBindIP:  hostBindIP,
		Volumes:     map[string]string{volumeName: dataDir},
		Network:     projectNetwork,
		CPULimit:    db.CpuLimit,
		MemoryLimit: db.MemoryLimit,
		// Lets the event watcher map Docker container events back to this
		// database so its status reflects actual container state.
		Labels: map[string]string{runtime.LabelDatabaseID: db.ID.String()},
	})
	if err != nil {
		return fmt.Errorf("create container: %w", err)
	}

	if err := rt.StartContainer(ctx, containerID); err != nil {
		return fmt.Errorf("start container: %w", err)
	}

	h.Queries.UpdateDatabaseAfterProvision(ctx, generated.UpdateDatabaseAfterProvisionParams{
		ID:           db.ID,
		Status:       status.DatabaseRunning,
		InternalHost: pgtype.Text{String: containerName, Valid: true},
		InternalPort: pgtype.Int4{Int32: port, Valid: true},
		HostPort:     hostPortCol,
	})
	return nil
}

// removeDBContainer stops and removes a database container by name. Both steps
// are best-effort: a missing/stopped container is not an error for the caller.
func (h *TaskHandler) removeDBContainer(ctx context.Context, rt runtime.ContainerRuntime, name string) {
	if err := rt.StopContainer(ctx, name); err != nil {
		slog.Debug("reconfigure: stop container (may already be stopped)", "container", name, "error", err)
	}
	if err := rt.RemoveContainer(ctx, name); err != nil {
		slog.Debug("reconfigure: remove container (may already be gone)", "container", name, "error", err)
	}
}

// freeLoopbackPort asks the OS for a free TCP port on the loopback interface.
// The listener is closed immediately; the small window before Docker binds it is
// acceptable for a single-host deployment (a collision just fails the recreate,
// which can be retried).
func freeLoopbackPort() (int32, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return int32(l.Addr().(*net.TCPAddr).Port), nil
}

func (h *TaskHandler) failDatabase(ctx context.Context, dbID pgtype.UUID, errMsg string) {
	slog.Error("database task failed", "database_id", fmt.Sprintf("%v", dbID), "error", errMsg)
	h.Queries.UpdateDatabaseStatus(ctx, generated.UpdateDatabaseStatusParams{
		ID:     dbID,
		Status: status.DatabaseFailed,
	})
}
