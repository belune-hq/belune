package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/hibiken/asynq"
	"github.com/jackc/pgx/v5"

	"github.com/belune-hq/belune/internal/config"
	"github.com/belune-hq/belune/internal/service/backup"
	"github.com/belune-hq/belune/internal/store/generated"
	"github.com/belune-hq/belune/internal/worker"
)

type backupRunView struct {
	ID         string  `json:"id"`
	StartedAt  string  `json:"started_at"`
	FinishedAt *string `json:"finished_at"`
	Status     string  `json:"status"`
	RemoteKey  *string `json:"remote_key"`
	SizeBytes  int64   `json:"size_bytes"`
	Error      *string `json:"error"`
	Log        string  `json:"log"`
	Encrypted  bool    `json:"encrypted"`
}

// backupRemoteView exposes the non-secret remote-storage config so the admin UI
// can show where control-plane backups are uploaded, and edit it. Credentials
// (access_key/secret_key) are never returned — write-only, same convention as
// project backup destinations.
type backupRemoteView struct {
	Endpoint string `json:"endpoint"`
	Region   string `json:"region"`
	Bucket   string `json:"bucket"`
	Prefix   string `json:"prefix"`
	UseSSL   bool   `json:"use_ssl"`
}

func toBackupRemoteView(rc backup.RemoteConfig) backupRemoteView {
	return backupRemoteView{
		Endpoint: rc.Endpoint,
		Region:   rc.Region,
		Bucket:   rc.Bucket,
		Prefix:   rc.Prefix,
		UseSSL:   rc.UseSSL,
	}
}

type backupStatusView struct {
	LastSucceededAt     *string           `json:"last_succeeded_at"`
	LastAttemptedAt     *string           `json:"last_attempted_at"`
	LastError           *string           `json:"last_error"`
	RemoteEnabled       bool              `json:"remote_enabled"`
	Remote              *backupRemoteView `json:"remote"`
	Retention           map[string]any    `json:"retention"`
	EncryptionEnabled   bool              `json:"encryption_enabled"`
	EncryptionRecipient *string           `json:"encryption_recipient"`
}

// resolveEncryptionRecipient mirrors worker.ageEncryptFile's recipient
// resolution (BACKUP_ENCRYPTION_KEY is either a literal age1... public key or
// a path to a file containing one). The recipient is a PUBLIC key — safe to
// display read-only on the dashboard. Editing it stays host-only by design
// (the archive contains .env itself; changing what it encrypts to must
// require host access, same boundary as restore).
func resolveEncryptionRecipient(keyOrPath string) string {
	if data, err := os.ReadFile(keyOrPath); err == nil {
		return strings.TrimSpace(string(data))
	}
	return keyOrPath
}

// ListBackupRuns returns backup runs most-recent-first, paginated via
// limit/offset query params (see parsePagination).
//
//apidoc:tag platform/backups
//apidoc:title Get Runs
//apidoc:order 1
func (h *Handler) ListBackupRuns(w http.ResponseWriter, r *http.Request) {
	limit, offset := parsePagination(r)
	runs, err := h.queries.ListBackupRuns(r.Context(), generated.ListBackupRunsParams{
		Limit:  limit,
		Offset: offset,
	})
	if err != nil {
		writeServerError(w, r, "failed to list backup runs", err)
		return
	}

	views := make([]backupRunView, 0, len(runs))
	for _, run := range runs {
		v := backupRunView{
			ID:        uuidToString(run.ID),
			StartedAt: run.StartedAt.Time.Format(time.RFC3339),
			Status:    run.Status,
			SizeBytes: run.SizeBytes,
			Log:       run.Log,
			Encrypted: run.Encrypted,
		}
		if run.FinishedAt.Valid {
			s := run.FinishedAt.Time.Format(time.RFC3339)
			v.FinishedAt = &s
		}
		if run.RemoteKey.Valid {
			v.RemoteKey = &run.RemoteKey.String
		}
		if run.Error.Valid {
			v.Error = &run.Error.String
		}
		views = append(views, v)
	}

	writeJSON(w, http.StatusOK, views)
}

