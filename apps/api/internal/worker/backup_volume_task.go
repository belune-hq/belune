package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/hibiken/asynq"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/belune-hq/belune/internal/naming"
	"github.com/belune-hq/belune/internal/notify"
	"github.com/belune-hq/belune/internal/runtime"
	"github.com/belune-hq/belune/internal/service/backup"
	"github.com/belune-hq/belune/internal/store/generated"
)

// backupVolumePayload triggers a snapshot of an application's persistent volume.
// MVP is config-based: BackupConfigID is always set (the config carries the
// destination + quiesce option).
type backupVolumePayload struct {
	ApplicationVolumeID string `json:"application_volume_id"`
	BackupConfigID      string `json:"backup_config_id"`
}

// HandleBackupVolumeTask tars an application volume and uploads it to the
// config's project destination. Hot by default (no downtime); when the config's
// quiesce flag is set and the app is running, the container is stopped for the
// duration of the tar to get a consistent snapshot.
func (h *TaskHandler) HandleBackupVolumeTask(ctx context.Context, t *asynq.Task) error {
	var payload backupVolumePayload
	if err := json.Unmarshal(t.Payload(), &payload); err != nil {
		return fmt.Errorf("unmarshal backup_volume payload: %w", err)
	}

	volID, err := parseUUID(payload.ApplicationVolumeID)
	if err != nil {
		return errors.Join(fmt.Errorf("invalid application_volume_id (permanent): %w", err), asynq.SkipRetry)
	}
	vol, err := h.Queries.GetApplicationVolume(ctx, volID)
	if err != nil {
		return errors.Join(fmt.Errorf("get volume (permanent): %w", err), asynq.SkipRetry)
	}

	if payload.BackupConfigID == "" {
		return errors.Join(errors.New("backup_config_id is required (permanent)"), asynq.SkipRetry)
	}
	cid, err := parseUUID(payload.BackupConfigID)
	if err != nil {
		return errors.Join(fmt.Errorf("invalid backup_config_id (permanent): %w", err), asynq.SkipRetry)
	}
	cfg, err := h.Queries.GetApplicationVolumeBackupConfig(ctx, cid)
	if err != nil {
		return errors.Join(fmt.Errorf("get backup config (permanent): %w", err), asynq.SkipRetry)
	}
	if cfg.ApplicationVolumeID != volID {
		return errors.Join(errors.New("backup config does not belong to volume (permanent)"), asynq.SkipRetry)
	}
	if h.BackupDestinations == nil {
		return errors.Join(errors.New("backup destinations service is not configured (permanent)"), asynq.SkipRetry)
	}
	dest, err := h.BackupDestinations.Resolve(ctx, cfg.DestinationID)
	if err != nil {
		return errors.Join(fmt.Errorf("resolve destination (permanent): %w", err), asynq.SkipRetry)
	}
	// A local destination has no bucket/endpoint/credentials to build a client
	// from — NewDestinationClient must never be called for it.
	var destClient *backup.DestinationClient
	if !dest.IsLocal() {
		destClient, err = backup.NewDestinationClient(dest)
		if err != nil {
			return errors.Join(fmt.Errorf("build destination client (permanent): %w", err), asynq.SkipRetry)
		}
	}

	appRow, err := h.Queries.GetApplicationWithProjectSlug(ctx, vol.ApplicationID)
	if err != nil {
		return errors.Join(fmt.Errorf("get application (permanent): %w", err), asynq.SkipRetry)
	}
	appIDStr := formatUUID(vol.ApplicationID)
	containerName := naming.ContainerName(appRow.ProjectSlug, appRow.Slug, appIDStr)

	rt, err := h.Runtimes.For(ctx, appRow.ServerID)
	if err != nil {
		return fmt.Errorf("resolve runtime: %w", err)
	}
	volumeName := naming.AppVolumeName(appIDStr, vol.Name)

	// Record the run up front so the UI shows a "running" backup immediately.
	run, err := h.Queries.InsertApplicationVolumeBackup(ctx, generated.InsertApplicationVolumeBackupParams{
		ApplicationVolumeID: volID,
		BackupConfigID:      cid,
	})
	if err != nil {
		return fmt.Errorf("insert backup run: %w", err)
	}

	lg := &runLog{}
	lg.flush = func(s string) {
		if err := h.Queries.SetApplicationVolumeBackupLog(ctx, generated.SetApplicationVolumeBackupLogParams{ID: run.ID, Log: pgtype.Text{String: s, Valid: true}}); err != nil {
			slog.Warn("backup_volume: flush log", "backup_id", formatUUID(run.ID), "error", err)
		}
	}
	lg.step("Volume backup started (volume=%s, path=%s, quiesce=%t)", vol.Name, vol.MountPath, cfg.Quiesce)

	if err := os.MkdirAll(h.Config.DatabaseBackupDir, 0o755); err != nil {
		h.failVolumeBackupLog(ctx, run.ID, fmt.Sprintf("create backup dir: %v", err), lg)
		return fmt.Errorf("create backup dir: %w", err)
	}
	fileName := fmt.Sprintf("%s-%s-%s.tar.gz", appRow.Slug, vol.Name, time.Now().UTC().Format("20060102T150405Z"))
	localPath := filepath.Join(h.Config.DatabaseBackupDir, fileName)

	f, err := os.Create(localPath)
	if err != nil {
		h.failVolumeBackupLog(ctx, run.ID, fmt.Sprintf("create backup file: %v", err), lg)
		return fmt.Errorf("create backup file: %w", err)
	}

	snapErr := h.snapshotAppVolume(ctx, rt, volumeName, vol.MountPath, containerName, cfg.Quiesce, appRow.Status == "running", f, lg)
	closeErr := f.Close()
	if snapErr != nil {
		_ = os.Remove(localPath)
		h.failVolumeBackupLog(ctx, run.ID, snapErr.Error(), lg)
		h.notifyApplicationOwner(ctx, vol.ApplicationID, appRow.ProjectID, notify.EventVolumeBackupFailed,
			"Volume backup failed",
			fmt.Sprintf("Backing up volume %s of %s failed: %s", vol.Name, appRow.Name, snapErr.Error()))
		return snapErr
	}
	if closeErr != nil {
		_ = os.Remove(localPath)
		h.failVolumeBackupLog(ctx, run.ID, fmt.Sprintf("finalise backup file: %v", closeErr), lg)
		return fmt.Errorf("finalise backup file: %w", closeErr)
	}

	var sizeBytes int64
	if info, statErr := os.Stat(localPath); statErr == nil {
		sizeBytes = info.Size()
	}
	lg.step("Archive written: %s (%d bytes)", fileName, sizeBytes)

	remoteKey := pgtype.Text{}
	localPathText := pgtype.Text{}
	if dest.IsLocal() {
		// Local destination: the staged archive already IS the backup — no
		// upload, no deletion. Record local_path so a later restore/prune finds
		// it (resolveVolumeBackupFile already prefers local_path when valid).
		lg.step("Local destination — keeping archive on-host: %s", localPath)
		localPathText = pgtype.Text{String: localPath, Valid: true}
	} else {
		// Object key = <destination.prefix>/<config.prefix>/<file>.
		key := backup.BuildKey(dest.Prefix, backup.BuildKey(cfg.Prefix, fileName))
		lg.step("Uploading to destination: %s", key)
		if _, upErr := destClient.UploadTo(ctx, localPath, key); upErr != nil {
			_ = os.Remove(localPath)
			h.failVolumeBackupLog(ctx, run.ID, fmt.Sprintf("upload to destination: %v", upErr), lg)
			h.notifyApplicationOwner(ctx, vol.ApplicationID, appRow.ProjectID, notify.EventVolumeBackupFailed,
				"Volume backup failed",
				fmt.Sprintf("Backing up volume %s of %s failed: %v", vol.Name, appRow.Name, upErr))
			return fmt.Errorf("upload to destination: %w", upErr)
		}
		remoteKey = pgtype.Text{String: key, Valid: true}
		// Remote is authoritative; drop the local copy so the server disk
		// doesn't mirror the bucket.
		removeLocalAfterUpload(localPath, lg)
	}

	// Retention: keep only the newest keep_latest backups for this config.
	h.pruneVolumeConfigBackups(ctx, cfg, destClient, lg)
	lg.step("Backup succeeded")

	h.finaliseVolumeBackup(ctx, run.ID, generated.UpdateApplicationVolumeBackupParams{
		ID:         run.ID,
		FinishedAt: pgtype.Timestamptz{Time: time.Now(), Valid: true},
		Status:     "succeeded",
		LocalPath:  localPathText,
		RemoteKey:  remoteKey,
		SizeBytes:  sizeBytes,
		Log:        pgtype.Text{String: lg.String(), Valid: true},
	})
	// Record where this copy went, so restore does not have to re-derive it from
	// the config — which by then may point at a different destination.
	h.recordBackupLocation(ctx, generated.InsertBackupLocationParams{
		VolumeBackupID: run.ID,
		DestinationID:  cfg.DestinationID,
		RemoteKey:      remoteKey,
		LocalPath:      localPathText,
	})
	if err := h.Queries.SetApplicationVolumeBackupConfigLastRun(ctx, generated.SetApplicationVolumeBackupConfigLastRunParams{
		ID:        cid,
		LastRunAt: pgtype.Timestamptz{Time: time.Now(), Valid: true},
	}); err != nil {
		slog.Warn("backup_volume: failed to stamp last_run_at", "config_id", formatUUID(cid), "error", err)
	}

	slog.Info("volume backed up", "volume_id", payload.ApplicationVolumeID, "size_bytes", sizeBytes, "local", dest.IsLocal(), "key", remoteKey.String)
	return nil
}

