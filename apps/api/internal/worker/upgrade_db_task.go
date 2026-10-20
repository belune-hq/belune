package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/hibiken/asynq"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/belune-hq/belune/internal/runtime"
	statuspkg "github.com/belune-hq/belune/internal/status"
	"github.com/belune-hq/belune/internal/store/generated"
)

// upgradeDBPayload triggers a guarded major-version upgrade of a managed
// database (known engines only).
type upgradeDBPayload struct {
	DatabaseID    string `json:"database_id"`
	TargetVersion string `json:"target_version"`
}

// HandleUpgradeDBTask performs a guarded dump-and-reload major-version upgrade:
//
//  1. Take a logical dump of the current database (recorded as a backup row —
//     this is the rollback artifact, also uploaded to S3 when enabled).
//  2. Remove the old container and wipe its volume.
//  3. Provision a fresh container at the target version and restore the dump.
//
// If provisioning or restore fails, the database is rolled back to the prior
// version from the same dump, so the database is never left empty. Only logical-
// dump engines (postgres/mysql/mongo) are eligible; "other" and redis are not.
//
// The task does not auto-retry: a failed upgrade either rolls back cleanly or
// leaves the database failed with its dump intact for manual recovery.
func (h *TaskHandler) HandleUpgradeDBTask(ctx context.Context, t *asynq.Task) error {
	var payload upgradeDBPayload
	if err := json.Unmarshal(t.Payload(), &payload); err != nil {
		return errors.Join(fmt.Errorf("unmarshal upgrade_db payload: %w", err), asynq.SkipRetry)
	}

	slog.Info("handling upgrade_db task", "database_id", payload.DatabaseID, "target_version", payload.TargetVersion)

	dbID, err := parseUUID(payload.DatabaseID)
	if err != nil {
		return errors.Join(fmt.Errorf("invalid database_id (permanent): %w", err), asynq.SkipRetry)
	}

	db, err := h.Queries.GetDatabase(ctx, dbID)
	if err != nil {
		return errors.Join(fmt.Errorf("get database: %w", err), asynq.SkipRetry)
	}

	// One resolve for the upgrade and for any rollback that follows it: the
	// dump, the recreate and the restore all have to land on the same host.
	rt, err := h.runtimeForDatabase(ctx, dbID)
	if err != nil {
		// Nothing has been dumped, removed or recreated yet, so the database is
		// still healthy at its current version — return it to running rather
		// than stamping a permanent failure on an untouched row.
		h.setDatabaseStatus(ctx, dbID, "running")
		return errors.Join(fmt.Errorf("resolve server: %w", err), asynq.SkipRetry)
	}

	if dbBackupMethod(db) != "logical" {
		h.failDatabase(ctx, dbID, fmt.Sprintf("upgrade not supported for type %s", db.Type))
		return errors.Join(fmt.Errorf("upgrade not supported for type %s (permanent)", db.Type), asynq.SkipRetry)
	}

	creds, err := h.decryptDBCredentials(db)
	if err != nil {
		h.failDatabase(ctx, dbID, fmt.Sprintf("credentials: %v", err))
		return errors.Join(fmt.Errorf("credentials (permanent): %w", err), asynq.SkipRetry)
	}

	oldVersion := db.Version

	// 1. Pre-upgrade dump — the rollback artifact, recorded as a backup row.
	dumpPath, err := h.dumpForUpgrade(ctx, rt, db, creds)
	if err != nil {
		// The database was not touched; return it to running.
		h.setDatabaseStatus(ctx, dbID, "running")
		return errors.Join(fmt.Errorf("pre-upgrade dump failed: %w", err), asynq.SkipRetry)
	}

	var hostPort int32
	if db.HostPort.Valid {
		hostPort = db.HostPort.Int32
	}

	// 2. Switch the row to the target version and rebuild the container on a
	// fresh volume. The dump (step 1) is the only data copy from here until the
	// restore succeeds, so a failure rolls back from it.
	dbNew, err := h.Queries.UpdateDatabaseVersion(ctx, generated.UpdateDatabaseVersionParams{ID: dbID, Version: payload.TargetVersion})
	if err != nil {
		h.setDatabaseStatus(ctx, dbID, "running")
		return errors.Join(fmt.Errorf("update version: %w", err), asynq.SkipRetry)
	}
	// The old pin belongs to the old version — clear it so provision re-pulls the
	// target tag and pins its digest (also covers same-tag "refresh to latest").
	dbNew.ImageDigest = pgtype.Text{}

	h.removeDBContainer(ctx, rt, db.Slug)
	if err := rt.RemoveVolume(ctx, db.Slug+"-vol"); err != nil {
		slog.Warn("upgrade: remove old volume (may not exist)", "database_id", payload.DatabaseID, "error", err)
	}

	if err := h.provisionDBContainer(ctx, rt, dbNew, creds, hostPort); err != nil {
		return h.rollbackUpgrade(ctx, rt, dbNew, creds, oldVersion, hostPort, dumpPath,
			fmt.Sprintf("provision target version: %v", err))
	}

	// The container is started but the engine needs a moment to accept
	// authenticated connections; restoring before then would fail spuriously.
	if err := h.waitForDBReady(ctx, rt, dbNew, creds); err != nil {
		return h.rollbackUpgrade(ctx, rt, dbNew, creds, oldVersion, hostPort, dumpPath,
			fmt.Sprintf("target version not ready: %v", err))
	}

	// 3. Restore the dump into the new-version container.
	if err := h.applyRestoreArchive(ctx, rt, dbNew, creds, "logical", dumpPath, ""); err != nil {
		return h.rollbackUpgrade(ctx, rt, dbNew, creds, oldVersion, hostPort, dumpPath,
			fmt.Sprintf("restore into target version: %v", err))
	}

	slog.Info("database upgraded", "database_id", payload.DatabaseID, "from", oldVersion, "to", payload.TargetVersion)
	return nil
}

