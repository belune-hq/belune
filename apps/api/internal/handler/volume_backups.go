package handler

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/hibiken/asynq"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/weiliang79/belune/internal/pkg/cronspec"
	"github.com/weiliang79/belune/internal/store/generated"
	"github.com/weiliang79/belune/internal/worker"
)

// resolveVolumeForBackup parses the applicationId + volumeId path params, checks
// access, loads the volume, and verifies it belongs to the application. On any
// failure it writes the response and returns ok=false.
func (h *Handler) resolveVolumeForBackup(w http.ResponseWriter, r *http.Request) (generated.ApplicationVolume, pgtype.UUID, bool) {
	var appUUID pgtype.UUID
	if err := appUUID.Scan(chi.URLParam(r, "applicationId")); err != nil {
		writeError(w, http.StatusBadRequest, "invalid application id")
		return generated.ApplicationVolume{}, appUUID, false
	}
	var volUUID pgtype.UUID
	if err := volUUID.Scan(chi.URLParam(r, "volumeId")); err != nil {
		writeError(w, http.StatusBadRequest, "invalid volume id")
		return generated.ApplicationVolume{}, appUUID, false
	}
	if !h.canAccessApplication(r, appUUID) {
		writeError(w, http.StatusForbidden, "access denied")
		return generated.ApplicationVolume{}, appUUID, false
	}
	vol, err := h.queries.GetApplicationVolume(r.Context(), volUUID)
	if err != nil {
		writeError(w, http.StatusNotFound, "volume not found")
		return generated.ApplicationVolume{}, appUUID, false
	}
	if vol.ApplicationID != appUUID {
		writeError(w, http.StatusNotFound, "volume not found")
		return generated.ApplicationVolume{}, appUUID, false
	}
	return vol, appUUID, true
}

type volumeBackupConfigResponse struct {
	ID          string     `json:"id"`
	VolumeID    string     `json:"application_volume_id"`
	Destination string     `json:"destination_id"`
	Prefix      string     `json:"prefix"`
	Schedule    string     `json:"schedule"`
	Timezone    string     `json:"timezone"`
	KeepLatest  *int32     `json:"keep_latest,omitempty"`
	Enabled     bool       `json:"enabled"`
	Quiesce     bool       `json:"quiesce"`
	LastRunAt   *time.Time `json:"last_run_at,omitempty"`
	CreatedAt   time.Time  `json:"created_at"`
}

