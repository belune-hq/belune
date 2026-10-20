package worker

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"time"

	"github.com/hibiken/asynq"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/robfig/cron/v3"

	"github.com/belune-hq/belune/internal/config"
	"github.com/belune-hq/belune/internal/pkg/cronspec"
	"github.com/belune-hq/belune/internal/store/generated"
)

// HandleBackupScheduleSweep runs every minute (and dispatches due scheduled
// tasks, see sweepScheduledTasks): for the control-plane backup
// schedule and for each enabled per-resource backup config (database or
// application volume) whose cron schedule has elapsed since its last run, it
// enqueues the matching backup task. Best-effort — a failed enqueue leaves the
// due-check baseline untouched so the next sweep retries.
func (h *TaskHandler) HandleBackupScheduleSweep(ctx context.Context) {
	if h.Enqueuer == nil {
		return
	}
	now := time.Now()
	h.sweepControlPlaneBackup(ctx, now)
	h.sweepDatabaseBackupConfigs(ctx, now)
	h.sweepVolumeBackupConfigs(ctx, now)
	h.sweepScheduledTasks(ctx, now)
}

// sweepControlPlaneBackup enqueues TypeBackupNow when the in-app control-plane
// backup schedule (Server → Backups) has elapsed since the last backup_runs
// row — from EITHER producer, so a manual click or a scripts/backup.sh CLI run
// (which also records a run — see backup.sh) resets the clock just like a
// scheduled one would. Unlike the per-resource sweeps there is no dedicated
// config row: enabled/schedule live in the generic settings table, and
// "last run" is read off backup_runs directly rather than a stamped
// last_run_at, so a CLI-triggered run is never double-counted.
func (h *TaskHandler) sweepControlPlaneBackup(ctx context.Context, now time.Time) {
	if s, err := h.Queries.GetSetting(ctx, config.SettingControlPlaneBackupEnabled); err == nil && s.Value == "false" {
		return
	}

	schedule := config.DefaultControlPlaneBackupSchedule
	// No stored setting yet: baseline "now" so a fresh install fires at the next
	// scheduled tick rather than immediately on first boot.
	baseline := now
	if s, err := h.Queries.GetSetting(ctx, config.SettingControlPlaneBackupSchedule); err == nil {
		if v := strings.TrimSpace(s.Value); v != "" {
			schedule = v
		}
		baseline = s.UpdatedAt.Time
	}

	sched, err := cron.ParseStandard(schedule)
	if err != nil {
		slog.Warn("backup sweep: invalid control-plane schedule", "schedule", schedule, "error", err)
		return
	}

	last, err := h.Queries.GetLastBackupRun(ctx)
	switch {
	case err == nil:
		if last.Status == "running" {
			return // a worker or CLI backup is already in flight
		}
		baseline = last.StartedAt.Time
	case errors.Is(err, pgx.ErrNoRows):
		// No run yet — keep the settings-row baseline computed above.
	default:
		slog.Warn("backup sweep: get last control-plane backup run", "error", err)
		return
	}

	if sched.Next(baseline).After(now) {
		return
	}

	if _, err := h.Enqueuer.Enqueue(asynq.NewTask(TypeBackupNow, nil), asynq.Queue("low")); err != nil {
		slog.Warn("backup sweep: enqueue control-plane backup", "error", err)
	}
}

func (h *TaskHandler) sweepDatabaseBackupConfigs(ctx context.Context, now time.Time) {
	configs, err := h.Queries.ListEnabledDatabaseBackupConfigs(ctx)
	if err != nil {
		slog.Warn("backup sweep: list configs", "error", err)
		return
	}

	for _, cfg := range configs {
		if !backupConfigDue(cfg, now) {
			continue
		}

		// Skip if a backup is already running for this database to avoid
		// concurrent archive writes against the same container.
		if h.databaseBackupInProgress(ctx, cfg.DatabaseID) {
			continue
		}

		payload, mErr := json.Marshal(backupDBPayload{
			DatabaseID:     formatUUID(cfg.DatabaseID),
			BackupConfigID: formatUUID(cfg.ID),
		})
		if mErr != nil {
			slog.Warn("backup sweep: marshal payload", "config_id", formatUUID(cfg.ID), "error", mErr)
			continue
		}

		if _, eErr := h.Enqueuer.Enqueue(asynq.NewTask(TypeBackupDB, payload),
			asynq.Queue("default"), asynq.MaxRetry(1)); eErr != nil {
			slog.Warn("backup sweep: enqueue backup", "config_id", formatUUID(cfg.ID), "error", eErr)
			continue
		}

		if tErr := h.Queries.TouchDatabaseBackupConfigRun(ctx, generated.TouchDatabaseBackupConfigRunParams{
			ID:        cfg.ID,
			LastRunAt: pgtype.Timestamptz{Time: now, Valid: true},
		}); tErr != nil {
			slog.Warn("backup sweep: stamp last_run_at", "config_id", formatUUID(cfg.ID), "error", tErr)
		}
	}
}