// snapshotAppVolume tars the named volume's contents (mounted at mountPath in a
// short-lived helper) into f. When quiesce is requested and the app is running,
// the app container is stopped for the tar and restarted afterwards.
func (h *TaskHandler) snapshotAppVolume(ctx context.Context, rt runtime.ContainerRuntime, volumeName, mountPath, containerName string, quiesce, appRunning bool, f *os.File, lg *runLog) error {
	helperImage := h.Config.DatabaseBackupHelperImage
	if err := rt.PullImage(ctx, helperImage); err != nil {
		return fmt.Errorf("pull helper image: %w", err)
	}

	if quiesce && appRunning {
		lg.step("Stopping application for consistent (quiesced) snapshot")
		if err := rt.StopContainer(ctx, containerName); err != nil {
			slog.Warn("snapshot volume: stop app (may already be stopped)", "container", containerName, "error", err)
		}
		defer func() {
			if err := rt.StartContainer(ctx, containerName); err != nil {
				slog.Error("snapshot volume: failed to restart app after backup", "container", containerName, "error", err)
			}
			lg.step("Application restarted")
		}()
	} else if quiesce {
		lg.step("Quiesce requested but app is not running — snapshotting directly")
	}

	var stderr bytes.Buffer
	exit, err := rt.RunHelper(ctx, runtime.ContainerConfig{
		Image:   helperImage,
		Cmd:     []string{"tar", "czf", "-", "-C", mountPath, "."},
		Volumes: map[string]string{volumeName: mountPath},
	}, nil, f, &stderr)
	lg.raw(stderr.String())
	if err != nil {
		return fmt.Errorf("snapshot helper: %w", err)
	}
	if exit != 0 {
		return errors.Join(fmt.Errorf("snapshot tar exited %d: %s", exit, strings.TrimSpace(stderr.String())), asynq.SkipRetry)
	}
	lg.step("Volume snapshot completed")
	return nil
}

