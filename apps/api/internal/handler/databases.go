package handler

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/hibiken/asynq"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/weiliang79/belune/internal/naming"
	"github.com/weiliang79/belune/internal/runtime"
	"github.com/weiliang79/belune/internal/service"
	"github.com/weiliang79/belune/internal/status"
	"github.com/weiliang79/belune/internal/store"
	"github.com/weiliang79/belune/internal/store/generated"
	"github.com/weiliang79/belune/internal/worker"
)

var defaultVersions = map[string]string{
	"postgres": "16",
	"mysql":    "8",
	"redis":    "7",
	"mongo":    "7",
}

type createDatabaseCredentials struct {
	User         string `json:"user"`
	Password     string `json:"password"`
	DatabaseName string `json:"database_name"`
	RootPassword string `json:"root_password"`
}

type createDatabaseRequest struct {
	Name        string                     `json:"name"`
	Slug        string                     `json:"slug"`
	Type        string                     `json:"type"`
	Version     string                     `json:"version"`
	Credentials *createDatabaseCredentials `json:"credentials"`

	// "other" type only: an arbitrary container image run as a managed database.
	Image          string            `json:"image"`
	ContainerPort  int32             `json:"container_port"`
	DataDir        string            `json:"data_dir"`
	Env            map[string]string `json:"env"`             // passed verbatim as container env
	BackupMode     string            `json:"backup_mode"`     // volume_snapshot | command
	BackupCommand  string            `json:"backup_command"`  // command mode: dump into $BELUNE_BACKUP_DIR
	RestoreCommand string            `json:"restore_command"` // command mode: restore from $BELUNE_BACKUP_DIR
}

// mysql has no entry: unlike the others, root isn't a safe default user (see
// createDatabaseRecord) — its default is derived from the slug instead.
var defaultUsers = map[string]string{
	"postgres": "postgres",
	"redis":    "default",
	"mongo":    "admin",
}

type provisionDBPayload struct {
	DatabaseID string `json:"database_id"`
}

type volumeInfo struct {
	Name      string `json:"name"`
	SizeBytes int64  `json:"size_bytes"`
}

// externalAccessInfo describes the SSH-tunnel loopback exposure. Enabled means a
// host port is bound on 127.0.0.1; the frontend builds the ssh -L command and a
// localhost connection string from these fields.
type externalAccessInfo struct {
	Enabled  bool   `json:"enabled"`
	HostPort int32  `json:"host_port,omitempty"`
	SSHHost  string `json:"ssh_host,omitempty"`
	SSHUser  string `json:"ssh_user,omitempty"`
}

type databaseResponse struct {
	generated.Database
	Volume         *volumeInfo         `json:"volume,omitempty"`
	ExternalAccess *externalAccessInfo `json:"external_access,omitempty"`
	// ContainerMissing is true when the managed container has been removed out
	// from under us (deleted on the host) while the record still exists — the
	// case Restart/Start can't recover from. The UI surfaces Reload to recreate
	// it. Only computed in the steady non-running states; see GetDatabase.
	ContainerMissing bool `json:"container_missing"`
}

// databaseCredentialsResponse is intentionally its own type, not a field on
// databaseResponse: connection credentials are a live, directly usable
// secret — the plaintext password IS the exploit, not a pointer to one — so
// they get the same RequireSession gate and audit trail as the other
// decrypt-and-return endpoints, split into their own route instead of riding
// along on GetDatabase's plain "read" scope. See RevealDatabaseCredentials.
type databaseCredentialsResponse struct {
	Credentials      map[string]string `json:"credentials,omitempty"`
	ConnectionString string            `json:"connection_string,omitempty"`
}