// dumpForUpgrade produces a logical dump recorded as a succeeded backup row and
// returns the local path used as the upgrade's rollback artifact.
func (h *TaskHandler) dumpForUpgrade(ctx context.Context, rt runtime.ContainerRuntime, db generated.Database, creds map[string]string) (string, error) {
	run, err := h.Queries.InsertDatabaseBackup(ctx, generated.InsertDatabaseBackupParams{DatabaseID: db.ID})
	if err != nil {
		return "", fmt.Errorf("insert backup run: %w", err)
	}

	lg := &runLog{}
	lg.step("Pre-upgrade dump started (engine=%s)", db.Type)

	if err := os.MkdirAll(h.Config.DatabaseBackupDir, 0o755); err != nil {
		h.failDatabaseBackupLog(ctx, run.ID, fmt.Sprintf("create backup dir: %v", err), lg)
		return "", err
	}
	fileName := fmt.Sprintf("%s-preupgrade-%s.backup.gz", db.Slug, time.Now().UTC().Format("20060102T150405Z"))
	localPath := filepath.Join(h.Config.DatabaseBackupDir, fileName)

	f, err := os.Create(localPath)
	if err != nil {
		h.failDatabaseBackupLog(ctx, run.ID, fmt.Sprintf("create backup file: %v", err), lg)
		return "", err
	}
	archiveErr := h.writeBackupArchive(ctx, rt, db, creds, "logical", f, lg, "")
	closeErr := f.Close()
	if archiveErr != nil {
		_ = os.Remove(localPath)
		h.failDatabaseBackupLog(ctx, run.ID, archiveErr.Error(), lg)
		return "", archiveErr
	}
	if closeErr != nil {
		_ = os.Remove(localPath)
		h.failDatabaseBackupLog(ctx, run.ID, closeErr.Error(), lg)
		return "", closeErr
	}

	var sizeBytes int64
	if info, statErr := os.Stat(localPath); statErr == nil {
		sizeBytes = info.Size()
	}
	// Validity gate: a real logical dump (even of an empty schema) compresses to
	// well over this; a near-empty archive means the engine produced no output
	// despite a zero exit. Abort here so the upgrade never wipes the volume with
	// nothing to restore from.
	const minPreUpgradeDumpBytes = 100
	if sizeBytes < minPreUpgradeDumpBytes {
		_ = os.Remove(localPath)
		msg := fmt.Sprintf("pre-upgrade dump is suspiciously small (%d bytes); aborting before volume wipe", sizeBytes)
		h.failDatabaseBackupLog(ctx, run.ID, msg, lg)
		return "", errors.New(msg)
	}
	lg.step("Archive written: %s (%d bytes)", fileName, sizeBytes)

	remoteKey := pgtype.Text{}
	if h.BackupService != nil && h.BackupService.Enabled() {
		if key, upErr := h.BackupService.Upload(ctx, localPath); upErr != nil {
			lg.step("S3 upload failed (keeping local copy): %v", upErr)
			slog.Warn("upgrade: pre-upgrade dump S3 upload failed; keeping local copy", "database_id", formatUUID(db.ID), "error", upErr)
		} else {
			remoteKey = pgtype.Text{String: key, Valid: true}
			lg.step("Uploaded to S3: %s", key)
		}
	}
	lg.step("Pre-upgrade dump succeeded")
	h.finaliseDatabaseBackup(ctx, run.ID, generated.UpdateDatabaseBackupParams{
		ID:         run.ID,
		FinishedAt: pgtype.Timestamptz{Time: time.Now(), Valid: true},
		Status:     "succeeded",
		LocalPath:  pgtype.Text{String: localPath, Valid: true},
		RemoteKey:  remoteKey,
		SizeBytes:  sizeBytes,
		Log:        lg.String(),
	})
	return localPath, nil
}

