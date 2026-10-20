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
	"github.com/belune-hq/belune/internal/store/generated"
)

// restoreVolumePayload restores an application volume from a previously-recorded
// backup. This is disruptive: the app is stopped, the volume is wiped and
// replaced with the snapshot, then the app is restarted.
type restoreVolumePayload struct {
	ApplicationVolumeID string `json:"application_volume_id"`
	BackupID            string `json:"backup_id"`
}

func (h *TaskHandler) HandleRestoreVolumeTask(ctx context.Context, t *asynq.Task) error {
	var payload restoreVolumePayload
	if err := json.Unmarshal(t.Payload(), &payload); err != nil {
		return fmt.Errorf("unmarshal restore_volume payload: %w", err)
	}

	slog.Info("handling restore_volume task", "volume_id", payload.ApplicationVolumeID, "backup_id", payload.BackupID)

	volID, err := parseUUID(payload.ApplicationVolumeID)
	if err != nil {
		return errors.Join(fmt.Errorf("invalid application_volume_id (permanent): %w", err), asynq.SkipRetry)
	}
	backupID, err := parseUUID(payload.BackupID)
	if err != nil {
		return errors.Join(fmt.Errorf("invalid backup_id (permanent): %w", err), asynq.SkipRetry)
	}

	vol, err := h.Queries.GetApplicationVolume(ctx, volID)
	if err != nil {
		return errors.Join(fmt.Errorf("get volume (permanent): %w", err), asynq.SkipRetry)
	}
	bk, err := h.Queries.GetApplicationVolumeBackup(ctx, backupID)
	if err != nil {
		return errors.Join(fmt.Errorf("get backup (permanent): %w", err), asynq.SkipRetry)
	}
	if bk.ApplicationVolumeID != volID {
		return errors.Join(errors.New("backup does not belong to volume (permanent)"), asynq.SkipRetry)
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

	// Record the restore run so the UI can show progress/outcome.
	run, err := h.Queries.InsertApplicationVolumeRestore(ctx, generated.InsertApplicationVolumeRestoreParams{
		ApplicationVolumeID: volID,
		BackupID:            backupID,
	})
	if err != nil {
		return fmt.Errorf("insert restore run: %w", err)
	}

	lg := &runLog{}
	lg.flush = func(s string) {
		if err := h.Queries.SetApplicationVolumeRestoreLog(ctx, generated.SetApplicationVolumeRestoreLogParams{ID: run.ID, Log: pgtype.Text{String: s, Valid: true}}); err != nil {
			slog.Warn("restore_volume: flush log", "restore_id", formatUUID(run.ID), "error", err)
		}
	}
	lg.step("Volume restore started (volume=%s, path=%s, backup=%s)", vol.Name, vol.MountPath, formatUUID(backupID))

	archivePath, cleanup, err := h.resolveVolumeBackupFile(ctx, bk)
	if err != nil {
		h.failVolumeRestoreLog(ctx, run.ID, fmt.Sprintf("resolve backup file: %v", err), lg)
		h.notifyApplicationOwner(ctx, vol.ApplicationID, appRow.ProjectID, notify.EventVolumeRestoreFailed,
			"Volume restore failed",
			fmt.Sprintf("Restoring volume %s of %s failed: %v", vol.Name, appRow.Name, err))
		return fmt.Errorf("resolve backup file: %w", err)
	}
	defer cleanup()
	lg.step("Backup archive ready")

	if err := h.restoreAppVolume(ctx, rt, volumeName, vol.MountPath, containerName, appRow.Status == "running", archivePath, lg); err != nil {
		h.failVolumeRestoreLog(ctx, run.ID, err.Error(), lg)
		h.notifyApplicationOwner(ctx, vol.ApplicationID, appRow.ProjectID, notify.EventVolumeRestoreFailed,
			"Volume restore failed",
			fmt.Sprintf("Restoring volume %s of %s failed: %s", vol.Name, appRow.Name, err.Error()))
		return err
	}

	lg.step("Restore succeeded")
	h.finaliseVolumeRestore(ctx, run.ID, generated.UpdateApplicationVolumeRestoreParams{
		ID:         run.ID,
		FinishedAt: pgtype.Timestamptz{Time: time.Now(), Valid: true},
		Status:     "succeeded",
		Log:        pgtype.Text{String: lg.String(), Valid: true},
	})
	h.notifyApplicationOwner(ctx, vol.ApplicationID, appRow.ProjectID, notify.EventVolumeRestored,
		"Volume restored",
		fmt.Sprintf("Volume %s of %s was restored from a backup.", vol.Name, appRow.Name))

	slog.Info("volume restored", "volume_id", payload.ApplicationVolumeID, "backup_id", payload.BackupID)
	return nil
}

// resolveVolumeBackupFile returns a readable path to the backup archive,
// downloading the remote object to a temp file when there is no local copy. The
// returned cleanup removes any temp file.
func (h *TaskHandler) resolveVolumeBackupFile(ctx context.Context, bk generated.ApplicationVolumeBackup) (string, func(), error) {
	noop := func() {}
	if bk.LocalPath.Valid {
		if _, err := os.Stat(bk.LocalPath.String); err == nil {
			return bk.LocalPath.String, noop, nil
		}
	}
	if !bk.RemoteKey.Valid {
		return "", noop, errors.New("backup has no local file and no remote copy")
	}
	if h.BackupDestinations == nil {
		return "", noop, errors.New("backup destinations service is not configured")
	}

	// Prefer the destination this backup was recorded as written to; the config
	// is a mutable pointer and may since have been repointed or deleted.
	copy, err := h.clientForRecordedVolumeBackup(ctx, bk.ID)
	if err != nil {
		return "", noop, err
	}
	if copy == nil {
		// No recorded location: written before 000061, or its config is gone.
		if !bk.BackupConfigID.Valid {
			return "", noop, errors.New("backup has no recorded location and no config to resolve its destination")
		}
		client, cErr := h.BackupDestinations.ClientForVolumeBackupConfig(ctx, bk.BackupConfigID)
		if cErr != nil {
			return "", noop, fmt.Errorf("resolve backup destination: %w", cErr)
		}
		copy = &recordedCopy{client: client, key: bk.RemoteKey.String}
	}
	tmp := filepath.Join(os.TempDir(), "belune-volrestore-"+filepath.Base(copy.key))
	cleanup := func() { _ = os.Remove(tmp) }
	if err := copy.client.Download(ctx, copy.key, tmp); err != nil {
		cleanup()
		return "", noop, err
	}
	return tmp, cleanup, nil
}

// restoreAppVolume stops the app (if running), wipes the named volume, untars
// the archive into it, then restarts the app. Stopping is mandatory: restoring
// under a live writer would corrupt the volume.
func (h *TaskHandler) restoreAppVolume(ctx context.Context, rt runtime.ContainerRuntime, volumeName, mountPath, containerName string, appRunning bool, archivePath string, lg *runLog) error {
	helperImage := h.Config.DatabaseBackupHelperImage
	if err := rt.PullImage(ctx, helperImage); err != nil {
		return fmt.Errorf("pull helper image: %w", err)
	}

	f, err := os.Open(archivePath)
	if err != nil {
		return fmt.Errorf("open archive: %w", err)
	}
	defer f.Close()

	if appRunning {
		lg.step("Stopping application for restore")
		if err := rt.StopContainer(ctx, containerName); err != nil {
			slog.Warn("restore volume: stop app (may already be stopped)", "container", containerName, "error", err)
		}
		defer func() {
			if err := rt.StartContainer(ctx, containerName); err != nil {
				slog.Error("restore volume: failed to restart app after restore", "container", containerName, "error", err)
			}
			lg.step("Application restarted")
		}()
	}

	// mountPath is passed as the shell's positional $0 (a parameter, never
	// re-parsed as script text) so it cannot inject shell syntax even though it
	// originates from user-set volume config. `cd "$0"` then operates on the
	// mount dir directly.
	const script = `cd "$0" && find . -mindepth 1 -delete && tar xzf -`
	var stderr bytes.Buffer
	exit, err := rt.RunHelper(ctx, runtime.ContainerConfig{
		Image:   helperImage,
		Cmd:     []string{"sh", "-c", script, mountPath},
		Volumes: map[string]string{volumeName: mountPath},
	}, f, nil, &stderr)
	lg.raw(stderr.String())
	if err != nil {
		return fmt.Errorf("restore helper: %w", err)
	}
	if exit != 0 {
		return errors.Join(fmt.Errorf("restore tar exited %d: %s", exit, strings.TrimSpace(stderr.String())), asynq.SkipRetry)
	}
	lg.step("Volume contents replaced from snapshot")
	return nil
}

func (h *TaskHandler) failVolumeRestoreLog(ctx context.Context, id pgtype.UUID, errMsg string, lg *runLog) {
	slog.Error("volume restore failed", "restore_id", formatUUID(id), "error", errMsg)
	logText := ""
	if lg != nil {
		lg.step("Restore failed: %s", errMsg)
		logText = lg.String()
	}
	h.finaliseVolumeRestore(ctx, id, generated.UpdateApplicationVolumeRestoreParams{
		ID:         id,
		FinishedAt: pgtype.Timestamptz{Time: time.Now(), Valid: true},
		Status:     "failed",
		Error:      pgtype.Text{String: errMsg, Valid: true},
		Log:        pgtype.Text{String: logText, Valid: true},
	})
}

func (h *TaskHandler) finaliseVolumeRestore(ctx context.Context, id pgtype.UUID, params generated.UpdateApplicationVolumeRestoreParams) {
	if !id.Valid {
		return
	}
	if err := h.Queries.UpdateApplicationVolumeRestore(ctx, params); err != nil {
		slog.Warn("restore_volume: failed to update run record", "restore_id", formatUUID(id), "error", err)
	}
}