func (h *TaskHandler) failVolumeBackupLog(ctx context.Context, id pgtype.UUID, errMsg string, lg *runLog) {
	slog.Error("volume backup failed", "backup_id", formatUUID(id), "error", errMsg)
	logText := ""
	if lg != nil {
		lg.step("Backup failed: %s", errMsg)
		logText = lg.String()
	}
	h.finaliseVolumeBackup(ctx, id, generated.UpdateApplicationVolumeBackupParams{
		ID:         id,
		FinishedAt: pgtype.Timestamptz{Time: time.Now(), Valid: true},
		Status:     "failed",
		Error:      pgtype.Text{String: errMsg, Valid: true},
		Log:        pgtype.Text{String: logText, Valid: true},
	})
}

// notifyApplicationOwner sends the application's owner a notification (bell +
// deep-link) and fans the event out to notification channels once. It is the
// application-scoped sibling of notifyDatabaseOwner. No-op when nothing is wired
// (e.g. tests). Channel dispatch fires independently of the owner's in-app
// alert preferences, matching the deploy/backup/TLS notification paths.
func (h *TaskHandler) notifyApplicationOwner(ctx context.Context, appID, projectID pgtype.UUID, notifType, title, body string) {
	if h.Notifier == nil && h.NotifyChannels == nil {
		return
	}
	link := fmt.Sprintf("/projects/%s/applications/%s", formatUUID(projectID), formatUUID(appID))

	h.dispatchToChannels(ctx, notify.Event{
		Type: notifType, Title: title, Body: body, Link: link, OccurredAt: time.Now(),
	})

	if h.Notifier == nil {
		return
	}
	owner, err := h.Queries.GetApplicationOwnerUserID(ctx, appID)
	if err != nil {
		slog.Warn("notify: could not resolve application owner", "application_id", formatUUID(appID), "error", err)
		return
	}
	h.Notifier.Notify(formatUUID(owner.UserID), notifType, title, body, link)
}