// resolveBackupRetention reads the dashboard-editable retention settings
// fresh, falling back per-key to the .env-configured Config defaults when a
// setting is unset — mirrors the worker's copy of this resolution (see
// worker.resolveBackupRetention), kept separate since the two packages don't
// share a settings-reading helper.
func (h *Handler) resolveBackupRetention(ctx context.Context) (days, count int) {
	days, count = h.cfg.BackupRetainDays, h.cfg.BackupRetainCount
	if s, err := h.queries.GetSetting(ctx, config.SettingControlPlaneBackupRetainDays); err == nil {
		if n, convErr := strconv.Atoi(strings.TrimSpace(s.Value)); convErr == nil && n > 0 {
			days = n
		}
	}
	if s, err := h.queries.GetSetting(ctx, config.SettingControlPlaneBackupRetainCount); err == nil {
		if n, convErr := strconv.Atoi(strings.TrimSpace(s.Value)); convErr == nil && n > 0 {
			count = n
		}
	}
	return days, count
}

// GetBackupStatus returns a summary of the most recent backup activity.
//
//apidoc:tag platform/backups
//apidoc:title Get Remote Status
//apidoc:order 2
func (h *Handler) GetBackupStatus(w http.ResponseWriter, r *http.Request) {
	rc := backup.LoadRemoteConfig(h.cfg)
	retainDays, retainCount := h.resolveBackupRetention(r.Context())
	status := backupStatusView{
		RemoteEnabled: rc.Enabled,
		Retention: map[string]any{
			"days":  retainDays,
			"count": retainCount,
		},
	}
	if rc.Enabled {
		v := toBackupRemoteView(rc)
		status.Remote = &v
	}

	if key := h.cfg.BackupEncryptionKey; key != "" {
		status.EncryptionEnabled = true
		recipient := resolveEncryptionRecipient(key)
		status.EncryptionRecipient = &recipient
	}

	if last, err := h.queries.GetLastBackupRun(r.Context()); err == nil {
		s := last.StartedAt.Time.Format(time.RFC3339)
		status.LastAttemptedAt = &s
		if last.Error.Valid {
			status.LastError = &last.Error.String
		}
	} else if !errors.Is(err, pgx.ErrNoRows) {
		writeServerError(w, r, "failed to query backup status", err)
		return
	}

	if succeeded, err := h.queries.GetLastSucceededBackupRun(r.Context()); err == nil {
		if succeeded.FinishedAt.Valid {
			s := succeeded.FinishedAt.Time.Format(time.RFC3339)
			status.LastSucceededAt = &s
		}
	} else if !errors.Is(err, pgx.ErrNoRows) {
		writeServerError(w, r, "failed to query backup status", err)
		return
	}

	writeJSON(w, http.StatusOK, status)
}

// TriggerBackupRun enqueues a TypeBackupNow task.
//
//apidoc:tag platform/backups
//apidoc:title Trigger Run
//apidoc:order 3
func (h *Handler) TriggerBackupRun(w http.ResponseWriter, r *http.Request) {
	// Reject if a backup is already in progress to avoid concurrent archive writes.
	last, err := h.queries.GetLastBackupRun(r.Context())
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		writeServerError(w, r, "failed to check backup status", err)
		return
	}
	if err == nil && last.Status == "running" {
		writeError(w, http.StatusConflict, "a backup is already in progress")
		return
	}

	task := asynq.NewTask(worker.TypeBackupNow, nil)
	if _, err := h.asynq.Enqueue(task, asynq.Queue("low")); err != nil {
		writeServerError(w, r, "failed to enqueue backup task", err)
		return
	}

	h.audit(r, "backup_triggered", "backup", "", nil)
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "queued"})
}

