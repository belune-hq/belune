package handler

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/hibiken/asynq"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/belune-hq/belune/internal/pkg/cronspec"
	"github.com/belune-hq/belune/internal/status"
	"github.com/belune-hq/belune/internal/store/generated"
	"github.com/belune-hq/belune/internal/worker"
)

type databaseBackupConfigResponse struct {
	ID            string     `json:"id"`
	DatabaseID    string     `json:"database_id"`
	DestinationID string     `json:"destination_id"`
	Prefix        string     `json:"prefix"`
	Schedule      string     `json:"schedule"`
	Timezone      string     `json:"timezone"`
	KeepLatest    *int32     `json:"keep_latest,omitempty"`
	Enabled       bool       `json:"enabled"`
	Databases     []string   `json:"databases"`
	AllDatabases  bool       `json:"all_databases"`
	LastRunAt     *time.Time `json:"last_run_at,omitempty"`
	CreatedAt     time.Time  `json:"created_at"`
	UpdatedAt     time.Time  `json:"updated_at"`
}

func toBackupConfigResponse(c generated.DatabaseBackupConfig) databaseBackupConfigResponse {
	all := c.TargetDatabase == "*"
	dbs := []string{}
	if !all {
		for _, p := range strings.Split(c.TargetDatabase, ",") {
			if p = strings.TrimSpace(p); p != "" {
				dbs = append(dbs, p)
			}
		}
	}
	resp := databaseBackupConfigResponse{
		ID:            uuidToString(c.ID),
		DatabaseID:    uuidToString(c.DatabaseID),
		DestinationID: uuidToString(c.DestinationID),
		Prefix:        c.Prefix,
		Schedule:      c.Schedule,
		Timezone:      c.Timezone,
		Enabled:       c.Enabled,
		Databases:     dbs,
		AllDatabases:  all,
		CreatedAt:     c.CreatedAt.Time,
		UpdatedAt:     c.UpdatedAt.Time,
	}
	if c.KeepLatest.Valid {
		v := c.KeepLatest.Int32
		resp.KeepLatest = &v
	}
	if c.LastRunAt.Valid {
		t := c.LastRunAt.Time
		resp.LastRunAt = &t
	}
	return resp
}

type databaseBackupConfigRequest struct {
	DestinationID string `json:"destination_id"`
	Prefix        string `json:"prefix"`
	Schedule      string `json:"schedule"`
	// Timezone the schedule is evaluated in. Omitted means UTC on create and the
	// stored zone on update, so a partial PUT never silently changes when a
	// backup fires.
	Timezone   string `json:"timezone"`
	KeepLatest *int32 `json:"keep_latest"`
	Enabled    *bool  `json:"enabled"`
	// Databases lists specific database names to back up. Empty means all
	// databases in the container (cluster backup).
	Databases []string `json:"databases"`
}

// targetDatabase resolves the request into the stored target: "*" when no
// databases are listed (all), otherwise the comma-joined specific names.
func (req *databaseBackupConfigRequest) targetDatabase() string {
	var names []string
	for _, d := range req.Databases {
		if d = strings.TrimSpace(d); d != "" {
			names = append(names, d)
		}
	}
	if len(names) == 0 {
		return "*"
	}
	return strings.Join(names, ",")
}

// databaseFromPath parses {databaseId}, authorizes access, and loads the row.
func (h *Handler) databaseFromPath(w http.ResponseWriter, r *http.Request) (generated.Database, bool) {
	var dbUUID pgtype.UUID
	if err := dbUUID.Scan(chi.URLParam(r, "databaseId")); err != nil {
		writeError(w, http.StatusBadRequest, "invalid database id")
		return generated.Database{}, false
	}
	if !h.canAccessDatabase(r, dbUUID) {
		writeError(w, http.StatusForbidden, "access denied")
		return generated.Database{}, false
	}
	db, err := h.queries.GetDatabase(r.Context(), dbUUID)
	if err != nil {
		writeError(w, http.StatusNotFound, "database not found")
		return generated.Database{}, false
	}
	return db, true
}