func (h *TaskHandler) finaliseVolumeBackup(ctx context.Context, id pgtype.UUID, params generated.UpdateApplicationVolumeBackupParams) {
	if !id.Valid {
		return
	}
	if err := h.Queries.UpdateApplicationVolumeBackup(ctx, params); err != nil {
		slog.Warn("backup_volume: failed to update run record", "backup_id", formatUUID(id), "error", err)
	}
}

// pruneVolumeConfigBackups keeps the newest keep_latest backups produced by a
// config and deletes the rest (rows + local files + remote objects in the
// config's destination). A NULL/zero keep_latest keeps all. client is nil for
// a local destination, so pruned rows there only lose their local file.
// Best-effort — a failed deletion is logged and does not fail the backup.
func (h *TaskHandler) pruneVolumeConfigBackups(ctx context.Context, cfg generated.ApplicationVolumeBackupConfig, client *backup.DestinationClient, lg *runLog) {
	if !cfg.KeepLatest.Valid || cfg.KeepLatest.Int32 <= 0 {
		return
	}
	keep := int(cfg.KeepLatest.Int32)
	rows, err := h.Queries.ListApplicationVolumeBackupsByConfig(ctx, generated.ListApplicationVolumeBackupsByConfigParams{
		BackupConfigID: cfg.ID,
		Limit:          1000,
	})
	if err != nil {
		slog.Warn("prune volume backups: list", "config_id", formatUUID(cfg.ID), "error", err)
		return
	}
	if len(rows) <= keep {
		return
	}
	for _, b := range rows[keep:] { // ListApplicationVolumeBackupsByConfig is newest-first
		if b.LocalPath.Valid {
			if err := os.Remove(b.LocalPath.String); err != nil && !os.IsNotExist(err) {
				slog.Warn("prune volume backups: remove local", "path", b.LocalPath.String, "error", err)
			}
		}
		if b.RemoteKey.Valid {
			// Delete from where the object actually went; the passed client is
			// the config's current destination, right only for backups written
			// before locations were recorded.
			target, key := client, b.RemoteKey.String
			if recorded, rerr := h.clientForRecordedVolumeBackup(ctx, b.ID); rerr != nil {
				slog.Warn("prune volume backups: resolve recorded destination", "backup_id", formatUUID(b.ID), "error", rerr)
			} else if recorded != nil {
				target, key = recorded.client, recorded.key
			}
			if target != nil {
				if err := target.DeleteFrom(ctx, []string{key}); err != nil {
					slog.Warn("prune volume backups: remove remote", "key", key, "error", err)
				}
			}
		}
		if err := h.Queries.DeleteApplicationVolumeBackup(ctx, b.ID); err != nil {
			slog.Warn("prune volume backups: delete row", "backup_id", formatUUID(b.ID), "error", err)
		}
	}
	lg.step("Pruned old backups, keeping latest %d", keep)
}