//apidoc:tag databases
//apidoc:title Create Database
//apidoc:order 3
func (h *Handler) CreateDatabase(w http.ResponseWriter, r *http.Request) {
	projectID := chi.URLParam(r, "projectId")
	var projectUUID pgtype.UUID
	if err := projectUUID.Scan(projectID); err != nil {
		writeError(w, http.StatusBadRequest, "invalid project id")
		return
	}

	if !h.canAccessProject(r, projectUUID) {
		writeError(w, http.StatusForbidden, "access denied")
		return
	}

	var req createDatabaseRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	if req.Name == "" || req.Type == "" {
		writeError(w, http.StatusBadRequest, "name and type are required")
		return
	}

	// Validate type
	isOther := req.Type == "other"
	if !isOther {
		if _, ok := defaultVersions[req.Type]; !ok {
			writeError(w, http.StatusBadRequest, "type must be one of: postgres, mysql, redis, mongo, other")
			return
		}
		// Apply default version if empty.
		if req.Version == "" {
			req.Version = defaultVersions[req.Type]
		}
	} else {
		// "other": the user supplies the image, port, data dir, and backup mode.
		if req.Image == "" || req.ContainerPort <= 0 {
			writeError(w, http.StatusBadRequest, "image and container_port are required for 'other' databases")
			return
		}
		if req.DataDir == "" {
			req.DataDir = "/data"
		}
		// Guard the volume mount path: a restore (volume_snapshot) wipes the
		// data dir, so an absolute, non-root path is required to avoid wiping
		// unintended container paths.
		if !strings.HasPrefix(req.DataDir, "/") || req.DataDir == "/" {
			writeError(w, http.StatusBadRequest, "data_dir must be an absolute path and not the container root")
			return
		}
		if req.BackupMode == "" {
			req.BackupMode = "volume_snapshot"
		}
		if req.BackupMode != "volume_snapshot" && req.BackupMode != "command" {
			writeError(w, http.StatusBadRequest, "backup_mode must be volume_snapshot or command")
			return
		}
		if req.BackupMode == "command" && (req.BackupCommand == "" || req.RestoreCommand == "") {
			writeError(w, http.StatusBadRequest, "backup_command and restore_command are required for command backup mode")
			return
		}
	}

	// The mysql image's entrypoint rejects MYSQL_USER="root" outright (root is
	// configured exclusively via root_password); catch it here as a 400 instead
	// of letting it surface later as a confusing provisioning failure.
	if req.Type == "mysql" && req.Credentials != nil && strings.EqualFold(req.Credentials.User, "root") {
		writeError(w, http.StatusBadRequest, "user cannot be \"root\" for MySQL — set root_password to configure the root account instead")
		return
	}

	// Fetch project for slug
	project, err := h.queries.GetProject(r.Context(), projectUUID)
	if err != nil {
		writeError(w, http.StatusNotFound, "project not found")
		return
	}

	db, _, err := h.createDatabaseRecord(r.Context(), project, req)
	if err != nil {
		slog.Error("failed to create database", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to create database")
		return
	}

	h.audit(r, "create_database", "database", uuidToString(db.ID), map[string]any{"name": req.Name, "type": req.Type})

	writeJSON(w, http.StatusAccepted, db)
}

// createDatabaseRecord builds credentials, inserts the database row with its
// finalized slug, and enqueues provisioning. It is the shared core of the
// CreateDatabase HTTP handler and the template instantiation engine: the caller
// is responsible for authz, request validation, and applying type defaults. It
// returns the created row and the plaintext credentials map (the latter lets the
// template engine resolve {{db.*}} placeholders without a decrypt round-trip).
func (h *Handler) createDatabaseRecord(ctx context.Context, project generated.Project, req createDatabaseRequest) (generated.Database, map[string]string, error) {
	isOther := req.Type == "other"

	baseSlug := naming.Slugify(req.Name)
	if req.Slug != "" {
		baseSlug = naming.Slugify(req.Slug)
	}

	// Build credentials. For known engines these are well-known keys consumed by
	// the per-engine env in provisioning; for "other" the map is passed verbatim
	// as container env (so it doubles as the env/credentials the image needs).
	creds := make(map[string]string)
	if isOther {
		for k, v := range req.Env {
			creds[k] = v
		}
	} else {
		// Generate random password
		passwordBytes := make([]byte, 16)
		if _, err := rand.Read(passwordBytes); err != nil {
			return generated.Database{}, nil, fmt.Errorf("generate credentials: %w", err)
		}
		password := hex.EncodeToString(passwordBytes)

		// Determine effective credential values
		user := defaultUsers[req.Type]
		dbName := req.Name
		if req.Type == "mysql" {
			// root isn't a real app-scoped user, and Belune's own backup/restore/
			// health-check tooling authenticates as root via root_password (see
			// backup_db_task.go) — the app gets a real, scoped user instead,
			// named after the slug (a clean identifier, unlike the display name).
			user = baseSlug
			dbName = baseSlug
		}
		if req.Credentials != nil {
			if req.Credentials.User != "" {
				user = req.Credentials.User
			}
			if req.Credentials.Password != "" {
				password = req.Credentials.Password
			}
			if req.Credentials.DatabaseName != "" {
				dbName = req.Credentials.DatabaseName
			}
		}

		switch req.Type {
		case "postgres":
			creds["user"] = user
			creds["password"] = password
			creds["database"] = dbName
		case "mysql":
			rootPassword := ""
			if req.Credentials != nil && req.Credentials.RootPassword != "" {
				rootPassword = req.Credentials.RootPassword
			} else {
				rootPasswordBytes := make([]byte, 16)
				if _, err := rand.Read(rootPasswordBytes); err != nil {
					return generated.Database{}, nil, fmt.Errorf("generate root password: %w", err)
				}
				rootPassword = hex.EncodeToString(rootPasswordBytes)
			}
			creds["root_password"] = rootPassword
			creds["user"] = user
			creds["password"] = password
			creds["database"] = dbName
		case "redis":
			creds["password"] = password
		case "mongo":
			creds["username"] = user
			creds["password"] = password
		}
	}

	credsJSON, err := json.Marshal(creds)
	if err != nil {
		return generated.Database{}, nil, fmt.Errorf("marshal credentials: %w", err)
	}

	encrypted, err := h.cfg.Keyring.Encrypt(credsJSON)
	if err != nil {
		return generated.Database{}, nil, fmt.Errorf("encrypt credentials: %w", err)
	}

	// "other"-type provisioning columns (NULL/none for known engines).
	imageCol := pgtype.Text{}
	portCol := pgtype.Int4{}
	dataDirCol := pgtype.Text{}
	backupCmdCol := pgtype.Text{}
	restoreCmdCol := pgtype.Text{}
	backupMode := "none"
	if isOther {
		imageCol = pgtype.Text{String: req.Image, Valid: true}
		portCol = pgtype.Int4{Int32: req.ContainerPort, Valid: true}
		dataDirCol = pgtype.Text{String: req.DataDir, Valid: true}
		backupMode = req.BackupMode
		if req.BackupMode == "command" {
			backupCmdCol = pgtype.Text{String: req.BackupCommand, Valid: true}
			restoreCmdCol = pgtype.Text{String: req.RestoreCommand, Valid: true}
		}
	}

	var db generated.Database
	if err := store.WithTx(ctx, h.db, func(q *generated.Queries) error {
		var err error
		db, err = q.CreateDatabase(ctx, generated.CreateDatabaseParams{
			ProjectID:            project.ID,
			Type:                 req.Type,
			Name:                 req.Name,
			Slug:                 baseSlug,
			Version:              req.Version,
			Status:               status.DatabaseCreating,
			InternalHost:         pgtype.Text{},
			InternalPort:         pgtype.Int4{},
			CredentialsEncrypted: encrypted,
			Image:                imageCol,
			ContainerPort:        portCol,
			DataDir:              dataDirCol,
			BackupMode:           backupMode,
			BackupCommand:        backupCmdCol,
			RestoreCommand:       restoreCmdCol,
		})
		if err != nil {
			return err
		}
		// Construct final slug: {projectSlug}-{baseSlug}-{shortId}
		dbIDStr := fmt.Sprintf("%x-%x-%x-%x-%x",
			db.ID.Bytes[0:4], db.ID.Bytes[4:6], db.ID.Bytes[6:8], db.ID.Bytes[8:10], db.ID.Bytes[10:16])
		finalSlug := fmt.Sprintf("%s-%s-%s", project.Slug, baseSlug, dbIDStr[:8])
		if err := q.UpdateDatabaseSlug(ctx, generated.UpdateDatabaseSlugParams{
			ID:   db.ID,
			Slug: finalSlug,
		}); err != nil {
			return err
		}
		db.Slug = finalSlug
		return nil
	}); err != nil {
		return generated.Database{}, nil, err
	}

	// Enqueue provision task
	dbIDStr := fmt.Sprintf("%x-%x-%x-%x-%x",
		db.ID.Bytes[0:4], db.ID.Bytes[4:6], db.ID.Bytes[6:8], db.ID.Bytes[8:10], db.ID.Bytes[10:16])
	payload, err := json.Marshal(provisionDBPayload{DatabaseID: dbIDStr})
	if err != nil {
		return generated.Database{}, nil, fmt.Errorf("marshal provision payload: %w", err)
	}
	task := asynq.NewTask("provision_db", payload)
	if _, err := h.asynq.Enqueue(task, asynq.Queue("critical")); err != nil {
		return generated.Database{}, nil, fmt.Errorf("enqueue provision task: %w", err)
	}

	return db, creds, nil
}

//apidoc:tag databases
//apidoc:title Get Databases
//apidoc:order 1
func (h *Handler) ListDatabases(w http.ResponseWriter, r *http.Request) {
	projectID := chi.URLParam(r, "projectId")
	var projectUUID pgtype.UUID
	if err := projectUUID.Scan(projectID); err != nil {
		writeError(w, http.StatusBadRequest, "invalid project id")
		return
	}

	if !h.canAccessProject(r, projectUUID) {
		writeError(w, http.StatusForbidden, "access denied")
		return
	}

	databases, err := h.queries.ListDatabasesByProject(r.Context(), projectUUID)
	if err != nil {
		writeServerError(w, r, "failed to list databases", err)
		return
	}

	// Surface container_missing so the project services table can flag a database
	// whose container was deleted (the "Reload Needed" badge). Same gating as
	// GetDatabase: only the steady non-running states are inspected, so a table of
	// running databases costs zero Docker calls.
	// Every database in a project shares the project's host, so this resolves
	// once for the whole list rather than once per row — and only if some row
	// actually needs inspecting, so a table of running databases still costs
	// nothing at all.
	var rt runtime.ContainerRuntime
	for _, db := range databases {
		if db.Status == status.DatabaseStopped || db.Status == status.DatabaseFailed {
			resolved, err := h.runtimeForProject(r.Context(), projectUUID)
			if err != nil {
				slog.Warn("list databases: could not reach the project's server", "project_id", projectID, "error", err)
			}
			rt = resolved
			break
		}
	}

	resp := make([]databaseResponse, 0, len(databases))
	for _, db := range databases {
		item := databaseResponse{Database: db}
		if rt != nil && (db.Status == status.DatabaseStopped || db.Status == status.DatabaseFailed) {
			if exists, err := rt.ContainerExists(r.Context(), db.Slug); err == nil {
				item.ContainerMissing = !exists
			}
		}
		resp = append(resp, item)
	}

	writeJSON(w, http.StatusOK, resp)
}

//apidoc:tag databases
//apidoc:title Get Database
//apidoc:order 2
func (h *Handler) GetDatabase(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "databaseId")
	var uuid pgtype.UUID
	if err := uuid.Scan(id); err != nil {
		writeError(w, http.StatusBadRequest, "invalid database id")
		return
	}

	if !h.canAccessDatabase(r, uuid) {
		writeError(w, http.StatusForbidden, "access denied")
		return
	}

	db, err := h.queries.GetDatabase(r.Context(), uuid)
	if err != nil {
		writeError(w, http.StatusNotFound, "database not found")
		return
	}

	resp := databaseResponse{Database: db}

	// Credentials are deliberately NOT decrypted here — this endpoint sits
	// behind plain "read" scope, and a connection password is a live,
	// directly usable secret. See RevealDatabaseCredentials for the
	// session-gated endpoint that returns it.

	// Volume size is deliberately NOT fetched here. VolumeSize funnels into
	// Docker's `system df -v`, which stats every volume on the host — instant on a
	// quiet box, tens of seconds on a busy one — and made this whole page hang on
	// its loading skeleton. The frontend fetches it lazily from GetDatabaseVolume
	// with its own spinner. resp.Volume stays nil.

	// External-access (SSH tunnel) state — enabled when a loopback host port is
	// bound. SSH host/user are presentation-only hints. When SERVER_SSH_HOST is
	// unset, fall back to the resolved server IP so the tunnel command shows a real
	// address instead of a "<server-host>" placeholder (SSH usually lands on the
	// same box; an operator with a separate bastion sets SERVER_SSH_HOST).
	sshHost := h.cfg.ServerSSHHost
	if sshHost == "" {
		if ip, _ := h.effectiveServerIP(r.Context()); ip != "" {
			sshHost = ip
		}
	}
	resp.ExternalAccess = &externalAccessInfo{
		Enabled:  db.HostPort.Valid,
		HostPort: db.HostPort.Int32,
		SSHHost:  sshHost,
		SSHUser:  h.cfg.ServerSSHUser,
	}

	// Flag a genuinely-removed container so the UI can offer Reload to recreate
	// it. Only meaningful in the steady non-running states: during creating/
	// upgrading/backing_up the container is legitimately absent mid-task, and a
	// running database's container is present by definition. A stopped container
	// is still present, so it does not trip this — only a deleted one does.
	if db.Status == status.DatabaseStopped || db.Status == status.DatabaseFailed {
		if rt, err := h.runtimeForDatabase(r.Context(), uuid); err == nil {
			if exists, err := rt.ContainerExists(r.Context(), db.Slug); err == nil {
				resp.ContainerMissing = !exists
			}
		}
	}

	writeJSON(w, http.StatusOK, resp)
}