// rollbackUpgrade restores the database to its prior version from the pre-upgrade
// dump after a failed upgrade. Returns nil when the rollback succeeds (the
// database is healthy at the old version, so there is nothing to retry); returns
// the error only if the rollback itself fails (database left failed).
func (h *TaskHandler) rollbackUpgrade(ctx context.Context, rt runtime.ContainerRuntime, db generated.Database, creds map[string]string, oldVersion string, hostPort int32, dumpPath, reason string) error {
	slog.Warn("upgrade failed; rolling back to previous version", "database_id", formatUUID(db.ID), "reason", reason, "old_version", oldVersion)

	dbOld, err := h.Queries.UpdateDatabaseVersion(ctx, generated.UpdateDatabaseVersionParams{ID: db.ID, Version: oldVersion})
	if err != nil {
		h.failDatabase(ctx, db.ID, fmt.Sprintf("upgrade failed (%s); rollback could not reset version: %v", reason, err))
		return errors.Join(fmt.Errorf("rollback reset version: %w", err), asynq.SkipRetry)
	}
	// Re-pin for the restored old version (provision re-resolves the tag digest).
	dbOld.ImageDigest = pgtype.Text{}

	h.removeDBContainer(ctx, rt, dbOld.Slug)
	if err := rt.RemoveVolume(ctx, dbOld.Slug+"-vol"); err != nil {
		slog.Warn("rollback: remove volume (may not exist)", "database_id", formatUUID(db.ID), "error", err)
	}
	if err := h.provisionDBContainer(ctx, rt, dbOld, creds, hostPort); err != nil {
		h.failDatabase(ctx, db.ID, fmt.Sprintf("upgrade failed (%s); rollback provision failed: %v", reason, err))
		return errors.Join(fmt.Errorf("rollback provision: %w", err), asynq.SkipRetry)
	}
	if err := h.waitForDBReady(ctx, rt, dbOld, creds); err != nil {
		h.failDatabase(ctx, db.ID, fmt.Sprintf("upgrade failed (%s); rollback not ready: %v", reason, err))
		return errors.Join(fmt.Errorf("rollback not ready: %w", err), asynq.SkipRetry)
	}
	if err := h.applyRestoreArchive(ctx, rt, dbOld, creds, "logical", dumpPath, ""); err != nil {
		h.failDatabase(ctx, db.ID, fmt.Sprintf("upgrade failed (%s); rollback restore failed: %v", reason, err))
		return errors.Join(fmt.Errorf("rollback restore: %w", err), asynq.SkipRetry)
	}

	slog.Warn("upgrade rolled back; database restored to previous version", "database_id", formatUUID(db.ID), "old_version", oldVersion)
	return nil
}