// validateConfigRequest validates the schedule and destination (same-project
// invariant) and returns the parsed values.
func (h *Handler) validateConfigRequest(r *http.Request, db generated.Database, req databaseBackupConfigRequest, tz string) (destID pgtype.UUID, keep pgtype.Int4, msg string) {
	if err := destID.Scan(req.DestinationID); err != nil {
		return destID, keep, "invalid destination id"
	}
	dest, err := h.backupDestSvc.Get(r.Context(), destID)
	if err != nil {
		return destID, keep, "destination not found"
	}
	if dest.ProjectID != db.ProjectID {
		return destID, keep, "destination belongs to a different project"
	}
	if strings.TrimSpace(req.Schedule) == "" {
		return destID, keep, "schedule is required"
	}
	if err := cronspec.ValidateTimezone(tz); err != nil {
		return destID, keep, err.Error()
	}
	if _, err := cronspec.Parse(strings.TrimSpace(req.Schedule), tz); err != nil {
		return destID, keep, "invalid cron schedule"
	}
	if req.KeepLatest != nil {
		if *req.KeepLatest < 1 {
			return destID, keep, "keep_latest must be at least 1"
		}
		keep = pgtype.Int4{Int32: *req.KeepLatest, Valid: true}
	}
	return destID, keep, ""
}

// ListDatabaseBackupConfigs returns a database's backup configurations.
//
//apidoc:tag databases/volumes-backups
//apidoc:title Get Configs
//apidoc:order 2
func (h *Handler) ListDatabaseBackupConfigs(w http.ResponseWriter, r *http.Request) {
	db, ok := h.databaseFromPath(w, r)
	if !ok {
		return
	}
	rows, err := h.queries.ListDatabaseBackupConfigs(r.Context(), db.ID)
	if err != nil {
		writeServerError(w, r, "failed to list backup configs", err)
		return
	}
	resp := make([]databaseBackupConfigResponse, 0, len(rows))
	for _, c := range rows {
		resp = append(resp, toBackupConfigResponse(c))
	}
	writeJSON(w, http.StatusOK, resp)
}

// CreateDatabaseBackupConfig creates a scheduled backup configuration.
//
//apidoc:tag databases/volumes-backups
//apidoc:title Create Config
//apidoc:order 3
func (h *Handler) CreateDatabaseBackupConfig(w http.ResponseWriter, r *http.Request) {
	db, ok := h.databaseFromPath(w, r)
	if !ok {
		return
	}
	if !databaseBackupEnabled(db) {
		writeError(w, http.StatusBadRequest, "backups are not supported for this database")
		return
	}
	var req databaseBackupConfigRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	tz := timezoneOr(req.Timezone, cronspec.DefaultTimezone)
	destID, keep, msg := h.validateConfigRequest(r, db, req, tz)
	if msg != "" {
		writeError(w, http.StatusBadRequest, msg)
		return
	}
	enabled := true
	if req.Enabled != nil {
		enabled = *req.Enabled
	}

	cfg, err := h.queries.CreateDatabaseBackupConfig(r.Context(), generated.CreateDatabaseBackupConfigParams{
		DatabaseID:     db.ID,
		DestinationID:  destID,
		Prefix:         strings.TrimSpace(req.Prefix),
		Schedule:       strings.TrimSpace(req.Schedule),
		KeepLatest:     keep,
		Enabled:        enabled,
		TargetDatabase: req.targetDatabase(),
		Timezone:       tz,
	})
	if err != nil {
		writeServerError(w, r, "failed to create backup config", err)
		return
	}
	h.audit(r, "create_backup_config", "database", uuidToString(db.ID), map[string]any{"config_id": uuidToString(cfg.ID)})
	writeJSON(w, http.StatusCreated, toBackupConfigResponse(cfg))
}