// RevealDatabaseCredentials decrypts and returns the database's live
// connection credentials — split out of GetDatabase (which sits behind
// plain "read" scope) because this is the same class of endpoint the
// RequireSession reveal boundary protects: a decrypted secret returned in
// plaintext, not a masked/derived value. Ends in "/reveal" on purpose so
// reveal_boundary_test.go's structural sweep discovers it automatically.
// GET /api/projects/{projectId}/databases/{databaseId}/credentials/reveal
//
//apidoc:tag databases
func (h *Handler) RevealDatabaseCredentials(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "databaseId")
	var uuid pgtype.UUID
	if err := uuid.Scan(id); err != nil {
		writeError(w, http.StatusBadRequest, "invalid database id")
		return
	}

	if !h.canAccessDatabase(r, uuid) {
		writeError(w, http.StatusForbidden, "access denied")
		return
	}

	db, err := h.queries.GetDatabase(r.Context(), uuid)
	if err != nil {
		writeError(w, http.StatusNotFound, "database not found")
		return
	}

	var resp databaseCredentialsResponse
	if len(db.CredentialsEncrypted) > 0 {
		credsJSON, err := h.cfg.Keyring.Decrypt(db.CredentialsEncrypted)
		if err != nil {
			writeServerError(w, r, "failed to decrypt credentials", err)
			return
		}
		var creds map[string]string
		if err := json.Unmarshal(credsJSON, &creds); err != nil {
			writeServerError(w, r, "failed to decrypt credentials", err)
			return
		}
		resp.Credentials = creds
		resp.ConnectionString = buildConnectionString(db, creds)
	}

	h.audit(r, "reveal_database_credentials", "database", id, nil)

	writeJSON(w, http.StatusOK, resp)
}

type databaseVolumeResponse struct {
	Name string `json:"name"`
	// SizeBytes is null when the size could not be computed in time (a busy host)
	// — the client shows the name and a soft "unavailable" rather than hanging.
	SizeBytes *int64 `json:"size_bytes"`
}

// GetDatabaseVolume returns the managed volume's name and on-disk size. Split out
// of GetDatabase because the size query (`docker system df -v`) can take tens of
// seconds on a busy host; here it has its own request, spinner, and timeout.
// GET /api/projects/{projectId}/databases/{databaseId}/volume
//
//apidoc:tag databases/volumes-backups
//apidoc:title Get Volume
//apidoc:order 1
func (h *Handler) GetDatabaseVolume(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "databaseId")
	var uuid pgtype.UUID
	if err := uuid.Scan(id); err != nil {
		writeError(w, http.StatusBadRequest, "invalid database id")
		return
	}
	if !h.canAccessDatabase(r, uuid) {
		writeError(w, http.StatusForbidden, "access denied")
		return
	}

	db, err := h.queries.GetDatabase(r.Context(), uuid)
	if err != nil {
		writeError(w, http.StatusNotFound, "database not found")
		return
	}

	resp := databaseVolumeResponse{Name: db.Slug + "-vol"}

	// Bound the host-wide disk scan so a thrashing box returns "unavailable"
	// instead of holding the request open. The error is intentionally swallowed:
	// a missing size is a soft state, not a failure.
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	if rt, err := h.runtimeForDatabase(ctx, uuid); err == nil {
		if size, err := rt.VolumeSize(ctx, resp.Name); err == nil {
			resp.SizeBytes = &size
		}
	}

	writeJSON(w, http.StatusOK, resp)
}

type externalAccessRequest struct {
	Enabled bool `json:"enabled"`
}