// backupConfigDue reports whether cfg's schedule has fired since its last run.
// The baseline is last_run_at, or created_at for a config that has never run
// (so a new config fires at its next scheduled occurrence, not immediately).
// An unparseable schedule is treated as not-due (validated at create/update).
func backupConfigDue(cfg generated.DatabaseBackupConfig, now time.Time) bool {
	sched, err := cronspec.Parse(cfg.Schedule, cfg.Timezone)
	if err != nil {
		slog.Warn("backup sweep: invalid schedule", "config_id", formatUUID(cfg.ID), "schedule", cfg.Schedule, "error", err)
		return false
	}
	baseline := cfg.CreatedAt.Time
	if cfg.LastRunAt.Valid {
		baseline = cfg.LastRunAt.Time
	}
	next := sched.Next(baseline)
	return !next.After(now)
}

// databaseBackupInProgress reports whether the most recent backup run for a
// database is still running.
func (h *TaskHandler) databaseBackupInProgress(ctx context.Context, dbID pgtype.UUID) bool {
	recent, err := h.Queries.ListDatabaseBackups(ctx, generated.ListDatabaseBackupsParams{
		DatabaseID: dbID,
		Limit:      1,
	})
	if err != nil {
		// On a query error, be conservative and skip this tick.
		slog.Warn("backup sweep: check in-progress", "database_id", formatUUID(dbID), "error", err)
		return true
	}
	return len(recent) > 0 && recent[0].Status == "running"
}

// sweepVolumeBackupConfigs enqueues a backup_volume task for each enabled,
// scheduled application-volume config whose cron has elapsed. Mirrors the
// database sweep above.
func (h *TaskHandler) sweepVolumeBackupConfigs(ctx context.Context, now time.Time) {
	configs, err := h.Queries.ListEnabledApplicationVolumeBackupConfigs(ctx)
	if err != nil {
		slog.Warn("backup sweep: list volume configs", "error", err)
		return
	}

	for _, cfg := range configs {
		if !volumeBackupConfigDue(cfg, now) {
			continue
		}

		// Skip if a backup is already running for this volume to avoid
		// concurrent archive writes against the same volume.
		if h.volumeBackupInProgress(ctx, cfg.ApplicationVolumeID) {
			continue
		}

		payload, mErr := json.Marshal(backupVolumePayload{
			ApplicationVolumeID: formatUUID(cfg.ApplicationVolumeID),
			BackupConfigID:      formatUUID(cfg.ID),
		})
		if mErr != nil {
			slog.Warn("backup sweep: marshal volume payload", "config_id", formatUUID(cfg.ID), "error", mErr)
			continue
		}

		if _, eErr := h.Enqueuer.Enqueue(asynq.NewTask(TypeBackupVolume, payload),
			asynq.Queue("default"), asynq.MaxRetry(1)); eErr != nil {
			slog.Warn("backup sweep: enqueue volume backup", "config_id", formatUUID(cfg.ID), "error", eErr)
			continue
		}

		if tErr := h.Queries.SetApplicationVolumeBackupConfigLastRun(ctx, generated.SetApplicationVolumeBackupConfigLastRunParams{
			ID:        cfg.ID,
			LastRunAt: pgtype.Timestamptz{Time: now, Valid: true},
		}); tErr != nil {
			slog.Warn("backup sweep: stamp volume last_run_at", "config_id", formatUUID(cfg.ID), "error", tErr)
		}
	}
}

// volumeBackupConfigDue reports whether cfg's schedule has fired since its last
// run. Baseline is last_run_at, or created_at for a config that has never run.
// An unparseable/empty schedule is treated as not-due (validated at
// create/update; the enabled-config query already filters empty schedules).
func volumeBackupConfigDue(cfg generated.ApplicationVolumeBackupConfig, now time.Time) bool {
	sched, err := cronspec.Parse(cfg.Schedule, cfg.Timezone)
	if err != nil {
		slog.Warn("backup sweep: invalid volume schedule", "config_id", formatUUID(cfg.ID), "schedule", cfg.Schedule, "error", err)
		return false
	}
	baseline := cfg.CreatedAt.Time
	if cfg.LastRunAt.Valid {
		baseline = cfg.LastRunAt.Time
	}
	next := sched.Next(baseline)
	return !next.After(now)
}

// volumeBackupInProgress reports whether the most recent backup run for a volume
// is still running.
func (h *TaskHandler) volumeBackupInProgress(ctx context.Context, volID pgtype.UUID) bool {
	recent, err := h.Queries.ListApplicationVolumeBackups(ctx, generated.ListApplicationVolumeBackupsParams{
		ApplicationVolumeID: volID,
		Limit:               1,
	})
	if err != nil {
		slog.Warn("backup sweep: check volume in-progress", "volume_id", formatUUID(volID), "error", err)
		return true
	}
	return len(recent) > 0 && recent[0].Status == "running"
}