// waitForDBReady polls the engine's own client until it accepts an
// authenticated connection (or the deadline passes), so a restore that runs
// immediately after (re)provisioning does not race container startup. Engines
// without a probe return nil (no wait).
func (h *TaskHandler) waitForDBReady(ctx context.Context, rt runtime.ContainerRuntime, db generated.Database, creds map[string]string) error {
	cmd, ok := dbReadyCmd(db.Type, creds)
	if !ok {
		return nil
	}
	deadline := time.Now().Add(90 * time.Second)
	var lastErr error
	for {
		exit, err := rt.ContainerExec(ctx, db.Slug, cmd, nil, nil, nil)
		if err == nil && exit == 0 {
			return nil
		}
		lastErr = fmt.Errorf("exit=%d, err=%v", exit, err)
		if time.Now().After(deadline) {
			return fmt.Errorf("database not ready before timeout: %w", lastErr)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

// ReconcileInterruptedUpgrades marks databases left in the "upgrading" state as
// failed. Upgrades run with no auto-retry, so any database still "upgrading"
// when the worker boots is from an interrupted upgrade (e.g. a crash between the
// volume wipe and the restore) that will not resume on its own — without this it
// would sit in a silent, permanent transitional state. The pre-upgrade dump was
// recorded as a backup, so the data is recoverable from it.
func (h *TaskHandler) ReconcileInterruptedUpgrades(ctx context.Context) {
	upgrading, err := h.Queries.ListDatabasesByStatus(ctx, statuspkg.DatabaseUpgrading)
	if err != nil {
		slog.Warn("reconcile: failed to list upgrading databases", "error", err)
	}
	for _, db := range upgrading {
		slog.Warn("reconcile: marking interrupted upgrade as failed; recover from the pre-upgrade backup",
			"database_id", formatUUID(db.ID), "slug", db.Slug)
		h.failDatabase(ctx, db.ID, "upgrade interrupted (worker restart); restore from the latest pre-upgrade backup")
	}

	// A volume snapshot only stops the container to tar it (read-only), so an
	// interrupted one is safely recovered by restarting the container.
	backingUp, err := h.Queries.ListDatabasesByStatus(ctx, statuspkg.DatabaseBackingUp)
	if err != nil {
		slog.Warn("reconcile: failed to list backing-up databases", "error", err)
	}
	for _, db := range backingUp {
		slog.Warn("reconcile: restarting database left mid-snapshot", "database_id", formatUUID(db.ID), "slug", db.Slug)
		// The status is stamped either way. Leaving the row in backing_up is
		// the exact silent, permanent transitional state this function exists
		// to clear, and a failed restart already falls through to it.
		if rt, err := h.runtimeForDatabase(ctx, db.ID); err != nil {
			slog.Warn("reconcile: could not reach the database's server", "database_id", formatUUID(db.ID), "error", err)
		} else if err := rt.StartContainer(ctx, db.Slug); err != nil {
			slog.Warn("reconcile: failed to restart database after snapshot", "database_id", formatUUID(db.ID), "error", err)
		}
		h.setDatabaseStatus(ctx, db.ID, statuspkg.DatabaseRunning)
	}
}

func (h *TaskHandler) setDatabaseStatus(ctx context.Context, dbID pgtype.UUID, status string) {
	if _, err := h.Queries.UpdateDatabaseStatus(ctx, generated.UpdateDatabaseStatusParams{ID: dbID, Status: status}); err != nil {
		slog.Warn("failed to set database status", "database_id", formatUUID(dbID), "status", status, "error", err)
	}
}