func toVolumeBackupConfigResponse(c generated.ApplicationVolumeBackupConfig) volumeBackupConfigResponse {
	resp := volumeBackupConfigResponse{
		ID:          uuidToString(c.ID),
		VolumeID:    uuidToString(c.ApplicationVolumeID),
		Destination: uuidToString(c.DestinationID),
		Prefix:      c.Prefix,
		Schedule:    c.Schedule,
		Timezone:    c.Timezone,
		Enabled:     c.Enabled,
		Quiesce:     c.Quiesce,
		CreatedAt:   c.CreatedAt.Time,
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

// ListBackupConfigsForVolume lists the backup configs attached to ONE volume.
// Named for its scope rather than its table, so it reads against its
// application-wide sibling below — the pair differs only by how much it covers.
//
//apidoc:tag applications/volumes-backups
//apidoc:title Get Configs for a Volume
//apidoc:order 4
func (h *Handler) ListBackupConfigsForVolume(w http.ResponseWriter, r *http.Request) {
	vol, _, ok := h.resolveVolumeForBackup(w, r)
	if !ok {
		return
	}
	configs, err := h.queries.ListApplicationVolumeBackupConfigs(r.Context(), vol.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list backup configs")
		return
	}
	out := make([]volumeBackupConfigResponse, 0, len(configs))
	for _, c := range configs {
		out = append(out, toVolumeBackupConfigResponse(c))
	}
	writeJSON(w, http.StatusOK, out)
}

// appVolumeBackupConfigResponse is a config plus the volume it belongs to, for
// the application-wide backup config list (across all of an app's volumes).
type appVolumeBackupConfigResponse struct {
	volumeBackupConfigResponse
	VolumeName string `json:"volume_name"`
	MountPath  string `json:"mount_path"`
}

// ListBackupConfigsForApplication lists every volume backup config across ALL of
// an application's volumes, so the Mounts tab can render a single list.
//
// ⚠️ "Volume" stays out of both names on purpose. An application's only backup
// configs are volume ones (the other backup-config table is
// database_backup_configs, and databases are project-scoped), so the word adds
// nothing here — but dropping it from one name and not the other would make the
// pair read as two concepts rather than two scopes.
//
//apidoc:tag applications/volumes-backups
//apidoc:title Get Configs for an App
//apidoc:order 3
func (h *Handler) ListBackupConfigsForApplication(w http.ResponseWriter, r *http.Request) {
	var appUUID pgtype.UUID
	if err := appUUID.Scan(chi.URLParam(r, "applicationId")); err != nil {
		writeError(w, http.StatusBadRequest, "invalid application id")
		return
	}
	if !h.canAccessApplication(r, appUUID) {
		writeError(w, http.StatusForbidden, "access denied")
		return
	}
	rows, err := h.queries.ListApplicationVolumeBackupConfigsForApp(r.Context(), appUUID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list backup configs")
		return
	}
	out := make([]appVolumeBackupConfigResponse, 0, len(rows))
	for _, row := range rows {
		resp := appVolumeBackupConfigResponse{
			volumeBackupConfigResponse: toVolumeBackupConfigResponse(generated.ApplicationVolumeBackupConfig{
				ID:                  row.ID,
				ApplicationVolumeID: row.ApplicationVolumeID,
				DestinationID:       row.DestinationID,
				Prefix:              row.Prefix,
				Schedule:            row.Schedule,
				Timezone:            row.Timezone,
				KeepLatest:          row.KeepLatest,
				Enabled:             row.Enabled,
				Quiesce:             row.Quiesce,
				LastRunAt:           row.LastRunAt,
				CreatedAt:           row.CreatedAt,
				UpdatedAt:           row.UpdatedAt,
			}),
			VolumeName: row.VolumeName,
			MountPath:  row.VolumeMountPath,
		}
		out = append(out, resp)
	}
	writeJSON(w, http.StatusOK, out)
}

type volumeBackupConfigRequest struct {
	DestinationID string `json:"destination_id"`
	Prefix        string `json:"prefix"`
	Schedule      string `json:"schedule"`
	// Omitted means UTC on create and the stored zone on update (see
	// databaseBackupConfigRequest.Timezone).
	Timezone   string `json:"timezone"`
	KeepLatest *int32 `json:"keep_latest"`
	Enabled    *bool  `json:"enabled"`
	Quiesce    bool   `json:"quiesce"`
}

// validateVolumeBackupSchedule reports whether a config request is well-formed.
// An empty schedule means manual-only (no scheduled runs) and is allowed; a
// non-empty schedule must be a valid cron expression. keep_latest, when set,
// must be positive.
func validateVolumeBackupSchedule(req volumeBackupConfigRequest, tz string) (string, bool) {
	if err := cronspec.ValidateTimezone(tz); err != nil {
		return err.Error(), false
	}
	if req.Schedule != "" {
		if _, err := cronspec.Parse(req.Schedule, tz); err != nil {
			return "invalid cron schedule", false
		}
	}
	if req.KeepLatest != nil && *req.KeepLatest <= 0 {
		return "keep_latest must be a positive number", false
	}
	return "", true
}

//apidoc:tag applications/volumes-backups
//apidoc:title Create Config
//apidoc:order 4
func (h *Handler) CreateVolumeBackupConfig(w http.ResponseWriter, r *http.Request) {
	vol, appUUID, ok := h.resolveVolumeForBackup(w, r)
	if !ok {
		return
	}

	var req volumeBackupConfigRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	tz := timezoneOr(req.Timezone, cronspec.DefaultTimezone)
	if msg, ok := validateVolumeBackupSchedule(req, tz); !ok {
		writeError(w, http.StatusBadRequest, msg)
		return
	}
	var destUUID pgtype.UUID
	if err := destUUID.Scan(req.DestinationID); err != nil {
		writeError(w, http.StatusBadRequest, "invalid destination_id")
		return
	}
	// The destination must belong to the same project as the application.
	if !h.destinationInProjectForApp(r, destUUID, appUUID) {
		writeError(w, http.StatusBadRequest, "destination does not belong to this project")
		return
	}

	enabled := true
	if req.Enabled != nil {
		enabled = *req.Enabled
	}
	cfg, err := h.queries.CreateApplicationVolumeBackupConfig(r.Context(), generated.CreateApplicationVolumeBackupConfigParams{
		ApplicationVolumeID: vol.ID,
		DestinationID:       destUUID,
		Prefix:              req.Prefix,
		Schedule:            req.Schedule,
		KeepLatest:          nullableInt4(req.KeepLatest),
		Enabled:             enabled,
		Quiesce:             req.Quiesce,
		Timezone:            tz,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to create backup config")
		return
	}
	h.audit(r, "create_volume_backup_config", "application_volume_backup_config", uuidToString(cfg.ID), map[string]any{
		"application_volume_id": uuidToString(vol.ID),
	})
	writeJSON(w, http.StatusCreated, toVolumeBackupConfigResponse(cfg))
}

//apidoc:tag applications/volumes-backups
//apidoc:title Update Config
//apidoc:order 5
func (h *Handler) UpdateVolumeBackupConfig(w http.ResponseWriter, r *http.Request) {
	vol, appUUID, ok := h.resolveVolumeForBackup(w, r)
	if !ok {
		return
	}
	cfg, ok := h.configForVolume(w, r, vol.ID)
	if !ok {
		return
	}

	var req volumeBackupConfigRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	tz := timezoneOr(req.Timezone, cfg.Timezone)
	if msg, ok := validateVolumeBackupSchedule(req, tz); !ok {
		writeError(w, http.StatusBadRequest, msg)
		return
	}
	var destUUID pgtype.UUID
	if err := destUUID.Scan(req.DestinationID); err != nil {
		writeError(w, http.StatusBadRequest, "invalid destination_id")
		return
	}
	if !h.destinationInProjectForApp(r, destUUID, appUUID) {
		writeError(w, http.StatusBadRequest, "destination does not belong to this project")
		return
	}
	enabled := true
	if req.Enabled != nil {
		enabled = *req.Enabled
	}
	updated, err := h.queries.UpdateApplicationVolumeBackupConfig(r.Context(), generated.UpdateApplicationVolumeBackupConfigParams{
		ID:            cfg.ID,
		DestinationID: destUUID,
		Prefix:        req.Prefix,
		Schedule:      req.Schedule,
		KeepLatest:    nullableInt4(req.KeepLatest),
		Enabled:       enabled,
		Quiesce:       req.Quiesce,
		Timezone:      tz,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to update backup config")
		return
	}
	h.audit(r, "update_volume_backup_config", "application_volume_backup_config", uuidToString(cfg.ID), nil)
	writeJSON(w, http.StatusOK, toVolumeBackupConfigResponse(updated))
}

//apidoc:tag applications/volumes-backups
//apidoc:title Delete Config
//apidoc:order 6
func (h *Handler) DeleteVolumeBackupConfig(w http.ResponseWriter, r *http.Request) {
	vol, _, ok := h.resolveVolumeForBackup(w, r)
	if !ok {
		return
	}
	cfg, ok := h.configForVolume(w, r, vol.ID)
	if !ok {
		return
	}

	// Remove this config's backup objects before dropping the config, since a
	// run written before locations existed has only the config to resolve its
	// destination. Each run is routed to where it was actually written, not to
	// wherever the config points now.
	if h.backupDestSvc != nil {
		runs, _ := h.queries.ListApplicationVolumeBackups(r.Context(), generated.ListApplicationVolumeBackupsParams{
			ApplicationVolumeID: vol.ID,
			Limit:               1000,
		})
		var owned []generated.ApplicationVolumeBackup
		for _, b := range runs {
			if b.BackupConfigID.Valid && b.BackupConfigID == cfg.ID {
				owned = append(owned, b)
			}
		}
		if err := h.backupDestSvc.PurgeVolumeBackupObjects(r.Context(), owned, cfg.ID); err != nil {
			writeError(w, http.StatusInternalServerError, "failed to remove backup objects from destination")
			return
		}
	}

	if err := h.queries.DeleteApplicationVolumeBackupConfig(r.Context(), cfg.ID); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to delete backup config")
		return
	}
	h.audit(r, "delete_volume_backup_config", "application_volume_backup_config", uuidToString(cfg.ID), nil)
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

// RunVolumeBackupConfig enqueues a manual backup of the volume using a config.
//
//apidoc:tag applications/volumes-backups
//apidoc:title Run Config Backup
//apidoc:order 8
func (h *Handler) RunVolumeBackupConfig(w http.ResponseWriter, r *http.Request) {
	vol, _, ok := h.resolveVolumeForBackup(w, r)
	if !ok {
		return
	}
	cfg, ok := h.configForVolume(w, r, vol.ID)
	if !ok {
		return
	}

	payload, _ := json.Marshal(map[string]string{
		"application_volume_id": uuidToString(vol.ID),
		"backup_config_id":      uuidToString(cfg.ID),
	})
	if _, err := h.asynq.Enqueue(asynq.NewTask(worker.TypeBackupVolume, payload), asynq.Queue("default"), asynq.MaxRetry(1)); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to enqueue backup")
		return
	}
	h.audit(r, "run_volume_backup", "application_volume", uuidToString(vol.ID), map[string]any{"config_id": uuidToString(cfg.ID)})
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "started"})
}

type volumeBackupResponse struct {
	ID         string     `json:"id"`
	Status     string     `json:"status"`
	SizeBytes  int64      `json:"size_bytes"`
	HasRemote  bool       `json:"has_remote"`
	ConfigID   string     `json:"config_id,omitempty"`
	StartedAt  time.Time  `json:"started_at"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
	Error      string     `json:"error,omitempty"`
	Log        string     `json:"log,omitempty"`
}

//apidoc:tag applications/volumes-backups
//apidoc:title Get Backups
//apidoc:order 9
func (h *Handler) ListVolumeBackups(w http.ResponseWriter, r *http.Request) {
	vol, _, ok := h.resolveVolumeForBackup(w, r)
	if !ok {
		return
	}
	runs, err := h.queries.ListApplicationVolumeBackups(r.Context(), generated.ListApplicationVolumeBackupsParams{
		ApplicationVolumeID: vol.ID,
		Limit:               50,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list backups")
		return
	}
	out := make([]volumeBackupResponse, 0, len(runs))
	for _, b := range runs {
		resp := volumeBackupResponse{
			ID:        uuidToString(b.ID),
			Status:    b.Status,
			SizeBytes: b.SizeBytes,
			HasRemote: b.RemoteKey.Valid,
			StartedAt: b.StartedAt.Time,
			Error:     b.Error.String,
			Log:       b.Log.String,
		}
		if b.BackupConfigID.Valid {
			resp.ConfigID = uuidToString(b.BackupConfigID)
		}
		if b.FinishedAt.Valid {
			t := b.FinishedAt.Time
			resp.FinishedAt = &t
		}
		out = append(out, resp)
	}
	writeJSON(w, http.StatusOK, out)
}

// RestoreVolumeBackup enqueues an in-app restore of a backup into its volume.
//
//apidoc:tag applications/volumes-backups
func (h *Handler) RestoreVolumeBackup(w http.ResponseWriter, r *http.Request) {
	vol, _, ok := h.resolveVolumeForBackup(w, r)
	if !ok {
		return
	}
	var backupUUID pgtype.UUID
	if err := backupUUID.Scan(chi.URLParam(r, "backupId")); err != nil {
		writeError(w, http.StatusBadRequest, "invalid backup id")
		return
	}
	bk, err := h.queries.GetApplicationVolumeBackup(r.Context(), backupUUID)
	if err != nil {
		writeError(w, http.StatusNotFound, "backup not found")
		return
	}
	if bk.ApplicationVolumeID != vol.ID {
		writeError(w, http.StatusNotFound, "backup not found")
		return
	}
	if bk.Status != "succeeded" {
		writeError(w, http.StatusBadRequest, "only a succeeded backup can be restored")
		return
	}

	payload, _ := json.Marshal(map[string]string{
		"application_volume_id": uuidToString(vol.ID),
		"backup_id":             uuidToString(bk.ID),
	})
	if _, err := h.asynq.Enqueue(asynq.NewTask(worker.TypeRestoreVolume, payload), asynq.Queue("critical")); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to enqueue restore")
		return
	}
	h.audit(r, "restore_volume", "application_volume", uuidToString(vol.ID), map[string]any{"backup_id": uuidToString(bk.ID)})
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "started"})
}

type volumeRestoreResponse struct {
	ID         string     `json:"id"`
	BackupID   string     `json:"backup_id,omitempty"`
	Status     string     `json:"status"`
	StartedAt  time.Time  `json:"started_at"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
	Error      string     `json:"error,omitempty"`
	Log        string     `json:"log,omitempty"`
}

// ListVolumeRestores returns the recent restore runs for a volume.
//
//apidoc:tag applications/volumes-backups
//apidoc:title Get Restores
//apidoc:order 10
func (h *Handler) ListVolumeRestores(w http.ResponseWriter, r *http.Request) {
	vol, _, ok := h.resolveVolumeForBackup(w, r)
	if !ok {
		return
	}
	runs, err := h.queries.ListApplicationVolumeRestores(r.Context(), generated.ListApplicationVolumeRestoresParams{
		ApplicationVolumeID: vol.ID,
		Limit:               50,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list restores")
		return
	}
	out := make([]volumeRestoreResponse, 0, len(runs))
	for _, b := range runs {
		resp := volumeRestoreResponse{
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

// configForVolume loads the {configId} path param and verifies it belongs to the
// volume. Writes the response and returns ok=false on failure.
func (h *Handler) configForVolume(w http.ResponseWriter, r *http.Request, volumeID pgtype.UUID) (generated.ApplicationVolumeBackupConfig, bool) {
	var cfgUUID pgtype.UUID
	if err := cfgUUID.Scan(chi.URLParam(r, "configId")); err != nil {
		writeError(w, http.StatusBadRequest, "invalid config id")
		return generated.ApplicationVolumeBackupConfig{}, false
	}
	cfg, err := h.queries.GetApplicationVolumeBackupConfig(r.Context(), cfgUUID)
	if err != nil {
		writeError(w, http.StatusNotFound, "backup config not found")
		return generated.ApplicationVolumeBackupConfig{}, false
	}
	if cfg.ApplicationVolumeID != volumeID {
		writeError(w, http.StatusNotFound, "backup config not found")
		return generated.ApplicationVolumeBackupConfig{}, false
	}
	return cfg, true
}

// destinationInProjectForApp verifies the destination belongs to the same project
// as the application (destinations are project-scoped).
func (h *Handler) destinationInProjectForApp(r *http.Request, destID, appID pgtype.UUID) bool {
	dest, err := h.queries.GetBackupDestination(r.Context(), destID)
	if err != nil {
		return false
	}
	app, err := h.queries.GetApplication(r.Context(), appID)
	if err != nil {
		return false
	}
	return dest.ProjectID == app.ProjectID
}

func nullableInt4(v *int32) pgtype.Int4 {
	if v == nil {
		return pgtype.Int4{}
	}
	return pgtype.Int4{Int32: *v, Valid: true}
}