// TestBackupRemote verifies the currently configured remote storage
// (dashboard-managed file, falling back to .env) is reachable and the bucket
// exists, without mutating anything. Read-only diagnostic for the admin
// Backups tab.
//
//apidoc:tag platform/backups
//apidoc:title Test Remote
//apidoc:order 4
func (h *Handler) TestBackupRemote(w http.ResponseWriter, r *http.Request) {
	if !backup.LoadRemoteConfig(h.cfg).Enabled {
		writeError(w, http.StatusBadRequest,
			"remote storage is disabled — enable it from the Remote Storage card")
		return
	}
	if err := backup.New(h.cfg).Check(r.Context()); err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

type updateBackupRemoteRequest struct {
	Enabled   *bool  `json:"enabled"`
	Endpoint  string `json:"endpoint"`
	Region    string `json:"region"`
	Bucket    string `json:"bucket"`
	Prefix    string `json:"prefix"`
	UseSSL    *bool  `json:"use_ssl"`
	AccessKey string `json:"access_key"`
	SecretKey string `json:"secret_key"`
}

// TestBackupRemoteParams tests the remote-storage values POSTED from the
// Remote Storage form, before they are saved and without requiring Enabled —
// TestBackupRemote can only report on what is already stored, which forced an
// operator to save and enable unvalidated credentials just to find out whether
// they work. Nothing is written, so no audit entry.
//
// Blank credentials fall back to the stored ones (same convention as
// UpdateBackupRemote and the project-level destination test), so editing a
// bucket or endpoint doesn't require re-typing a secret the UI never
// redisplays.
//
//apidoc:tag platform/backups
//apidoc:title Test Remote Configs
//apidoc:order 6
func (h *Handler) TestBackupRemoteParams(w http.ResponseWriter, r *http.Request) {
	var req updateBackupRemoteRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	bucket := strings.TrimSpace(req.Bucket)
	if bucket == "" {
		writeError(w, http.StatusBadRequest, "bucket is required")
		return
	}

	current := backup.LoadRemoteConfig(h.cfg)
	// Provider is left unset: the control-plane remote has no provider concept
	// (it is always S3-compatible) and NewDestinationClient reads only the
	// transport fields. Enabled is ignored on purpose — testing a draft is the
	// point.
	dest := backup.Destination{
		Endpoint:  strings.TrimSpace(req.Endpoint),
		Region:    strings.TrimSpace(req.Region),
		Bucket:    bucket,
		Prefix:    strings.TrimSpace(req.Prefix),
		AccessKey: current.AccessKey,
		SecretKey: current.SecretKey,
		UseSSL:    req.UseSSL == nil || *req.UseSSL,
	}
	if req.AccessKey != "" {
		dest.AccessKey = req.AccessKey
	}
	if req.SecretKey != "" {
		dest.SecretKey = req.SecretKey
	}

	client, err := backup.NewDestinationClient(dest)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	if err := client.Test(r.Context()); err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// UpdateBackupRemote saves the dashboard-managed control-plane remote-storage
// config to cfg.BackupRemoteConfigPath (mode 0600), read fresh by the worker's
// S3 client and by scripts/backup.sh/belune-backup-upload on the next backup —
// no restart needed. Endpoint/region/bucket/prefix/use_ssl/enabled always
// reflect the submitted form state; a blank access_key or secret_key preserves
// the currently stored one (same convention as project backup destinations),
// so the dashboard never has to redisplay a saved secret to let the operator
// edit unrelated fields.
//
//apidoc:tag platform/backups
//apidoc:title Update Remote
//apidoc:order 5
func (h *Handler) UpdateBackupRemote(w http.ResponseWriter, r *http.Request) {
	var req updateBackupRemoteRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	enabled := req.Enabled != nil && *req.Enabled
	bucket := strings.TrimSpace(req.Bucket)
	if enabled && bucket == "" {
		writeError(w, http.StatusBadRequest, "bucket is required to enable remote storage")
		return
	}

	current := backup.LoadRemoteConfig(h.cfg)
	next := backup.RemoteConfig{
		Enabled:   enabled,
		Endpoint:  strings.TrimSpace(req.Endpoint),
		Region:    strings.TrimSpace(req.Region),
		Bucket:    bucket,
		Prefix:    strings.TrimSpace(req.Prefix),
		UseSSL:    req.UseSSL == nil || *req.UseSSL,
		AccessKey: current.AccessKey,
		SecretKey: current.SecretKey,
	}
	if req.AccessKey != "" {
		next.AccessKey = req.AccessKey
	}
	if req.SecretKey != "" {
		next.SecretKey = req.SecretKey
	}

	if err := backup.SaveRemoteConfig(h.cfg.BackupRemoteConfigPath, next); err != nil {
		writeServerError(w, r, "failed to save remote storage config", err)
		return
	}

	h.audit(r, "update_backup_remote", "backup", "", nil)
	v := toBackupRemoteView(next)
	writeJSON(w, http.StatusOK, map[string]any{"status": "saved", "remote": v})
}