// configInDatabase parses {configId}, loads it, and verifies it belongs to db.
func (h *Handler) configInDatabase(w http.ResponseWriter, r *http.Request, db generated.Database) (generated.DatabaseBackupConfig, bool) {
	var cfgUUID pgtype.UUID
	if err := cfgUUID.Scan(chi.URLParam(r, "configId")); err != nil {
		writeError(w, http.StatusBadRequest, "invalid config id")
		return generated.DatabaseBackupConfig{}, false
	}
	cfg, err := h.queries.GetDatabaseBackupConfig(r.Context(), cfgUUID)
	if err != nil {
		writeError(w, http.StatusNotFound, "backup config not found")
		return generated.DatabaseBackupConfig{}, false
	}
	if cfg.DatabaseID != db.ID {
		writeError(w, http.StatusNotFound, "backup config not found")
		return generated.DatabaseBackupConfig{}, false
	}
	return cfg, true
}

// UpdateDatabaseBackupConfig updates a scheduled backup configuration.
//
//apidoc:tag databases/volumes-backups
//apidoc:title Update Config
//apidoc:order 4
func (h *Handler) UpdateDatabaseBackupConfig(w http.ResponseWriter, r *http.Request) {
	db, ok := h.databaseFromPath(w, r)
	if !ok {
		return
	}
	cfg, ok := h.configInDatabase(w, r, db)
	if !ok {
		return
	}
	var req databaseBackupConfigRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	tz := timezoneOr(req.Timezone, cfg.Timezone)
	destID, keep, msg := h.validateConfigRequest(r, db, req, tz)
	if msg != "" {
		writeError(w, http.StatusBadRequest, msg)
		return
	}
	enabled := cfg.Enabled
	if req.Enabled != nil {
		enabled = *req.Enabled
	}

	updated, err := h.queries.UpdateDatabaseBackupConfig(r.Context(), generated.UpdateDatabaseBackupConfigParams{
		ID:             cfg.ID,
		DestinationID:  destID,
		Prefix:         strings.TrimSpace(req.Prefix),
		Schedule:       strings.TrimSpace(req.Schedule),
		KeepLatest:     keep,
		Enabled:        enabled,
		TargetDatabase: req.targetDatabase(),
		Timezone:       tz,
	})
	if err != nil {
		writeServerError(w, r, "failed to update backup config", err)
		return
	}
	h.audit(r, "update_backup_config", "database", uuidToString(db.ID), map[string]any{"config_id": uuidToString(cfg.ID)})
	writeJSON(w, http.StatusOK, toBackupConfigResponse(updated))
}