// SetDatabaseExternalAccess enables or disables SSH-tunnel external access by
// recreating the database container with/without a loopback host-port binding.
// The recreate runs as an async task; the database is marked transitional
// (creating) so the UI reflects the brief downtime.
//
//apidoc:tag databases
//apidoc:title Set External Access
//apidoc:order 7
func (h *Handler) SetDatabaseExternalAccess(w http.ResponseWriter, r *http.Request) {
	databaseID := chi.URLParam(r, "databaseId")
	var dbUUID pgtype.UUID
	if err := dbUUID.Scan(databaseID); err != nil {
		writeError(w, http.StatusBadRequest, "invalid database id")
		return
	}

	if !h.canAccessDatabase(r, dbUUID) {
		writeError(w, http.StatusForbidden, "access denied")
		return
	}

	var req externalAccessRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	db, err := h.queries.GetDatabase(r.Context(), dbUUID)
	if err != nil {
		writeError(w, http.StatusNotFound, "database not found")
		return
	}
	if db.Status != status.DatabaseRunning {
		writeError(w, http.StatusConflict, "database must be running to change external access")
		return
	}
	// Already in the requested state — nothing to recreate.
	if db.HostPort.Valid == req.Enabled {
		writeJSON(w, http.StatusOK, map[string]string{"status": db.Status})
		return
	}

	// Mark transitional before the async recreate so the UI shows progress.
	if _, err := h.queries.UpdateDatabaseStatus(r.Context(), generated.UpdateDatabaseStatusParams{
		ID:     dbUUID,
		Status: status.DatabaseCreating,
	}); err != nil {
		writeServerError(w, r, "failed to update database", err)
		return
	}

	payload, err := json.Marshal(map[string]any{"database_id": databaseID, "enable": req.Enabled})
	if err != nil {
		writeServerError(w, r, "failed to create reconfigure task", err)
		return
	}
	if _, err := h.asynq.Enqueue(asynq.NewTask("reconfigure_db", payload), asynq.Queue("critical")); err != nil {
		writeServerError(w, r, "failed to enqueue reconfigure task", err)
		return
	}

	h.audit(r, "update_database", "database", databaseID, map[string]any{
		"external_access": req.Enabled,
	})

	writeJSON(w, http.StatusAccepted, map[string]string{"status": status.DatabaseCreating})
}

type updateDatabaseRequest struct {
	CPULimit    float64 `json:"cpu_limit"`
	MemoryLimit int64   `json:"memory_limit"`
}

// SetDatabaseResources edits a managed database's resource limits (CPU cores /
// memory bytes) and applies them live to the running container. The limit is
// persisted regardless, so it also takes effect if the container is later
// recreated.
//
// ⚠️ Renamed from UpdateDatabase and MOVED from PUT .../databases/{databaseId}
// to .../databases/{databaseId}/resources in v0.1.20, to match the
// application endpoint (SetResources). The old path only ever set these two
// fields, so "update the database" overstated it and the asymmetry was a
// standing papercut. This is a breaking change to a published path and is
// listed as one.
//
//apidoc:tag databases
//apidoc:title Set Resources
func (h *Handler) SetDatabaseResources(w http.ResponseWriter, r *http.Request) {
	databaseID := chi.URLParam(r, "databaseId")
	var dbUUID pgtype.UUID
	if err := dbUUID.Scan(databaseID); err != nil {
		writeError(w, http.StatusBadRequest, "invalid database id")
		return
	}

	if !h.canAccessDatabase(r, dbUUID) {
		writeError(w, http.StatusForbidden, "access denied")
		return
	}

	var req updateDatabaseRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	// The REST response has never said whether the live apply landed, and
	// changing that would change a published response shape; the MCP tool is
	// the caller that needs to know.
	db, _, err := h.dbService.SetResources(r.Context(), dbUUID, req.CPULimit, req.MemoryLimit)
	if err != nil {
		if errors.Is(err, service.ErrNegativeLimit) {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeServerError(w, r, "failed to update database", err)
		return
	}

	h.audit(r, "set_database_resources", "database", databaseID, map[string]any{
		"cpu_limit":    req.CPULimit,
		"memory_limit": req.MemoryLimit,
	})

	writeJSON(w, http.StatusOK, db)
}

// databaseBackupEnabled reports whether a database can be backed up: known
// engines have a logical-dump tool; "other" supports backup when a backup_mode
// (volume_snapshot or command) was configured. redis (cache) is not supported.
func databaseBackupEnabled(db generated.Database) bool {
	switch db.Type {
	case "postgres", "mysql", "mongo":
		return true
	case "other":
		return db.BackupMode == "volume_snapshot" || db.BackupMode == "command"
	}
	return false
}

type databaseBackupResponse struct {
	ID             string     `json:"id"`
	Status         string     `json:"status"`
	SizeBytes      int64      `json:"size_bytes"`
	HasRemote      bool       `json:"has_remote"`
	RemoteKey      string     `json:"remote_key,omitempty"`
	ConfigID       *string    `json:"config_id,omitempty"`
	TargetDatabase string     `json:"target_database,omitempty"`
	StartedAt      time.Time  `json:"started_at"`
	FinishedAt     *time.Time `json:"finished_at,omitempty"`
	Error          string     `json:"error,omitempty"`
	Log            string     `json:"log,omitempty"`
}

func toBackupResponse(b generated.DatabaseBackup) databaseBackupResponse {
	resp := databaseBackupResponse{
		ID:             uuidToString(b.ID),
		Status:         b.Status,
		SizeBytes:      b.SizeBytes,
		HasRemote:      b.RemoteKey.Valid,
		TargetDatabase: b.TargetDatabase,
		Log:            b.Log,
		StartedAt:      b.StartedAt.Time,
	}
	if b.RemoteKey.Valid {
		resp.RemoteKey = b.RemoteKey.String
	}
	if b.BackupConfigID.Valid {
		s := uuidToString(b.BackupConfigID)
		resp.ConfigID = &s
	}
	if b.FinishedAt.Valid {
		t := b.FinishedAt.Time
		resp.FinishedAt = &t
	}
	if b.Error.Valid {
		resp.Error = b.Error.String
	}
	return resp
}

// ListDatabaseBackups returns the recent backup runs for a database (newest first).
//
//apidoc:tag databases/volumes-backups
//apidoc:title Get Backups
//apidoc:order 7
func (h *Handler) ListDatabaseBackups(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "databaseId")
	var dbUUID pgtype.UUID
	if err := dbUUID.Scan(id); err != nil {
		writeError(w, http.StatusBadRequest, "invalid database id")
		return
	}
	if !h.canAccessDatabase(r, dbUUID) {
		writeError(w, http.StatusForbidden, "access denied")
		return
	}

	backups, err := h.queries.ListDatabaseBackups(r.Context(), generated.ListDatabaseBackupsParams{
		DatabaseID: dbUUID,
		Limit:      50,
	})
	if err != nil {
		writeServerError(w, r, "failed to list backups", err)
		return
	}

	resp := make([]databaseBackupResponse, 0, len(backups))
	for _, b := range backups {
		resp = append(resp, toBackupResponse(b))
	}
	writeJSON(w, http.StatusOK, resp)
}

