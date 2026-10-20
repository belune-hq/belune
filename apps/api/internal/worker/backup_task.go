package worker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/hibiken/asynq"
	"github.com/jackc/pgx/v5/pgtype"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/belune-hq/belune/internal/pkg/metrics"
	"github.com/belune-hq/belune/internal/pkg/tracing"
	"github.com/belune-hq/belune/internal/store/generated"
)

// HandleBackupNowTask runs a control-plane backup natively (Postgres dump +
// Caddy TLS data + .env, via the Docker API — see backup_control_plane.go) and
// records the result in backup_runs. Triggered by a manual "Run Backup Now"
// click or the in-app cron sweep (backup_schedule_task.go); scripts/backup.sh
// remains the host/offline DR path, producing byte-identical archives so
// restore.sh (unchanged) works against output from either producer. The task
// does not retry on failure — a second run is triggered by the user or the
// next scheduled sweep.
func (h *TaskHandler) HandleBackupNowTask(ctx context.Context, t *asynq.Task) error {
	ctx, span := tracing.Tracer().Start(ctx, "backup.run")
	defer span.End()

	var run generated.BackupRun
	if h.Queries != nil {
		var err error
		run, err = h.Queries.InsertBackupRun(ctx, "worker")
		if err != nil {
			slog.Warn("backup_now: failed to insert run record", "error", err)
		}
		if run.ID.Valid {
			span.SetAttributes(attribute.String("backup.run_id", formatUUID(run.ID)))
		}
	}

	start := time.Now()
	lg := &runLog{}

	if err := os.MkdirAll(h.Config.ControlPlaneBackupDir, 0o755); err != nil {
		return h.failBackupNow(ctx, span, run.ID, lg, fmt.Errorf("create backup dir: %w", err), start)
	}

	// scripts/backup.sh (host CLI) takes the same flock against the same
	// bind-mounted directory, so a concurrent CLI run and worker run can't
	// clobber each other's archive.
	lockPath := filepath.Join(h.Config.ControlPlaneBackupDir, controlPlaneLockName)
	lock, err := acquireFileLock(lockPath)
	if err != nil {
		return h.failBackupNow(ctx, span, run.ID, lg, BackupLockFailure(err, lockPath), start)
	}
	defer lock.release()

	archivePath, buildErr := h.buildControlPlaneArchive(ctx, lg)
	if buildErr != nil {
		return h.failBackupNow(ctx, span, run.ID, lg, buildErr, start)
	}

	var sizeBytes int64
	if info, statErr := os.Stat(archivePath); statErr == nil {
		sizeBytes = info.Size()
	}
	// buildControlPlaneArchive already applied encryption (if configured) and
	// renamed the archive to end in .age — that suffix is the source of truth
	// for whether THIS specific archive needs the private key to restore.
	encrypted := strings.HasSuffix(archivePath, ".age")

	remoteKey := pgtype.Text{}
	destination := "local"
	if h.BackupService != nil && h.BackupService.Enabled() {
		if key, upErr := h.BackupService.Upload(ctx, archivePath); upErr != nil {
			lg.warn("S3 upload failed (keeping local copy): %v", upErr)
			slog.Warn("backup_now: S3 upload failed; keeping local copy", "error", upErr)
		} else {
			remoteKey = pgtype.Text{String: key, Valid: true}
			destination = "remote"
			lg.step("Uploaded to S3: %s", key)
		}
	}
	lg.step("Backup complete: %s", archivePath)

	h.rotateLocalControlPlaneBackups(ctx, lg)
	h.finaliseRun(ctx, run.ID, sizeBytes, remoteKey, encrypted, "", lg.String())

	slog.Info("backup_now: completed", "size_bytes", sizeBytes, "destination", destination)
	metrics.RecordBackupRun(destination, nil, time.Since(start))
	span.SetAttributes(
		attribute.String("backup.destination", destination),
		attribute.Int64("backup.size_bytes", sizeBytes),
	)
	return nil
}

// failBackupNow finalises a failed run, records metrics/tracing, and returns
// the error wrapped with asynq.SkipRetry (a control-plane backup failure is
// not transient in a way an immediate retry would fix).
func (h *TaskHandler) failBackupNow(ctx context.Context, span trace.Span, runID pgtype.UUID, lg *runLog, err error, start time.Time) error {
	lg.fail("Backup failed: %s", err.Error())
	// A failed run never produced a valid encrypted archive to speak of,
	// regardless of how far it got.
	h.finaliseRun(ctx, runID, 0, pgtype.Text{}, false, err.Error(), lg.String())
	metrics.RecordBackupRun("local", err, time.Since(start))
	recordSpanErr(span, err)
	return errors.Join(fmt.Errorf("control-plane backup: %w", err), asynq.SkipRetry)
}

// HandleBackupRotateTask applies the retention policy: deletes remote objects
// older than the retained days that are beyond the retained count newest.
func (h *TaskHandler) HandleBackupRotateTask(ctx context.Context, t *asynq.Task) error {
	if h.BackupService == nil || !h.BackupService.Enabled() {
		slog.Debug("backup_rotate: remote backup not enabled, skipping")
		return nil
	}

	retainDays, retainCount := h.resolveBackupRetention(ctx)
	deleted, err := h.BackupService.Rotate(ctx, retainDays, retainCount)
	metrics.RecordBackupRotate(err)
	if err != nil {
		return fmt.Errorf("backup rotate: %w", err)
	}

	if len(deleted) > 0 {
		slog.Info("backup_rotate: removed old backups", "count", len(deleted))
	} else {
		slog.Debug("backup_rotate: nothing to prune")
	}
	return nil
}

// finaliseRun updates the backup_run record with the final status, whether
// the archive was encrypted, optional error message, and the full script
// output (log). Logs and swallows DB errors — the backup itself already
// succeeded or failed; we don't want a DB hiccup to mask that.
func (h *TaskHandler) finaliseRun(ctx context.Context, id pgtype.UUID, sizeBytes int64, remoteKey pgtype.Text, encrypted bool, errMsg, log string) {
	if h.Queries == nil || !id.Valid {
		return
	}

	status := "succeeded"
	errText := pgtype.Text{}
	if errMsg != "" {
		status = "failed"
		errText = pgtype.Text{String: errMsg, Valid: true}
	}

	if err := h.Queries.UpdateBackupRun(ctx, generated.UpdateBackupRunParams{
		ID:         id,
		FinishedAt: pgtype.Timestamptz{Time: time.Now(), Valid: true},
		Status:     status,
		RemoteKey:  remoteKey,
		SizeBytes:  sizeBytes,
		Error:      errText,
		Log:        log,
		Encrypted:  encrypted,
	}); err != nil {
		slog.Warn("backup_now: failed to update run record", "error", err)
	}
}