// DeleteDatabaseBackupConfig deletes a config and its produced backups (rows +
// local files + remote objects), so nothing is orphaned in the destination.
//
//apidoc:tag databases/volumes-backups
//apidoc:title Delete Config
//apidoc:order 5
func (h *Handler) DeleteDatabaseBackupConfig(w http.ResponseWriter, r *http.Request) {
	db, ok := h.databaseFromPath(w, r)
	if !ok {
		return
	}
	cfg, ok := h.configInDatabase(w, r, db)
	if !ok {
		return
	}

	// Remove this config's backups first (local + remote + row). Recorded
	// locations would survive the config, but runs written before 000061 have
	// only config_id to resolve their destination and it is nulled on delete.
	runs, err := h.queries.ListDatabaseBackupsByConfig(r.Context(), generated.ListDatabaseBackupsByConfigParams{
		BackupConfigID: cfg.ID,
		Limit:          1000,
	})
	if err == nil {
		for _, run := range runs {
			if delErr := h.dbService.DeleteBackup(r.Context(), db.ID, run.ID); delErr != nil {
				// Best-effort: log via audit-less path is not available; continue.
				continue
			}
		}
	}

	if err := h.queries.DeleteDatabaseBackupConfig(r.Context(), cfg.ID); err != nil {
		writeServerError(w, r, "failed to delete backup config", err)
		return
	}
	h.audit(r, "delete_backup_config", "database", uuidToString(db.ID), map[string]any{"config_id": uuidToString(cfg.ID)})
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

// RunDatabaseBackupConfig triggers an immediate backup for a config.
//
//apidoc:tag databases/volumes-backups
//apidoc:title Run Config Backup
//apidoc:order 6
func (h *Handler) RunDatabaseBackupConfig(w http.ResponseWriter, r *http.Request) {
	db, ok := h.databaseFromPath(w, r)
	if !ok {
		return
	}
	cfg, ok := h.configInDatabase(w, r, db)
	if !ok {
		return
	}
	if db.Status != status.DatabaseRunning {
		writeError(w, http.StatusConflict, "database must be running to back up")
		return
	}
	// Reject overlapping backups so a double-click doesn't spawn two runs.
	if recent, err := h.queries.ListDatabaseBackups(r.Context(), generated.ListDatabaseBackupsParams{DatabaseID: db.ID, Limit: 5}); err == nil {
		for _, b := range recent {
			if b.Status == "running" {
				writeError(w, http.StatusConflict, "a backup is already in progress")
				return
			}
		}
	}

	payload, err := json.Marshal(map[string]any{
		"database_id":      uuidToString(db.ID),
		"backup_config_id": uuidToString(cfg.ID),
	})
	if err != nil {
		writeServerError(w, r, "failed to create backup task", err)
		return
	}
	if _, err := h.asynq.Enqueue(asynq.NewTask(worker.TypeBackupDB, payload), asynq.Queue("default"), asynq.MaxRetry(1)); err != nil {
		writeServerError(w, r, "failed to enqueue backup task", err)
		return
	}
	h.audit(r, "run_backup_config", "database", uuidToString(db.ID), map[string]any{"config_id": uuidToString(cfg.ID)})
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "running"})
}

type projectBackupActivityResponse struct {
	databaseBackupResponse
	Kind         string  `json:"kind"` // "database" | "volume"
	ResourceID   string  `json:"resource_id"`
	ResourceName string  `json:"resource_name"`
	AppName      *string `json:"app_name,omitempty"` // volume rows only: the owning application's name
}

// ListProjectBackups returns recent backup runs across all of a project's
// databases AND application volumes (newest first) for the project
// Backups-tab activity summary.
//
//apidoc:tag projects/backup-destinations
//apidoc:title Get Recent Backups
//apidoc:order 7
func (h *Handler) ListProjectBackups(w http.ResponseWriter, r *http.Request) {
	projectUUID, ok := h.projectFromPath(w, r)
	if !ok {
		return
	}
	rows, err := h.queries.ListProjectBackupActivity(r.Context(), generated.ListProjectBackupActivityParams{
		ProjectID: projectUUID,
		Limit:     50,
	})
	if err != nil {
		writeServerError(w, r, "failed to list backups", err)
		return
	}
	resp := make([]projectBackupActivityResponse, 0, len(rows))
	for _, b := range rows {
		item := projectBackupActivityResponse{
			databaseBackupResponse: databaseBackupResponse{
				ID:        uuidToString(b.ID),
				Status:    b.Status,
				SizeBytes: b.SizeBytes,
				HasRemote: b.RemoteKey.Valid,
				Log:       b.Log,
				StartedAt: b.StartedAt.Time,
			},
			Kind:         b.Kind,
			ResourceID:   uuidToString(b.ResourceID),
			ResourceName: b.ResourceName,
		}
		if b.AppName.Valid {
			item.AppName = &b.AppName.String
		}
		if b.RemoteKey.Valid {
			item.RemoteKey = b.RemoteKey.String
		}
		if b.BackupConfigID.Valid {
			s := uuidToString(b.BackupConfigID)
			item.ConfigID = &s
		}
		if b.FinishedAt.Valid {
			t := b.FinishedAt.Time
			item.FinishedAt = &t
		}
		if b.Error.Valid {
			item.Error = b.Error.String
		}
		resp = append(resp, item)
	}
	writeJSON(w, http.StatusOK, resp)
}