type databaseRestoreResponse struct {
	ID         string     `json:"id"`
	BackupID   string     `json:"backup_id,omitempty"`
	Status     string     `json:"status"`
	StartedAt  time.Time  `json:"started_at"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
	Error      string     `json:"error,omitempty"`
	Log        string     `json:"log,omitempty"`
}

// ListDatabaseRestores returns the recent restore runs for a database.
//
//apidoc:tag databases/volumes-backups
//apidoc:title Get Restores
//apidoc:order 8
func (h *Handler) ListDatabaseRestores(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "databaseId")
	var dbUUID pgtype.UUID
	if err := dbUUID.Scan(id); err != nil {
		writeError(w, http.StatusBadRequest, "invalid database id")
		return
	}
	if !h.canAccessDatabase(r, dbUUID) {
		writeError(w, http.StatusForbidden, "access denied")
		return
	}

	runs, err := h.queries.ListDatabaseRestores(r.Context(), generated.ListDatabaseRestoresParams{
		DatabaseID: dbUUID,
		Limit:      50,
	})
	if err != nil {
		writeServerError(w, r, "failed to list restores", err)
		return
	}
	out := make([]databaseRestoreResponse, 0, len(runs))
	for _, b := range runs {
		resp := databaseRestoreResponse{
			ID:        uuidToString(b.ID),
			Status:    b.Status,
			StartedAt: b.StartedAt.Time,
			Error:     b.Error.String,
			Log:       b.Log.String,
		}
		if b.BackupID.Valid {
			resp.BackupID = uuidToString(b.BackupID)
		}
		if b.FinishedAt.Valid {
			t := b.FinishedAt.Time
			resp.FinishedAt = &t
		}
		out = append(out, resp)
	}
	writeJSON(w, http.StatusOK, out)
}

// DeleteDatabaseBackup removes one backup (row + local file + S3 object).
//
//apidoc:tag databases/volumes-backups
func (h *Handler) DeleteDatabaseBackup(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "databaseId")
	var dbUUID pgtype.UUID
	if err := dbUUID.Scan(id); err != nil {
		writeError(w, http.StatusBadRequest, "invalid database id")
		return
	}
	var backupUUID pgtype.UUID
	if err := backupUUID.Scan(chi.URLParam(r, "backupId")); err != nil {
		writeError(w, http.StatusBadRequest, "invalid backup id")
		return
	}
	// Owner-only: destroying a backup is irreversible, unlike routine database
	// operation which shared access already covers.
	if !h.isDatabaseOwner(r, dbUUID) {
		writeError(w, http.StatusForbidden, "access denied")
		return
	}
	if err := h.dbService.DeleteBackup(r.Context(), dbUUID, backupUUID); err != nil {
		writeError(w, http.StatusNotFound, "backup not found")
		return
	}
	h.audit(r, "delete_database_backup", "database", id, map[string]any{"backup_id": chi.URLParam(r, "backupId")})
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

// BackupDatabase enqueues an online logical-dump backup of a running database.
//
//apidoc:tag databases/volumes-backups
func (h *Handler) BackupDatabase(w http.ResponseWriter, r *http.Request) {
	databaseID := chi.URLParam(r, "databaseId")
	var dbUUID pgtype.UUID
	if err := dbUUID.Scan(databaseID); err != nil {
		writeError(w, http.StatusBadRequest, "invalid database id")
		return
	}
	if !h.canAccessDatabase(r, dbUUID) {
		writeError(w, http.StatusForbidden, "access denied")
		return
	}

	db, err := h.queries.GetDatabase(r.Context(), dbUUID)
	if err != nil {
		writeError(w, http.StatusNotFound, "database not found")
		return
	}
	if !databaseBackupEnabled(db) {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("logical backup is not supported for %s", db.Type))
		return
	}
	if db.Status != status.DatabaseRunning {
		writeError(w, http.StatusConflict, "database must be running to back up")
		return
	}
	// Reject overlapping backups so a double-click doesn't spawn two runs.
	if recent, err := h.queries.ListDatabaseBackups(r.Context(), generated.ListDatabaseBackupsParams{DatabaseID: dbUUID, Limit: 5}); err == nil {
		for _, b := range recent {
			if b.Status == "running" {
				writeError(w, http.StatusConflict, "a backup is already in progress")
				return
			}
		}
	}

	payload, err := json.Marshal(map[string]any{"database_id": databaseID})
	if err != nil {
		writeServerError(w, r, "failed to create backup task", err)
		return
	}
	// MaxRetry(1): a backup is user-triggered and records a run row on each
	// attempt, so avoid asynq's default 25 retries spraying failed rows.
	if _, err := h.asynq.Enqueue(asynq.NewTask("backup_db", payload), asynq.Queue("default"), asynq.MaxRetry(1)); err != nil {
		writeServerError(w, r, "failed to enqueue backup task", err)
		return
	}

	h.audit(r, "backup_database", "database", databaseID, nil)
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "running"})
}

type restoreDatabaseRequest struct {
	BackupID string `json:"backup_id"`
}

// RestoreDatabase enqueues a restore of a database from one of its recorded backups.
//
//apidoc:tag databases/volumes-backups
func (h *Handler) RestoreDatabase(w http.ResponseWriter, r *http.Request) {
	databaseID := chi.URLParam(r, "databaseId")
	var dbUUID pgtype.UUID
	if err := dbUUID.Scan(databaseID); err != nil {
		writeError(w, http.StatusBadRequest, "invalid database id")
		return
	}
	if !h.canAccessDatabase(r, dbUUID) {
		writeError(w, http.StatusForbidden, "access denied")
		return
	}

	var req restoreDatabaseRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	var backupUUID pgtype.UUID
	if err := backupUUID.Scan(req.BackupID); err != nil {
		writeError(w, http.StatusBadRequest, "invalid backup id")
		return
	}

	db, err := h.queries.GetDatabase(r.Context(), dbUUID)
	if err != nil {
		writeError(w, http.StatusNotFound, "database not found")
		return
	}
	if !databaseBackupEnabled(db) {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("restore is not supported for %s", db.Type))
		return
	}
	if db.Status != status.DatabaseRunning {
		writeError(w, http.StatusConflict, "database must be running to restore")
		return
	}

	backup, err := h.queries.GetDatabaseBackup(r.Context(), backupUUID)
	if err != nil {
		writeError(w, http.StatusNotFound, "backup not found")
		return
	}
	if backup.DatabaseID != db.ID {
		writeError(w, http.StatusBadRequest, "backup does not belong to this database")
		return
	}
	if backup.Status != "succeeded" {
		writeError(w, http.StatusConflict, "only a succeeded backup can be restored")
		return
	}

	payload, err := json.Marshal(map[string]any{"database_id": databaseID, "backup_id": req.BackupID})
	if err != nil {
		writeServerError(w, r, "failed to create restore task", err)
		return
	}
	if _, err := h.asynq.Enqueue(asynq.NewTask("restore_db", payload), asynq.Queue("critical")); err != nil {
		writeServerError(w, r, "failed to enqueue restore task", err)
		return
	}

	h.audit(r, "restore_database", "database", databaseID, map[string]any{"backup_id": req.BackupID})
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "restoring"})
}

// databaseUpgradable reports whether a database supports the guarded major-version
// upgrade (logical dump-and-reload engines only; "other" and redis are excluded).
func databaseUpgradable(dbType string) bool {
	return dbType == "postgres" || dbType == "mysql" || dbType == "mongo"
}

type upgradeDatabaseRequest struct {
	TargetVersion string `json:"target_version"`
}

// UpgradeDatabase enqueues a guarded major-version upgrade: the worker dumps the
// current data, rebuilds the container at the target version, and restores the
// dump (rolling back to the prior version on failure). Brief downtime.
//
//apidoc:tag databases
//apidoc:title Upgrade Version
//apidoc:order 6
func (h *Handler) UpgradeDatabase(w http.ResponseWriter, r *http.Request) {
	databaseID := chi.URLParam(r, "databaseId")
	var dbUUID pgtype.UUID
	if err := dbUUID.Scan(databaseID); err != nil {
		writeError(w, http.StatusBadRequest, "invalid database id")
		return
	}
	if !h.canAccessDatabase(r, dbUUID) {
		writeError(w, http.StatusForbidden, "access denied")
		return
	}

	var req upgradeDatabaseRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	req.TargetVersion = strings.TrimSpace(req.TargetVersion)
	if req.TargetVersion == "" {
		writeError(w, http.StatusBadRequest, "target_version is required")
		return
	}

	db, err := h.queries.GetDatabase(r.Context(), dbUUID)
	if err != nil {
		writeError(w, http.StatusNotFound, "database not found")
		return
	}
	if !databaseUpgradable(db.Type) {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("guarded upgrade is not supported for %s", db.Type))
		return
	}
	if db.Status != status.DatabaseRunning {
		writeError(w, http.StatusConflict, "database must be running to upgrade")
		return
	}
	// A target equal to the current version is allowed: with digest pinning it
	// means "refresh to the latest patch of this tag" (re-pull + re-pin) rather
	// than a no-op. It still runs the guarded dump/restore flow for safety.

	if _, err := h.queries.UpdateDatabaseStatus(r.Context(), generated.UpdateDatabaseStatusParams{
		ID:     dbUUID,
		Status: status.DatabaseUpgrading,
	}); err != nil {
		writeServerError(w, r, "failed to update database", err)
		return
	}

	payload, err := json.Marshal(map[string]any{"database_id": databaseID, "target_version": req.TargetVersion})
	if err != nil {
		writeServerError(w, r, "failed to create upgrade task", err)
		return
	}
	// No auto-retry: the worker performs its own dump-and-rollback; a blind retry
	// could re-run a destructive step.
	if _, err := h.asynq.Enqueue(asynq.NewTask("upgrade_db", payload), asynq.Queue("critical"), asynq.MaxRetry(0)); err != nil {
		writeServerError(w, r, "failed to enqueue upgrade task", err)
		return
	}

	h.audit(r, "upgrade_database", "database", databaseID, map[string]any{
		"from": db.Version, "to": req.TargetVersion,
	})
	writeJSON(w, http.StatusAccepted, map[string]string{"status": status.DatabaseUpgrading})
}

// StopDatabase stops the managed database container without removing it.
//
//apidoc:tag databases/actions
//apidoc:title Stop Database
//apidoc:order 2
func (h *Handler) StopDatabase(w http.ResponseWriter, r *http.Request) {
	databaseID := chi.URLParam(r, "databaseId")
	var dbUUID pgtype.UUID
	if err := dbUUID.Scan(databaseID); err != nil {
		writeError(w, http.StatusBadRequest, "invalid database id")
		return
	}

	if !h.canAccessDatabase(r, dbUUID) {
		writeError(w, http.StatusForbidden, "access denied")
		return
	}

	db, err := h.queries.GetDatabase(r.Context(), dbUUID)
	if err != nil {
		writeError(w, http.StatusNotFound, "database not found")
		return
	}

	rt, err := h.runtimeForDatabase(r.Context(), dbUUID)
	if err != nil {
		writeServerError(w, r, "failed to reach the database's server", err)
		return
	}

	// The database container name matches its slug (see provision/delete paths).
	if err := rt.StopContainer(r.Context(), db.Slug); err != nil {
		slog.Error("failed to stop database container", "container", db.Slug, "error", err)
		writeError(w, http.StatusInternalServerError, "failed to stop database")
		return
	}

	updated, err := h.queries.UpdateDatabaseStatus(r.Context(), generated.UpdateDatabaseStatusParams{
		ID:     dbUUID,
		Status: status.DatabaseStopped,
	})
	if err != nil {
		writeServerError(w, r, "failed to update database status", err)
		return
	}

	h.audit(r, "stop_database", "database", databaseID, nil)
	writeJSON(w, http.StatusOK, databaseResponse{Database: updated})
}

// StartDatabase starts a stopped database container.
//
//apidoc:tag databases/actions
//apidoc:title Start Database
//apidoc:order 1
func (h *Handler) StartDatabase(w http.ResponseWriter, r *http.Request) {
	databaseID := chi.URLParam(r, "databaseId")
	var dbUUID pgtype.UUID
	if err := dbUUID.Scan(databaseID); err != nil {
		writeError(w, http.StatusBadRequest, "invalid database id")
		return
	}

	if !h.canAccessDatabase(r, dbUUID) {
		writeError(w, http.StatusForbidden, "access denied")
		return
	}

	db, err := h.queries.GetDatabase(r.Context(), dbUUID)
	if err != nil {
		writeError(w, http.StatusNotFound, "database not found")
		return
	}

	rt, err := h.runtimeForDatabase(r.Context(), dbUUID)
	if err != nil {
		writeServerError(w, r, "failed to reach the database's server", err)
		return
	}

	if err := rt.StartContainer(r.Context(), db.Slug); err != nil {
		slog.Error("failed to start database container", "container", db.Slug, "error", err)
		writeError(w, http.StatusInternalServerError, "failed to start database")
		return
	}

	updated, err := h.queries.UpdateDatabaseStatus(r.Context(), generated.UpdateDatabaseStatusParams{
		ID:     dbUUID,
		Status: status.DatabaseRunning,
	})
	if err != nil {
		writeServerError(w, r, "failed to update database status", err)
		return
	}

	h.audit(r, "start_database", "database", databaseID, nil)
	writeJSON(w, http.StatusOK, databaseResponse{Database: updated})
}

// RestartDatabase stops and starts the existing database container (no recreate).
//
//apidoc:tag databases/actions
//apidoc:title Restart Database
//apidoc:order 3
func (h *Handler) RestartDatabase(w http.ResponseWriter, r *http.Request) {
	databaseID := chi.URLParam(r, "databaseId")
	var dbUUID pgtype.UUID
	if err := dbUUID.Scan(databaseID); err != nil {
		writeError(w, http.StatusBadRequest, "invalid database id")
		return
	}

	if !h.canAccessDatabase(r, dbUUID) {
		writeError(w, http.StatusForbidden, "access denied")
		return
	}

	db, err := h.queries.GetDatabase(r.Context(), dbUUID)
	if err != nil {
		writeError(w, http.StatusNotFound, "database not found")
		return
	}

	rt, err := h.runtimeForDatabase(r.Context(), dbUUID)
	if err != nil {
		writeServerError(w, r, "failed to reach the database's server", err)
		return
	}

	if err := rt.StopContainer(r.Context(), db.Slug); err != nil {
		slog.Error("failed to stop database container for restart", "container", db.Slug, "error", err)
		writeError(w, http.StatusInternalServerError, "failed to restart database")
		return
	}
	if err := rt.StartContainer(r.Context(), db.Slug); err != nil {
		slog.Error("failed to start database container for restart", "container", db.Slug, "error", err)
		writeError(w, http.StatusInternalServerError, "failed to restart database")
		return
	}

	updated, err := h.queries.UpdateDatabaseStatus(r.Context(), generated.UpdateDatabaseStatusParams{
		ID:     dbUUID,
		Status: status.DatabaseRunning,
	})
	if err != nil {
		writeServerError(w, r, "failed to update database status", err)
		return
	}

	h.audit(r, "restart_database", "database", databaseID, nil)
	writeJSON(w, http.StatusOK, databaseResponse{Database: updated})
}

// ReloadDatabase recreates the managed database container from its stored record
// (image + pinned digest, env, port, volume, network, resource limits), reattaching
// the data volume so nothing is lost. Unlike Restart — which stops and starts the
// existing container — Reload works when the container has drifted from the record
// or been deleted entirely, making it the recovery path for a missing container.
// The recreate runs as an async task; the database is marked transitional so the
// UI reflects the brief downtime. External-access (loopback host port) state is
// preserved.
//
//apidoc:tag databases/actions
//apidoc:title Reload Database
//apidoc:order 4
func (h *Handler) ReloadDatabase(w http.ResponseWriter, r *http.Request) {
	databaseID := chi.URLParam(r, "databaseId")
	var dbUUID pgtype.UUID
	if err := dbUUID.Scan(databaseID); err != nil {
		writeError(w, http.StatusBadRequest, "invalid database id")
		return
	}

	if !h.canAccessDatabase(r, dbUUID) {
		writeError(w, http.StatusForbidden, "access denied")
		return
	}

	db, err := h.queries.GetDatabase(r.Context(), dbUUID)
	if err != nil {
		writeError(w, http.StatusNotFound, "database not found")
		return
	}

	// Refuse to reload mid-task: a recreate now would race the owning worker
	// (provision/upgrade/backup) over the same container and volume.
	switch db.Status {
	case status.DatabaseCreating, status.DatabaseUpgrading, status.DatabaseBackingUp:
		writeError(w, http.StatusConflict, "database is busy — try again once the current operation finishes")
		return
	}

	// Mark transitional before the async recreate so the UI shows progress and
	// the reconcile loop leaves it alone while the container is momentarily absent.
	if _, err := h.queries.UpdateDatabaseStatus(r.Context(), generated.UpdateDatabaseStatusParams{
		ID:     dbUUID,
		Status: status.DatabaseCreating,
	}); err != nil {
		writeServerError(w, r, "failed to update database", err)
		return
	}

	payload, err := json.Marshal(map[string]string{"database_id": databaseID})
	if err != nil {
		writeServerError(w, r, "failed to create reload task", err)
		return
	}
	if _, err := h.asynq.Enqueue(asynq.NewTask("reload_db", payload), asynq.Queue("critical")); err != nil {
		writeServerError(w, r, "failed to enqueue reload task", err)
		return
	}

	h.audit(r, "reload_database", "database", databaseID, nil)
	writeJSON(w, http.StatusAccepted, map[string]string{"status": status.DatabaseCreating})
}

//apidoc:tag databases
func (h *Handler) DeleteDatabase(w http.ResponseWriter, r *http.Request) {
	databaseID := chi.URLParam(r, "databaseId")
	var dbUUID pgtype.UUID
	if err := dbUUID.Scan(databaseID); err != nil {
		writeError(w, http.StatusBadRequest, "invalid database id")
		return
	}

	// Owner-only: shared access grants full operational use of the project's
	// databases, but not the right to destroy one.
	if !h.isDatabaseOwner(r, dbUUID) {
		writeError(w, http.StatusForbidden, "access denied")
		return
	}

	// Backups are kept unless the caller asks for them to go. The default is the
	// safe direction on purpose: an omitted parameter from an older client, a
	// script, or a mistyped request keeps the data rather than destroying it.
	deleteBackups := r.URL.Query().Get("delete_backups") == "true"

	// Read the impact before the delete cascades it away, so the audit entry can
	// answer "where did the backups go" afterwards. A failure here must not block
	// the delete, but it must not vanish either — without this log the audit
	// record is simply missing its detail with nothing explaining why.
	impact, impactErr := h.dbService.DeletionImpact(r.Context(), dbUUID)
	if impactErr != nil {
		slog.Warn("could not determine deletion impact; audit detail will be incomplete",
			"database_id", databaseID, "error", impactErr)
	}

	if err := h.dbService.Delete(r.Context(), dbUUID, !deleteBackups); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to delete database")
		return
	}

	details := map[string]any{"backups_deleted": deleteBackups}
	if impactErr == nil {
		if deleteBackups {
			details["backups_destroyed"] = impact.BackupCount
		} else {
			details["backups_kept"] = impact.BackupCount
		}
		// Same normalisation the API response uses: a consumer reading both
		// should not have to handle null here and [] there.
		details["backup_destinations"] = orEmpty(impact.Destinations)
	}
	h.audit(r, "delete_database", "database", databaseID, details)

	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

// GetDatabaseDeletionImpact reports what deleting this database would destroy
// beyond the database itself, so the confirmation dialog can state the real
// consequence instead of implying only the container and its data are at stake.
//
//apidoc:tag databases
//apidoc:title Get Deletion Impact
//apidoc:order 5
func (h *Handler) GetDatabaseDeletionImpact(w http.ResponseWriter, r *http.Request) {
	databaseID := chi.URLParam(r, "databaseId")
	var dbUUID pgtype.UUID
	if err := dbUUID.Scan(databaseID); err != nil {
		writeError(w, http.StatusBadRequest, "invalid database id")
		return
	}
	if !h.canAccessDatabase(r, dbUUID) {
		writeError(w, http.StatusForbidden, "access denied")
		return
	}
	impact, err := h.dbService.DeletionImpact(r.Context(), dbUUID)
	if err != nil {
		writeServerError(w, r, "failed to determine deletion impact", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"backup_count":        impact.BackupCount,
		"backup_destinations": orEmpty(impact.Destinations),
	})
}

// orEmpty renders a nil slice as [] rather than null, so clients never have to
// branch on which one they got.
func orEmpty(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

func buildConnectionString(db generated.Database, creds map[string]string) string {
	host := db.InternalHost.String
	port := db.InternalPort.Int32

	switch db.Type {
	case "postgres":
		return fmt.Sprintf("postgresql://%s:%s@%s:%d/%s",
			creds["user"], creds["password"], host, port, creds["database"])
	case "mysql":
		return fmt.Sprintf("mysql://%s:%s@%s:%d/%s",
			creds["user"], creds["password"], host, port, creds["database"])
	case "redis":
		return fmt.Sprintf("redis://:%s@%s:%d",
			creds["password"], host, port)
	case "mongo":
		return fmt.Sprintf("mongodb://%s:%s@%s:%d",
			creds["username"], creds["password"], host, port)
	default:
		return ""
	}
}

// RestoreDatabaseFromTombstone recreates a deleted database from the tombstone
// its backups hang off, and restores the chosen backup into it.
//
// The replacement comes back under the original slug and credentials, so
// applications in the project reconnect without an env-var edit. That is why
// this is the offered path rather than "restore into a new database": attaching
// a database injects no connection variables, so a differently-named
// replacement leaves every dependent application pointing at a host that is not
// there.
//
//apidoc:tag databases/volumes-backups
func (h *Handler) RestoreDatabaseFromTombstone(w http.ResponseWriter, r *http.Request) {
	projectID := chi.URLParam(r, "projectId")
	var projectUUID pgtype.UUID
	if err := projectUUID.Scan(projectID); err != nil {
		writeError(w, http.StatusBadRequest, "invalid project id")
		return
	}
	backupID := chi.URLParam(r, "backupId")
	var backupUUID pgtype.UUID
	if err := backupUUID.Scan(backupID); err != nil {
		writeError(w, http.StatusBadRequest, "invalid backup id")
		return
	}
	if !h.canAccessProject(r, projectUUID) {
		writeError(w, http.StatusForbidden, "access denied")
		return
	}

	backup, err := h.queries.GetDatabaseBackup(r.Context(), backupUUID)
	if err != nil {
		writeError(w, http.StatusNotFound, "backup not found")
		return
	}
	if !backup.TombstoneID.Valid {
		writeError(w, http.StatusBadRequest, "this backup still belongs to a live database; restore it from there")
		return
	}
	if backup.Status != "succeeded" {
		writeError(w, http.StatusBadRequest, "only a succeeded backup can be restored")
		return
	}

	// The tombstone is the access boundary here: the backup is reachable only
	// through the project that owns the tombstone, so a backup belonging to
	// another project must not be restorable through this project's URL.
	tombstone, err := h.queries.GetDatabaseTombstone(r.Context(), backup.TombstoneID)
	if err != nil {
		writeError(w, http.StatusNotFound, "the deleted database's record is gone")
		return
	}
	if tombstone.ProjectID != projectUUID {
		writeError(w, http.StatusForbidden, "access denied")
		return
	}

	db, err := h.dbService.RestoreFromTombstone(r.Context(), tombstone.ID, backupUUID)
	if err != nil {
		slog.Error("failed to recreate database from tombstone", "tombstone_id", uuidToString(tombstone.ID), "error", err)
		writeError(w, http.StatusInternalServerError, "failed to recreate the database")
		return
	}

	payload, err := json.Marshal(map[string]string{
		"database_id": uuidToString(db.ID),
		"backup_id":   backupID,
	})
	if err != nil {
		writeServerError(w, r, "failed to schedule the restore", err)
		return
	}
	if _, err := h.asynq.Enqueue(asynq.NewTask(worker.TypeReplaceDatabase, payload), asynq.Queue("critical")); err != nil {
		// The row exists and owns its backups again, so this is recoverable:
		// the database is listed as creating and Reload will provision it.
		slog.Error("replacement created but could not be scheduled", "database_id", uuidToString(db.ID), "error", err)
		writeError(w, http.StatusInternalServerError, "the database was recreated but the restore could not be scheduled")
		return
	}

	h.audit(r, "restore_database_from_tombstone", "database", uuidToString(db.ID), map[string]any{
		"backup_id": backupID,
		"slug":      db.Slug,
	})
	writeJSON(w, http.StatusAccepted, databaseResponse{Database: db})
}

// orphanedBackupResponse is a backup whose database no longer exists. It
// carries what the database WAS, because there is no resource page left to read
// that from — which is the whole reason these need their own listing.
type orphanedBackupResponse struct {
	databaseBackupResponse
	TombstoneID       string    `json:"tombstone_id"`
	DatabaseName      string    `json:"database_name"`
	DatabaseSlug      string    `json:"database_slug"`
	DatabaseType      string    `json:"database_type"`
	DatabaseDeletedAt time.Time `json:"database_deleted_at"`
}

// ListProjectOrphanedDatabaseBackups returns the backups in a project whose database has
// been deleted. Without this they exist and are billed but appear nowhere: the
// per-database Backups tab is gone along with the database.
//
//apidoc:tag databases/volumes-backups
//apidoc:title Get Orphaned Database Backups
//apidoc:description Backups whose database has been deleted but which were kept, listed against the tombstone that now parents them. Database backups only — an application's volume backups are erased with the application and can never reach this state.
//apidoc:order 10
func (h *Handler) ListProjectOrphanedDatabaseBackups(w http.ResponseWriter, r *http.Request) {
	projectID := chi.URLParam(r, "projectId")
	var projectUUID pgtype.UUID
	if err := projectUUID.Scan(projectID); err != nil {
		writeError(w, http.StatusBadRequest, "invalid project id")
		return
	}
	if !h.canAccessProject(r, projectUUID) {
		writeError(w, http.StatusForbidden, "access denied")
		return
	}

	rows, err := h.queries.ListOrphanedBackupsByProject(r.Context(), projectUUID)
	if err != nil {
		writeServerError(w, r, "failed to list backups", err)
		return
	}

	resp := make([]orphanedBackupResponse, 0, len(rows))
	for _, row := range rows {
		resp = append(resp, orphanedBackupResponse{
			databaseBackupResponse: toBackupResponse(generated.DatabaseBackup{
				ID:             row.ID,
				Status:         row.Status,
				SizeBytes:      row.SizeBytes,
				RemoteKey:      row.RemoteKey,
				TargetDatabase: row.TargetDatabase,
				Log:            row.Log,
				StartedAt:      row.StartedAt,
				FinishedAt:     row.FinishedAt,
				Error:          row.Error,
				BackupConfigID: row.BackupConfigID,
			}),
			TombstoneID:       uuidToString(row.TombstoneID),
			DatabaseName:      row.DatabaseName,
			DatabaseSlug:      row.DatabaseSlug,
			DatabaseType:      row.DatabaseType,
			DatabaseDeletedAt: row.DatabaseDeletedAt.Time,
		})
	}
	writeJSON(w, http.StatusOK, resp)
}

// DeleteOrphanedDatabaseBackup erases a backup whose database is gone. The per-database
// delete cannot reach these — the database it hung off is what disappeared.
//
// Session-gated, so this has no page in the reference — the title is for
// openapi.json, which third-party codegen reads, and keeps the pair consistent
// with its sibling listing.
//
//apidoc:tag databases/volumes-backups
//apidoc:title Delete Orphaned Database Backup
func (h *Handler) DeleteOrphanedDatabaseBackup(w http.ResponseWriter, r *http.Request) {
	projectID := chi.URLParam(r, "projectId")
	var projectUUID pgtype.UUID
	if err := projectUUID.Scan(projectID); err != nil {
		writeError(w, http.StatusBadRequest, "invalid project id")
		return
	}
	backupID := chi.URLParam(r, "backupId")
	var backupUUID pgtype.UUID
	if err := backupUUID.Scan(backupID); err != nil {
		writeError(w, http.StatusBadRequest, "invalid backup id")
		return
	}
	// Owner-only: destroying a backup is irreversible, unlike routine project
	// operation which shared access already covers.
	if !h.isProjectOwner(r, projectUUID) {
		writeError(w, http.StatusForbidden, "access denied")
		return
	}

	backup, err := h.queries.GetDatabaseBackup(r.Context(), backupUUID)
	if err != nil {
		writeError(w, http.StatusNotFound, "backup not found")
		return
	}
	if !backup.TombstoneID.Valid {
		writeError(w, http.StatusBadRequest, "this backup belongs to a live database; delete it from there")
		return
	}
	// Same boundary as the restore path: the tombstone's project is what makes
	// this backup reachable, so another project's orphan must not be.
	tombstone, err := h.queries.GetDatabaseTombstone(r.Context(), backup.TombstoneID)
	if err != nil || tombstone.ProjectID != projectUUID {
		writeError(w, http.StatusForbidden, "access denied")
		return
	}

	if err := h.dbService.DeleteOrphanedBackup(r.Context(), backupUUID); err != nil {
		writeServerError(w, r, "failed to delete the backup", err)
		return
	}

	h.audit(r, "delete_orphaned_backup", "project", projectID, map[string]any{
		"backup_id":     backupID,
		"database_name": tombstone.Name,
	})
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

// ProvisionDatabase provisions a managed database for a caller outside this
// package, currently the MCP create_database tool.
//
// ⚠️ A thin adapter on purpose. createDatabaseRecord is 156 lines of
// credential generation, encryption and provisioning enqueue with two existing
// callers (this file's REST endpoint and template instantiation), and one of
// those two — InstantiateTemplate — has no integration test. Lifting that body
// into the service would be the tidier shape, but it is the wrong trade
// without a net under both callers: nothing here changes it, so nothing here
// can regress it.
//
// The type assertion back to a request struct stays inside this package, which
// is what keeps internal/mcpserver from needing to know the REST wire shape.
func (h *Handler) ProvisionDatabase(ctx context.Context, project generated.Project, in service.NewDatabase) (generated.Database, error) {
	version := in.Version
	if version == "" {
		version = defaultVersions[in.Type]
	}
	db, _, err := h.createDatabaseRecord(ctx, project, createDatabaseRequest{
		Name:    in.Name,
		Type:    in.Type,
		Version: version,
	})
	return db, err
}

// DatabaseTypeHasDefaultVersion reports whether a type is one of the managed
// engines, which is also what decides whether a version default exists for it.
// Exported for the same reason as ProvisionDatabase: the check belongs beside
// the map it reads.
func DatabaseTypeHasDefaultVersion(dbType string) bool {
	_, ok := defaultVersions[dbType]
	return ok
}
