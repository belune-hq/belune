package worker

import (
	"bytes"
	"compress/gzip"
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

	"github.com/belune-hq/belune/internal/notify"
	"github.com/belune-hq/belune/internal/pkg/joblog"
	"github.com/belune-hq/belune/internal/pkg/loglevel"
	"github.com/belune-hq/belune/internal/runtime"
	"github.com/belune-hq/belune/internal/service/backup"
	statuspkg "github.com/belune-hq/belune/internal/status"
	"github.com/belune-hq/belune/internal/store/generated"
)

// beluneBackupDir is the in-container scratch dir for "command" backup mode. The
// user's backup command writes here (exposed as $BELUNE_BACKUP_DIR); the system
// then tars it out. Under /tmp so it is writable by non-root container users.
const beluneBackupDir = "/tmp/belune-backup"

// backupDBPayload triggers a backup of a managed database. BackupConfigID is
// optional: when set, the dump is uploaded to that config's project destination
// (with its prefix) and retention follows the config's keep_latest; when empty
// the backup is an ad-hoc run using the global env-var S3 target (best-effort).
type backupDBPayload struct {
	DatabaseID     string `json:"database_id"`
	BackupConfigID string `json:"backup_config_id,omitempty"`
}

// restoreDBPayload restores a managed database from a previously-recorded backup.
type restoreDBPayload struct {
	DatabaseID string `json:"database_id"`
	BackupID   string `json:"backup_id"`
}

// dumpSpec is the per-engine logical dump/restore command pair (known engines).
type dumpSpec struct {
	dump    []string
	restore []string
	ok      bool
}

// shArg single-quotes a value for safe inclusion in a `sh -c` command line.
func shArg(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// dumpTargets parses a stored target_database value into the concrete database
// names to dump, plus an "all" flag:
//   - "*"          -> all databases (cluster), names nil
//   - ""           -> the provisioned managed database (creds["database"])
//   - "a" / "a,b"  -> those specific databases
func dumpTargets(creds map[string]string, target string) (names []string, all bool) {
	if target == "*" {
		return nil, true
	}
	for _, p := range strings.Split(target, ",") {
		if p = strings.TrimSpace(p); p != "" {
			names = append(names, p)
		}
	}
	if len(names) == 0 {
		names = []string{creds["database"]}
	}
	return names, false
}

func sh(cmd string) []string { return []string{"sh", "-c", cmd} }

// dbDumpSpec returns the dump/restore command pair for a known engine, scoped by
// target (see dumpTargets): "*" = all databases, "" = the managed database, or a
// comma-separated list of specific database names.
func dbDumpSpec(dbType string, creds map[string]string, target string) dumpSpec {
	names, all := dumpTargets(creds, target)
	switch dbType {
	case "postgres":
		user := shArg(creds["user"])
		pw := shArg(creds["password"])
		switch {
		case all:
			return dumpSpec{
				dump:    sh(fmt.Sprintf("PGPASSWORD=%s pg_dumpall -U %s --clean --if-exists --no-owner", pw, user)),
				restore: sh(fmt.Sprintf("PGPASSWORD=%s psql -U %s -d postgres", pw, user)),
				ok:      true,
			}
		case len(names) == 1:
			db := shArg(names[0])
			return dumpSpec{
				dump:    sh(fmt.Sprintf("PGPASSWORD=%s pg_dump -U %s -d %s --clean --if-exists --no-owner", pw, user, db)),
				restore: sh(fmt.Sprintf("PGPASSWORD=%s psql -U %s -d %s -v ON_ERROR_STOP=1", pw, user, db)),
				ok:      true,
			}
		default: // multiple specific: per-db --create dumps concatenated
			var b strings.Builder
			for i, n := range names {
				if i > 0 {
					b.WriteString(" && ")
				}
				b.WriteString(fmt.Sprintf("PGPASSWORD=%s pg_dump -U %s -d %s --create --clean --if-exists --no-owner", pw, user, shArg(n)))
			}
			return dumpSpec{
				dump:    sh(b.String()),
				restore: sh(fmt.Sprintf("PGPASSWORD=%s psql -U %s -d postgres", pw, user)),
				ok:      true,
			}
		}
	case "mysql":
		// Use root for dump+restore: dumping --routines/--triggers needs broad
		// privileges, and restoring DEFINER clauses fails as the app user.
		rootPw := shArg(creds["root_password"])
		switch {
		case all:
			return dumpSpec{
				dump:    sh(fmt.Sprintf("MYSQL_PWD=%s mysqldump --single-transaction --routines --triggers -u root --all-databases", rootPw)),
				restore: sh(fmt.Sprintf("MYSQL_PWD=%s mysql -u root", rootPw)),
				ok:      true,
			}
		case len(names) == 1:
			db := shArg(names[0])
			return dumpSpec{
				dump:    sh(fmt.Sprintf("MYSQL_PWD=%s mysqldump --single-transaction --routines --triggers -u root %s", rootPw, db)),
				restore: sh(fmt.Sprintf("MYSQL_PWD=%s mysql -u root %s", rootPw, db)),
				ok:      true,
			}
		default: // --databases includes CREATE DATABASE/USE per db
			list := ""
			for _, n := range names {
				list += " " + shArg(n)
			}
			return dumpSpec{
				dump:    sh(fmt.Sprintf("MYSQL_PWD=%s mysqldump --single-transaction --routines --triggers -u root --databases%s", rootPw, list)),
				restore: sh(fmt.Sprintf("MYSQL_PWD=%s mysql -u root", rootPw)),
				ok:      true,
			}
		}
	case "mongo":
		user := shArg(creds["username"])
		pw := shArg(creds["password"])
		base := fmt.Sprintf("mongodump --username %s --password %s --authenticationDatabase admin", user, pw)
		var dumpCmd string
		switch {
		case all:
			dumpCmd = base + " --archive"
		case len(names) == 1:
			dumpCmd = fmt.Sprintf("%s --db %s --archive", base, shArg(names[0]))
		default: // multiple databases via repeated --nsInclude
			ns := ""
			for _, n := range names {
				ns += fmt.Sprintf(" --nsInclude %s", shArg(n+".*"))
			}
			dumpCmd = base + ns + " --archive"
		}
		// Restore from an --archive is self-describing, so one command covers all scopes.
		return dumpSpec{
			dump:    sh(dumpCmd),
			restore: sh(fmt.Sprintf("mongorestore --username %s --password %s --authenticationDatabase admin --archive --drop", user, pw)),
			ok:      true,
		}
	default:
		return dumpSpec{ok: false}
	}
}

// dbReadyCmd returns a trivial connect-and-query command for a known engine,
// run via the same client the restore uses, so a zero exit means the engine is
// actually accepting authenticated connections (not merely started). ok is false
// for engines without a probe.
func dbReadyCmd(dbType string, creds map[string]string) ([]string, bool) {
	switch dbType {
	case "postgres":
		return []string{"sh", "-c", fmt.Sprintf("PGPASSWORD=%s psql -U %s -d %s -tAc 'SELECT 1'",
			shArg(creds["password"]), shArg(creds["user"]), shArg(creds["database"]))}, true
	case "mysql":
		return []string{"sh", "-c", fmt.Sprintf("MYSQL_PWD=%s mysql -u root -e 'SELECT 1'",
			shArg(creds["root_password"]))}, true
	case "mongo":
		return []string{"sh", "-c", fmt.Sprintf("mongosh --username %s --password %s --authenticationDatabase admin --quiet --eval 'db.runCommand({ping:1})'",
			shArg(creds["username"]), shArg(creds["password"]))}, true
	}
	return nil, false
}

// dbBackupMethod selects how a database is backed up:
//   - "logical": known engine, online dump (pg_dump/mysqldump/mongodump)
//   - "volume_snapshot": "other" — cold tar of the data-dir volume
//   - "command": "other" — user-supplied backup/restore commands
//   - "none": unsupported (redis, or "other" with backup disabled)
func dbBackupMethod(db generated.Database) string {
	switch db.Type {
	case "postgres", "mysql", "mongo":
		return "logical"
	case "other":
		switch db.BackupMode {
		case "volume_snapshot", "command":
			return db.BackupMode
		}
	}
	return "none"
}

// runLog accumulates a short, human-readable per-run backup log (timestamped
// step lines plus raw engine stderr) persisted to the run's log column.
//
// When flush is set, the log is persisted to the database as it grows so the UI
// can show it in near-realtime rather than only at completion. step lines flush
// immediately (they are the coarse progress signal); raw output is throttled to
// avoid hammering the database on chatty engine stderr. The finalise* update
// always writes the complete log, so a skipped intermediate flush is harmless.
type runLog struct {
	b         joblog.Builder
	flush     func(log string)
	lastFlush time.Time
}

// step records an informational progress line.
func (l *runLog) step(format string, args ...any) {
	l.b.Add(loglevel.Info, fmt.Sprintf(format, args...))
	l.persist(true)
}

// warn records a non-fatal warning line.
func (l *runLog) warn(format string, args ...any) {
	l.b.Add(loglevel.Warning, fmt.Sprintf(format, args...))
	l.persist(true)
}

// fail records a terminal error line.
func (l *runLog) fail(format string, args ...any) {
	l.b.Add(loglevel.Error, fmt.Sprintf(format, args...))
	l.persist(true)
}

// raw appends captured command output verbatim (stderr from dump/restore
// tools), one detected-level entry per line.
func (l *runLog) raw(s string) {
	l.b.AddRaw("stderr", s)
	l.persist(false)
}

func (l *runLog) persist(force bool) {
	if l.flush == nil {
		return
	}
	if !force && time.Since(l.lastFlush) < 500*time.Millisecond {
		return
	}
	l.lastFlush = time.Now()
	l.flush(l.String())
}

func (l *runLog) String() string { return l.b.String() }

// backupScopeLabel renders a human label for a target-database value, for logs.
func backupScopeLabel(target string) string {
	switch target {
	case "":
		return "managed database"
	case "*":
		return "all databases"
	default:
		return target
	}
}

func (h *TaskHandler) HandleBackupDBTask(ctx context.Context, t *asynq.Task) error {
	var payload backupDBPayload
	if err := json.Unmarshal(t.Payload(), &payload); err != nil {
		return fmt.Errorf("unmarshal backup_db payload: %w", err)
	}

	slog.Info("handling backup_db task", "database_id", payload.DatabaseID)

	dbID, err := parseUUID(payload.DatabaseID)
	if err != nil {
		return errors.Join(fmt.Errorf("invalid database_id (permanent): %w", err), asynq.SkipRetry)
	}

	db, err := h.Queries.GetDatabase(ctx, dbID)
	if err != nil {
		return fmt.Errorf("get database: %w", err)
	}

	// Resolved once for the whole run: a backup that exec'd into one host and
	// wrote its volume on another would be silently wrong.
	rt, err := h.runtimeForDatabase(ctx, dbID)
	if err != nil {
		return err
	}

	method := dbBackupMethod(db)
	if method == "none" {
		return errors.Join(fmt.Errorf("backup is not supported for database type %s (permanent)", db.Type), asynq.SkipRetry)
	}

	// Resolve the optional backup config + its project destination up front so a
	// misconfigured config fails before we start dumping.
	var cfg *generated.DatabaseBackupConfig
	var destClient *backup.DestinationClient
	var destPrefix string
	var destLocal bool
	configID := pgtype.UUID{}
	if payload.BackupConfigID != "" {
		cid, parseErr := parseUUID(payload.BackupConfigID)
		if parseErr != nil {
			return errors.Join(fmt.Errorf("invalid backup_config_id (permanent): %w", parseErr), asynq.SkipRetry)
		}
		c, getErr := h.Queries.GetDatabaseBackupConfig(ctx, cid)
		if getErr != nil {
			return errors.Join(fmt.Errorf("get backup config (permanent): %w", getErr), asynq.SkipRetry)
		}
		if c.DatabaseID != dbID {
			return errors.Join(errors.New("backup config does not belong to database (permanent)"), asynq.SkipRetry)
		}
		if h.BackupDestinations == nil {
			return errors.Join(errors.New("backup destinations service is not configured (permanent)"), asynq.SkipRetry)
		}
		dest, resErr := h.BackupDestinations.Resolve(ctx, c.DestinationID)
		if resErr != nil {
			return errors.Join(fmt.Errorf("resolve destination (permanent): %w", resErr), asynq.SkipRetry)
		}
		// A local destination has no bucket/endpoint/credentials to build a
		// client from — NewDestinationClient must never be called for it.
		if !dest.IsLocal() {
			client, clErr := backup.NewDestinationClient(dest)
			if clErr != nil {
				return errors.Join(fmt.Errorf("build destination client (permanent): %w", clErr), asynq.SkipRetry)
			}
			destClient = client
			destPrefix = dest.Prefix
		}
		cfg = &c
		destLocal = dest.IsLocal()
		configID = cid
	}

	// Target database scope: "" = provisioned DB, "*" = all (cluster), or a
	// specific name. Carried onto the run so restore replays the same scope.
	target := ""
	if cfg != nil {
		target = cfg.TargetDatabase
	}

	// Record the run up front so the UI shows a "running" backup immediately.
	run, err := h.Queries.InsertDatabaseBackup(ctx, generated.InsertDatabaseBackupParams{
		DatabaseID:     dbID,
		BackupConfigID: configID,
		TargetDatabase: target,
	})
	if err != nil {
		return fmt.Errorf("insert backup run: %w", err)
	}

	lg := &runLog{}
	lg.flush = func(s string) {
		if err := h.Queries.SetDatabaseBackupLog(ctx, generated.SetDatabaseBackupLogParams{ID: run.ID, Log: s}); err != nil {
			slog.Warn("backup_db: flush log", "backup_id", formatUUID(run.ID), "error", err)
		}
	}
	lg.step("Backup started (method=%s, engine=%s, scope=%s)", method, db.Type, backupScopeLabel(target))

	creds, err := h.decryptDBCredentials(db)
	if err != nil {
		h.failDatabaseBackupLog(ctx, run.ID, fmt.Sprintf("credentials: %v", err), lg)
		return errors.Join(fmt.Errorf("credentials (permanent): %w", err), asynq.SkipRetry)
	}

	if err := os.MkdirAll(h.Config.DatabaseBackupDir, 0o755); err != nil {
		h.failDatabaseBackupLog(ctx, run.ID, fmt.Sprintf("create backup dir: %v", err), lg)
		return fmt.Errorf("create backup dir: %w", err)
	}

	fileName := fmt.Sprintf("%s-%s.backup.gz", db.Slug, time.Now().UTC().Format("20060102T150405Z"))
	localPath := filepath.Join(h.Config.DatabaseBackupDir, fileName)

	f, err := os.Create(localPath)
	if err != nil {
		h.failDatabaseBackupLog(ctx, run.ID, fmt.Sprintf("create backup file: %v", err), lg)
		return fmt.Errorf("create backup file: %w", err)
	}

	archiveErr := h.writeBackupArchive(ctx, rt, db, creds, method, f, lg, target)
	closeErr := f.Close()
	if archiveErr != nil {
		_ = os.Remove(localPath)
		h.failDatabaseBackupLog(ctx, run.ID, archiveErr.Error(), lg)
		h.notifyDatabaseOwner(ctx, db, "database.backup_failed", "Database backup failed",
			fmt.Sprintf("Backing up %s failed: %s", db.Name, archiveErr.Error()))
		return archiveErr
	}
	if closeErr != nil {
		_ = os.Remove(localPath)
		h.failDatabaseBackupLog(ctx, run.ID, fmt.Sprintf("finalise backup file: %v", closeErr), lg)
		return fmt.Errorf("finalise backup file: %w", closeErr)
	}

	var sizeBytes int64
	if info, statErr := os.Stat(localPath); statErr == nil {
		sizeBytes = info.Size()
	}
	lg.step("Archive written: %s (%d bytes)", fileName, sizeBytes)

	// A successful remote upload is authoritative, so we drop the local archive
	// afterwards — otherwise the server disk mirrors the bucket and grows without
	// bound (there is no local retention cap). We keep the local copy only when
	// there is no remote copy: upload failed, or no remote is configured, where
	// the local file is the sole backup and is bounded by pruneDatabaseBackups.
	remoteKey := pgtype.Text{}
	localKept := true
	if cfg != nil && destLocal {
		// Local destination: the staged archive already IS the backup — no
		// upload, no deletion. Retention (pruneConfigBackups below) prunes it
		// like any other local file once newer runs push it past keep_latest.
		lg.step("Local destination — keeping archive on-host: %s", localPath)
	} else if cfg != nil {
		// Config-driven backup: the project destination is the whole point, so a
		// failed upload fails the run (unlike the best-effort global path). The
		// object key is <destination.prefix>/<config.prefix>/<file>.
		key := backup.BuildKey(destPrefix, backup.BuildKey(cfg.Prefix, fileName))
		lg.step("Uploading to destination: %s", key)
		if _, upErr := destClient.UploadTo(ctx, localPath, key); upErr != nil {
			_ = os.Remove(localPath)
			h.failDatabaseBackupLog(ctx, run.ID, fmt.Sprintf("upload to destination: %v", upErr), lg)
			h.notifyDatabaseOwner(ctx, db, "database.backup_failed", "Database backup failed",
				fmt.Sprintf("Backing up %s failed: %v", db.Name, upErr))
			return fmt.Errorf("upload to destination: %w", upErr)
		}
		remoteKey = pgtype.Text{String: key, Valid: true}
		removeLocalAfterUpload(localPath, lg)
		localKept = false
		lg.step("Uploaded to destination")
	} else if h.BackupService != nil && h.BackupService.Enabled() {
		// Off-host copy is best-effort: keep the local archive when the upload fails.
		if key, upErr := h.BackupService.Upload(ctx, localPath); upErr != nil {
			lg.warn("S3 upload failed (keeping local copy): %v", upErr)
			slog.Warn("backup_db: S3 upload failed; keeping local copy", "database_id", payload.DatabaseID, "error", upErr)
		} else {
			remoteKey = pgtype.Text{String: key, Valid: true}
			removeLocalAfterUpload(localPath, lg)
			localKept = false
			lg.step("Uploaded to S3: %s", key)
		}
	} else {
		lg.step("Stored locally: %s", localPath)
	}

	localPathText := pgtype.Text{}
	if localKept {
		localPathText = pgtype.Text{String: localPath, Valid: true}
	}

	lg.step("Backup succeeded")
	h.finaliseDatabaseBackup(ctx, run.ID, generated.UpdateDatabaseBackupParams{
		ID:         run.ID,
		FinishedAt: pgtype.Timestamptz{Time: time.Now(), Valid: true},
		Status:     "succeeded",
		LocalPath:  localPathText,
		RemoteKey:  remoteKey,
		SizeBytes:  sizeBytes,
		Log:        lg.String(),
	})

	if cfg != nil {
		// Record where this copy actually went, so a later restore does not have
		// to re-derive it from the config — which by then may point elsewhere.
		// Ad-hoc runs get no row: the global env-var target has no destination
		// row to reference (see the 000061 migration).
		h.recordBackupLocation(ctx, generated.InsertBackupLocationParams{
			DatabaseBackupID: run.ID,
			DestinationID:    cfg.DestinationID,
			RemoteKey:        remoteKey,
			LocalPath:        localPathText,
		})
		h.pruneConfigBackups(ctx, *cfg, destClient)
	} else {
		h.pruneDatabaseBackups(ctx, db.ID)
	}

	slog.Info("database backed up", "database_id", payload.DatabaseID, "method", method, "size_bytes", sizeBytes, "remote", remoteKey.Valid, "config", cfg != nil)
	return nil
}

// recordBackupLocation stores where a backup copy was written. Best-effort: the
// archive is already safely written, so failing the run over the bookkeeping
// would be worse than losing it — the restore path still falls back to the
// config when a backup has no recorded location.
func (h *TaskHandler) recordBackupLocation(ctx context.Context, p generated.InsertBackupLocationParams) {
	if !p.RemoteKey.Valid && !p.LocalPath.Valid {
		return // nothing was written anywhere; has_a_file would reject it
	}
	// An on-disk copy belongs to a specific host; a bucket-only copy belongs to
	// none, and leaving server_id NULL is how that is said. Every backup runs on
	// the control plane's own host today, so that is the only answer there is —
	// this is the one place that changes when a copy can be written elsewhere.
	if p.LocalPath.Valid {
		if server, err := h.Queries.GetLocalServer(ctx); err == nil {
			p.ServerID = server.ID
		} else {
			// Recording where the archive went still beats recording nothing.
			slog.Warn("backup: resolve local server for location", "error", err)
		}
	}
	if _, err := h.Queries.InsertBackupLocation(ctx, p); err != nil {
		slog.Warn("backup: record location",
			"database_backup_id", formatUUID(p.DatabaseBackupID),
			"volume_backup_id", formatUUID(p.VolumeBackupID),
			"error", err)
	}
}

// recordedCopy is a backup copy that is actually retrievable: the client for
// the bucket it lives in, paired with the key it was stored under. The two must
// travel together — using one copy's client against another copy's key is the
// same class of mistake this whole change exists to fix.
type recordedCopy struct {
	client *backup.DestinationClient
	key    string
}

// clientForRecordedDatabaseBackup returns the recorded copy of a database
// backup. A nil copy with a nil error means there is nothing recorded to use —
// no location row (a backup predating 000061, or an ad-hoc run), or no location
// with a remote object — and the caller should fall back to the config path.
func (h *TaskHandler) clientForRecordedDatabaseBackup(ctx context.Context, backupID pgtype.UUID) (*recordedCopy, error) {
	if h.BackupDestinations == nil {
		return nil, nil
	}
	locs, err := h.Queries.ListLocationsForDatabaseBackup(ctx, backupID)
	if err != nil {
		return nil, fmt.Errorf("list backup locations: %w", err)
	}
	return h.copyForLocations(ctx, locs)
}

// clientForRecordedVolumeBackup is the volume-backup twin of the above.
func (h *TaskHandler) clientForRecordedVolumeBackup(ctx context.Context, backupID pgtype.UUID) (*recordedCopy, error) {
	if h.BackupDestinations == nil {
		return nil, nil
	}
	locs, err := h.Queries.ListLocationsForVolumeBackup(ctx, backupID)
	if err != nil {
		return nil, fmt.Errorf("list backup locations: %w", err)
	}
	return h.copyForLocations(ctx, locs)
}

// copyForLocations picks the first recorded location holding a remote object and
// returns its client and key together. Today a backup has exactly one location;
// carrying the key is what lets 0.1.x add a second copy without the restore path
// silently pairing one destination with another's key.
func (h *TaskHandler) copyForLocations(ctx context.Context, locs []generated.BackupLocation) (*recordedCopy, error) {
	for _, loc := range locs {
		if !loc.RemoteKey.Valid {
			continue // local-only copy; the caller already tried the local file
		}
		client, err := h.BackupDestinations.ClientForDestination(ctx, loc.DestinationID)
		if err != nil {
			return nil, fmt.Errorf("resolve recorded backup destination: %w", err)
		}
		if client != nil {
			return &recordedCopy{client: client, key: loc.RemoteKey.String}, nil
		}
	}
	return nil, nil
}

// removeLocalAfterUpload deletes the local archive once it is safely uploaded to
// a remote target, so the server disk isn't used to mirror the bucket. A removal
// failure is noted in the run log but does not fail the (already-uploaded) backup.
func removeLocalAfterUpload(localPath string, lg *runLog) {
	if err := os.Remove(localPath); err != nil && !os.IsNotExist(err) {
		lg.warn("could not remove local copy after upload: %v", err)
	}
}

// remoteDeleter removes remote backup objects by key. nil = no remote deletion.
type remoteDeleter func(ctx context.Context, keys []string) error

// globalRemoteDeleter returns the deleter for the global env-var S3 target, or
// nil when remote backup is disabled.
func (h *TaskHandler) globalRemoteDeleter() remoteDeleter {
	if h.BackupService != nil && h.BackupService.Enabled() {
		return h.BackupService.Delete
	}
	return nil
}

// deleteBackupArtifacts removes a backup's local file, remote object, and row.
// The recorded location decides which bucket the object is deleted from; del is
// the fallback for backups written before locations existed (nil skips it).
func (h *TaskHandler) deleteBackupArtifacts(ctx context.Context, b generated.DatabaseBackup, del remoteDeleter) {
	if b.LocalPath.Valid {
		if err := os.Remove(b.LocalPath.String); err != nil && !os.IsNotExist(err) {
			slog.Warn("prune: remove local backup", "path", b.LocalPath.String, "error", err)
		}
	}
	if b.RemoteKey.Valid {
		copy, err := h.clientForRecordedDatabaseBackup(ctx, b.ID)
		if err != nil {
			slog.Warn("prune: resolve recorded destination", "backup_id", formatUUID(b.ID), "error", err)
		}
		switch {
		case copy != nil:
			if err := copy.client.DeleteFrom(ctx, []string{copy.key}); err != nil {
				slog.Warn("prune: remove remote backup", "key", copy.key, "error", err)
			}
		case del != nil:
			if err := del(ctx, []string{b.RemoteKey.String}); err != nil {
				slog.Warn("prune: remove remote backup", "key", b.RemoteKey.String, "error", err)
			}
		}
	}
	if err := h.Queries.DeleteDatabaseBackup(ctx, b.ID); err != nil {
		slog.Warn("prune: delete backup row", "backup_id", formatUUID(b.ID), "error", err)
	}
}

// pruneDatabaseBackups keeps the newest N backups for a database (config
// DatabaseBackupRetainCount, default 7) and deletes the rest. Best-effort. Used
// for ad-hoc (config-less) runs against the global target.
func (h *TaskHandler) pruneDatabaseBackups(ctx context.Context, dbID pgtype.UUID) {
	keep := 7
	if h.Config != nil && h.Config.DatabaseBackupRetainCount > 0 {
		keep = h.Config.DatabaseBackupRetainCount
	}
	rows, err := h.Queries.ListDatabaseBackups(ctx, generated.ListDatabaseBackupsParams{DatabaseID: dbID, Limit: 1000})
	if err != nil {
		slog.Warn("prune: list backups", "error", err)
		return
	}
	if len(rows) <= keep {
		return
	}
	del := h.globalRemoteDeleter()
	for _, b := range rows[keep:] { // ListDatabaseBackups is newest-first
		h.deleteBackupArtifacts(ctx, b, del)
	}
}

// pruneConfigBackups keeps the newest keep_latest backups produced by a config
// and deletes the rest (rows + local files + remote objects in the config's
// destination, when it has one). A NULL/zero keep_latest keeps all.
// Best-effort. client is nil for a local destination — deleteBackupArtifacts
// already only deletes remotely when both a remote_key and a deleter are
// present, so a nil client here just means every pruned row's local file
// gets removed and nothing else.
func (h *TaskHandler) pruneConfigBackups(ctx context.Context, cfg generated.DatabaseBackupConfig, client *backup.DestinationClient) {
	if !cfg.KeepLatest.Valid || cfg.KeepLatest.Int32 <= 0 {
		return
	}
	keep := int(cfg.KeepLatest.Int32)
	rows, err := h.Queries.ListDatabaseBackupsByConfig(ctx, generated.ListDatabaseBackupsByConfigParams{
		BackupConfigID: cfg.ID,
		Limit:          1000,
	})
	if err != nil {
		slog.Warn("prune: list config backups", "error", err)
		return
	}
	if len(rows) <= keep {
		return
	}
	var del remoteDeleter
	if client != nil {
		del = func(ctx context.Context, keys []string) error { return client.DeleteFrom(ctx, keys) }
	}
	for _, b := range rows[keep:] { // newest-first
		h.deleteBackupArtifacts(ctx, b, del)
	}
}

// writeBackupArchive produces the gzipped backup archive into f according to the
// chosen method. Permanent failures are wrapped with asynq.SkipRetry.
func (h *TaskHandler) writeBackupArchive(ctx context.Context, rt runtime.ContainerRuntime, db generated.Database, creds map[string]string, method string, f *os.File, lg *runLog, target string) error {
	switch method {
	case "logical":
		lg.step("Running logical dump (%s)", backupScopeLabel(target))
		spec := dbDumpSpec(db.Type, creds, target)
		gz := gzip.NewWriter(f)
		var stderr bytes.Buffer
		exit, execErr := rt.ContainerExec(ctx, db.Slug, spec.dump, nil, gz, &stderr)
		gzErr := gz.Close()
		lg.raw(stderr.String()) // dump tools write warnings/errors to stderr
		if execErr != nil {
			return fmt.Errorf("exec dump: %w", execErr)
		}
		if exit != 0 {
			return errors.Join(fmt.Errorf("dump exited %d: %s", exit, strings.TrimSpace(stderr.String())), asynq.SkipRetry)
		}
		if gzErr != nil {
			return fmt.Errorf("flush dump gzip: %w", gzErr)
		}
		lg.step("Dump completed")
		return nil
	case "volume_snapshot":
		return h.snapshotVolume(ctx, rt, db, f, lg)
	case "command":
		return h.commandBackup(ctx, rt, db, f, lg)
	}
	return errors.Join(fmt.Errorf("unknown backup method %q", method), asynq.SkipRetry)
}

// snapshotVolume cold-tars an "other" database's data-dir volume into f. The
// database container is stopped for a consistent snapshot and always restarted.
func (h *TaskHandler) snapshotVolume(ctx context.Context, rt runtime.ContainerRuntime, db generated.Database, f *os.File, lg *runLog) error {
	dataDir := db.DataDir.String
	if dataDir == "" {
		return errors.Join(errors.New("data_dir is not set (permanent)"), asynq.SkipRetry)
	}
	helperImage := h.Config.DatabaseBackupHelperImage
	if err := rt.PullImage(ctx, helperImage); err != nil {
		return fmt.Errorf("pull helper image: %w", err)
	}

	// Cold snapshot: the database is offline while it is tarred. Reflect that in
	// the row so the UI doesn't show it as running, and always return it to
	// running (a snapshot is read-only, so the prior state is always "running").
	lg.step("Stopping database for cold volume snapshot")
	h.setDatabaseStatus(ctx, db.ID, statuspkg.DatabaseBackingUp)
	if err := rt.StopContainer(ctx, db.Slug); err != nil {
		slog.Warn("snapshot: stop database (may already be stopped)", "database_id", formatUUID(db.ID), "error", err)
	}
	defer func() {
		if err := rt.StartContainer(ctx, db.Slug); err != nil {
			slog.Error("snapshot: failed to restart database after backup", "database_id", formatUUID(db.ID), "error", err)
		}
		h.setDatabaseStatus(ctx, db.ID, statuspkg.DatabaseRunning)
	}()

	var stderr bytes.Buffer
	exit, err := rt.RunHelper(ctx, runtime.ContainerConfig{
		Image:   helperImage,
		Cmd:     []string{"tar", "czf", "-", "-C", dataDir, "."},
		Volumes: map[string]string{db.Slug + "-vol": dataDir},
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

// commandBackup runs the user's backup command in the running container (writing
// into $BELUNE_BACKUP_DIR), then tars that directory into f.
func (h *TaskHandler) commandBackup(ctx context.Context, rt runtime.ContainerRuntime, db generated.Database, f *os.File, lg *runLog) error {
	if !db.BackupCommand.Valid || db.BackupCommand.String == "" {
		return errors.Join(errors.New("backup_command is not set (permanent)"), asynq.SkipRetry)
	}

	lg.step("Running backup command")
	prep := fmt.Sprintf("rm -rf %s && mkdir -p %s && export BELUNE_BACKUP_DIR=%s && ( %s )",
		beluneBackupDir, beluneBackupDir, beluneBackupDir, db.BackupCommand.String)
	var stderr bytes.Buffer
	exit, err := rt.ContainerExec(ctx, db.Slug, []string{"sh", "-c", prep}, nil, nil, &stderr)
	lg.raw(stderr.String())
	if err != nil {
		return fmt.Errorf("exec backup command: %w", err)
	}
	if exit != 0 {
		return errors.Join(fmt.Errorf("backup command exited %d: %s", exit, strings.TrimSpace(stderr.String())), asynq.SkipRetry)
	}

	// Requires `sh` + `tar` in the "other" image; a missing tar surfaces here as
	// a non-zero exit ("tar: not found") in the returned error (documented in the
	// create dialog).
	lg.step("Archiving $BELUNE_BACKUP_DIR")
	var terr bytes.Buffer
	exit, err = rt.ContainerExec(ctx, db.Slug,
		[]string{"sh", "-c", fmt.Sprintf("tar czf - -C %s .", beluneBackupDir)}, nil, f, &terr)
	lg.raw(terr.String())
	if err != nil {
		return fmt.Errorf("exec tar: %w", err)
	}
	if exit != 0 {
		return errors.Join(fmt.Errorf("tar exited %d: %s", exit, strings.TrimSpace(terr.String())), asynq.SkipRetry)
	}
	lg.step("Command backup completed")

	// Best-effort cleanup.
	_, _ = rt.ContainerExec(ctx, db.Slug, []string{"sh", "-c", "rm -rf " + beluneBackupDir}, nil, nil, nil)
	return nil
}

// HandleRestoreDBTask restores a managed database from a recorded backup using
// the method that produced it. Local file is preferred; otherwise downloaded
// from S3.
func (h *TaskHandler) HandleRestoreDBTask(ctx context.Context, t *asynq.Task) error {
	var payload restoreDBPayload
	if err := json.Unmarshal(t.Payload(), &payload); err != nil {
		return fmt.Errorf("unmarshal restore_db payload: %w", err)
	}

	slog.Info("handling restore_db task", "database_id", payload.DatabaseID, "backup_id", payload.BackupID)

	dbID, err := parseUUID(payload.DatabaseID)
	if err != nil {
		return errors.Join(fmt.Errorf("invalid database_id (permanent): %w", err), asynq.SkipRetry)
	}
	backupID, err := parseUUID(payload.BackupID)
	if err != nil {
		return errors.Join(fmt.Errorf("invalid backup_id (permanent): %w", err), asynq.SkipRetry)
	}

	db, err := h.Queries.GetDatabase(ctx, dbID)
	if err != nil {
		return fmt.Errorf("get database: %w", err)
	}

	// Resolved once for the whole run: a backup that exec'd into one host and
	// wrote its volume on another would be silently wrong.
	rt, err := h.runtimeForDatabase(ctx, dbID)
	if err != nil {
		return err
	}

	backup, err := h.Queries.GetDatabaseBackup(ctx, backupID)
	if err != nil {
		return fmt.Errorf("get backup: %w", err)
	}
	if backup.DatabaseID != db.ID {
		return errors.Join(errors.New("backup does not belong to database (permanent)"), asynq.SkipRetry)
	}

	// Record the restore run so the UI can show restore history.
	run, err := h.Queries.InsertDatabaseRestore(ctx, generated.InsertDatabaseRestoreParams{
		DatabaseID: dbID,
		BackupID:   backupID,
	})
	if err != nil {
		return fmt.Errorf("insert restore run: %w", err)
	}
	lg := &runLog{}
	lg.flush = func(s string) {
		if err := h.Queries.SetDatabaseRestoreLog(ctx, generated.SetDatabaseRestoreLogParams{ID: run.ID, Log: pgtype.Text{String: s, Valid: true}}); err != nil {
			slog.Warn("restore_db: flush log", "restore_id", formatUUID(run.ID), "error", err)
		}
	}
	lg.step("Restore started (backup=%s, scope=%s)", formatUUID(backupID), backupScopeLabel(backup.TargetDatabase))

	method := dbBackupMethod(db)
	if method == "none" {
		h.failDatabaseRestoreLog(ctx, run.ID, fmt.Sprintf("restore not supported for database type %s", db.Type), lg)
		return errors.Join(fmt.Errorf("restore not supported for database type %s (permanent)", db.Type), asynq.SkipRetry)
	}

	creds, err := h.decryptDBCredentials(db)
	if err != nil {
		h.failDatabaseRestoreLog(ctx, run.ID, fmt.Sprintf("credentials: %v", err), lg)
		return errors.Join(fmt.Errorf("credentials (permanent): %w", err), asynq.SkipRetry)
	}

	dumpPath, cleanup, err := h.resolveBackupFile(ctx, backup)
	if err != nil {
		h.failDatabaseRestoreLog(ctx, run.ID, fmt.Sprintf("resolve backup file: %v", err), lg)
		return fmt.Errorf("resolve backup file: %w", err)
	}
	defer cleanup()
	lg.step("Backup archive ready; applying restore (method=%s)", method)

	if err := h.applyRestoreArchive(ctx, rt, db, creds, method, dumpPath, backup.TargetDatabase); err != nil {
		h.notifyRestore(ctx, db, false, err.Error())
		h.failDatabaseRestoreLog(ctx, run.ID, err.Error(), lg)
		return err
	}

	lg.step("Restore succeeded")
	h.finaliseDatabaseRestore(ctx, run.ID, generated.UpdateDatabaseRestoreParams{
		ID:         run.ID,
		FinishedAt: pgtype.Timestamptz{Time: time.Now(), Valid: true},
		Status:     "succeeded",
		Log:        pgtype.Text{String: lg.String(), Valid: true},
	})
	h.notifyRestore(ctx, db, true, "")
	slog.Info("database restored", "database_id", payload.DatabaseID, "backup_id", payload.BackupID, "method", method, "scope", backupScopeLabel(backup.TargetDatabase))
	return nil
}

func (h *TaskHandler) failDatabaseRestoreLog(ctx context.Context, id pgtype.UUID, errMsg string, lg *runLog) {
	slog.Error("database restore failed", "restore_id", formatUUID(id), "error", errMsg)
	logText := ""
	if lg != nil {
		lg.fail("Restore failed: %s", errMsg)
		logText = lg.String()
	}
	h.finaliseDatabaseRestore(ctx, id, generated.UpdateDatabaseRestoreParams{
		ID:         id,
		FinishedAt: pgtype.Timestamptz{Time: time.Now(), Valid: true},
		Status:     "failed",
		Error:      pgtype.Text{String: errMsg, Valid: true},
		Log:        pgtype.Text{String: logText, Valid: true},
	})
}

func (h *TaskHandler) finaliseDatabaseRestore(ctx context.Context, id pgtype.UUID, params generated.UpdateDatabaseRestoreParams) {
	if !id.Valid {
		return
	}
	if err := h.Queries.UpdateDatabaseRestore(ctx, params); err != nil {
		slog.Warn("restore_db: failed to update run record", "restore_id", formatUUID(id), "error", err)
	}
}

// notifyDatabaseOwner sends the database's owner a notification (bell +
// deep-link). No-op when no notifier is wired (e.g. tests).
func (h *TaskHandler) notifyDatabaseOwner(ctx context.Context, db generated.Database, notifType, title, body string) {
	if h.Notifier == nil && h.NotifyChannels == nil {
		return
	}
	link := fmt.Sprintf("/projects/%s/databases/%s", formatUUID(db.ProjectID), formatUUID(db.ID))

	// Fire provider channels once per event, before resolving the in-app recipient.
	h.dispatchToChannels(ctx, notify.Event{
		Type: notifType, Title: title, Body: body, Link: link, OccurredAt: time.Now(),
	})

	if h.Notifier == nil {
		return
	}
	owner, err := h.Queries.GetDatabaseOwnerUserID(ctx, db.ID)
	if err != nil {
		slog.Warn("notify: could not resolve database owner", "database_id", formatUUID(db.ID), "error", err)
		return
	}
	h.Notifier.Notify(formatUUID(owner.UserID), notifType, title, body, link)
}

// notifyRestore tells the database owner whether an async restore succeeded or
// failed, since restore is otherwise fire-and-forget.
func (h *TaskHandler) notifyRestore(ctx context.Context, db generated.Database, ok bool, detail string) {
	if ok {
		h.notifyDatabaseOwner(ctx, db, "database.restored", "Database restored",
			fmt.Sprintf("%s was restored from a backup.", db.Name))
	} else {
		h.notifyDatabaseOwner(ctx, db, "database.restore_failed", "Database restore failed",
			fmt.Sprintf("Restoring %s failed: %s", db.Name, detail))
	}
}

func (h *TaskHandler) applyRestoreArchive(ctx context.Context, rt runtime.ContainerRuntime, db generated.Database, creds map[string]string, method, dumpPath, target string) error {
	switch method {
	case "logical":
		// A single-database dump has no CREATE DATABASE, so restore fails if the
		// database was dropped. Recreate it first (no-op if it exists). Skipped for
		// cluster restores (pg_dumpall / --all-databases recreate databases).
		if err := h.ensureRestoreDatabase(ctx, rt, db, creds, target); err != nil {
			return err
		}

		f, err := os.Open(dumpPath)
		if err != nil {
			return fmt.Errorf("open dump file: %w", err)
		}
		defer f.Close()
		gz, err := gzip.NewReader(f)
		if err != nil {
			return fmt.Errorf("open gzip reader: %w", err)
		}
		defer gz.Close()

		spec := dbDumpSpec(db.Type, creds, target)
		var stderr bytes.Buffer
		exit, execErr := rt.ContainerExec(ctx, db.Slug, spec.restore, gz, nil, &stderr)
		if execErr != nil {
			return fmt.Errorf("exec restore: %w", execErr)
		}
		if exit != 0 {
			return errors.Join(fmt.Errorf("restore exited %d: %s", exit, strings.TrimSpace(stderr.String())), asynq.SkipRetry)
		}
		return nil
	case "volume_snapshot":
		return h.restoreVolume(ctx, rt, db, dumpPath)
	case "command":
		return h.commandRestore(ctx, rt, db, dumpPath)
	}
	return errors.Join(fmt.Errorf("unknown restore method %q", method), asynq.SkipRetry)
}

// ensureRestoreDatabase creates the restore target database if it was dropped, so
// a single-database restore doesn't fail with "database does not exist". No-op for
// cluster restores ("*", which recreate databases) and for engines that create
// databases implicitly (mongo). Best-effort: a genuine failure surfaces later as
// a clear restore error.
func (h *TaskHandler) ensureRestoreDatabase(ctx context.Context, rt runtime.ContainerRuntime, db generated.Database, creds map[string]string, target string) error {
	names, all := dumpTargets(creds, target)
	// "all" (pg_dumpall / --all-databases) and multi-db dumps (--create /
	// --databases) already recreate databases; only a single-db dump needs help.
	if all || len(names) != 1 {
		return nil
	}
	name := names[0]
	if name == "" {
		return nil
	}

	var cmd []string
	switch db.Type {
	case "postgres":
		// Quote as a SQL identifier (double internal quotes). CREATE DATABASE has
		// no IF NOT EXISTS, so tolerate the "already exists" error below.
		ident := `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
		cmd = []string{"sh", "-c", fmt.Sprintf("PGPASSWORD=%s psql -U %s -d postgres -c %s",
			shArg(creds["password"]), shArg(creds["user"]), shArg("CREATE DATABASE "+ident))}
	case "mysql":
		ident := "`" + strings.ReplaceAll(name, "`", "``") + "`"
		cmd = []string{"sh", "-c", fmt.Sprintf("MYSQL_PWD=%s mysql -u root -e %s",
			shArg(creds["root_password"]), shArg("CREATE DATABASE IF NOT EXISTS "+ident))}
	default:
		return nil // mongo creates databases on write
	}

	var stderr bytes.Buffer
	exit, err := rt.ContainerExec(ctx, db.Slug, cmd, nil, nil, &stderr)
	if err != nil {
		return fmt.Errorf("ensure restore database: %w", err)
	}
	// Postgres returns non-zero when the database already exists — that's fine.
	if exit != 0 && !strings.Contains(stderr.String(), "already exists") {
		slog.Warn("restore: ensure database returned non-zero (continuing)",
			"database_id", formatUUID(db.ID), "target", name, "stderr", strings.TrimSpace(stderr.String()))
	}
	return nil
}

// restoreVolume wipes the data-dir volume and untars the snapshot into it. The
// database is stopped for the swap and always restarted.
func (h *TaskHandler) restoreVolume(ctx context.Context, rt runtime.ContainerRuntime, db generated.Database, dumpPath string) error {
	dataDir := db.DataDir.String
	if dataDir == "" {
		return errors.Join(errors.New("data_dir is not set (permanent)"), asynq.SkipRetry)
	}
	helperImage := h.Config.DatabaseBackupHelperImage
	if err := rt.PullImage(ctx, helperImage); err != nil {
		return fmt.Errorf("pull helper image: %w", err)
	}

	f, err := os.Open(dumpPath)
	if err != nil {
		return fmt.Errorf("open snapshot file: %w", err)
	}
	defer f.Close()

	if err := rt.StopContainer(ctx, db.Slug); err != nil {
		slog.Warn("restore: stop database (may already be stopped)", "database_id", formatUUID(db.ID), "error", err)
	}
	defer func() {
		if err := rt.StartContainer(ctx, db.Slug); err != nil {
			slog.Error("restore: failed to restart database after restore", "database_id", formatUUID(db.ID), "error", err)
		}
	}()

	script := fmt.Sprintf("find %s -mindepth 1 -delete && tar xzf - -C %s", dataDir, dataDir)
	var stderr bytes.Buffer
	exit, err := rt.RunHelper(ctx, runtime.ContainerConfig{
		Image:   helperImage,
		Cmd:     []string{"sh", "-c", script},
		Volumes: map[string]string{db.Slug + "-vol": dataDir},
	}, f, nil, &stderr)
	if err != nil {
		return fmt.Errorf("restore helper: %w", err)
	}
	if exit != 0 {
		return errors.Join(fmt.Errorf("restore tar exited %d: %s", exit, strings.TrimSpace(stderr.String())), asynq.SkipRetry)
	}
	return nil
}

// commandRestore untars the archive into $BELUNE_BACKUP_DIR in the running
// container, then runs the user's restore command.
func (h *TaskHandler) commandRestore(ctx context.Context, rt runtime.ContainerRuntime, db generated.Database, dumpPath string) error {
	if !db.RestoreCommand.Valid || db.RestoreCommand.String == "" {
		return errors.Join(errors.New("restore_command is not set (permanent)"), asynq.SkipRetry)
	}

	f, err := os.Open(dumpPath)
	if err != nil {
		return fmt.Errorf("open backup file: %w", err)
	}
	defer f.Close()

	unpack := fmt.Sprintf("rm -rf %s && mkdir -p %s && tar xzf - -C %s", beluneBackupDir, beluneBackupDir, beluneBackupDir)
	var uerr bytes.Buffer
	exit, err := rt.ContainerExec(ctx, db.Slug, []string{"sh", "-c", unpack}, f, nil, &uerr)
	if err != nil {
		return fmt.Errorf("exec unpack: %w", err)
	}
	if exit != 0 {
		return errors.Join(fmt.Errorf("unpack exited %d: %s", exit, strings.TrimSpace(uerr.String())), asynq.SkipRetry)
	}

	run := fmt.Sprintf("export BELUNE_BACKUP_DIR=%s && ( %s )", beluneBackupDir, db.RestoreCommand.String)
	var rerr bytes.Buffer
	exit, err = rt.ContainerExec(ctx, db.Slug, []string{"sh", "-c", run}, nil, nil, &rerr)
	if err != nil {
		return fmt.Errorf("exec restore command: %w", err)
	}
	if exit != 0 {
		return errors.Join(fmt.Errorf("restore command exited %d: %s", exit, strings.TrimSpace(rerr.String())), asynq.SkipRetry)
	}

	_, _ = rt.ContainerExec(ctx, db.Slug, []string{"sh", "-c", "rm -rf " + beluneBackupDir}, nil, nil, nil)
	return nil
}

// resolveBackupFile returns a readable path to the backup archive. If the local
// copy is missing it downloads the remote object to a temp file; the returned
// cleanup removes any temp file (no-op for the local copy).
func (h *TaskHandler) resolveBackupFile(ctx context.Context, backup generated.DatabaseBackup) (string, func(), error) {
	noop := func() {}
	if backup.LocalPath.Valid {
		if _, err := os.Stat(backup.LocalPath.String); err == nil {
			return backup.LocalPath.String, noop, nil
		}
	}
	if !backup.RemoteKey.Valid {
		return "", noop, errors.New("backup has no local file and no remote copy")
	}

	tmp := filepath.Join(os.TempDir(), "belune-restore-"+filepath.Base(backup.RemoteKey.String))
	cleanup := func() { _ = os.Remove(tmp) }

	// Prefer the destination this backup was recorded as written to. Resolving
	// through the config instead would follow a pointer that has moved: repoint
	// or delete the config and every prior backup aims at the wrong bucket.
	if copy, err := h.clientForRecordedDatabaseBackup(ctx, backup.ID); err != nil {
		return "", noop, err
	} else if copy != nil {
		if err := copy.client.Download(ctx, copy.key, tmp); err != nil {
			return "", noop, err
		}
		return tmp, cleanup, nil
	}

	// No recorded location: a backup written before 000061, or one whose config
	// was deleted. Fall back to the config-driven path, unchanged.
	if backup.BackupConfigID.Valid {
		if h.BackupDestinations == nil {
			return "", noop, errors.New("backup destinations service is not configured")
		}
		client, err := h.BackupDestinations.ClientForConfig(ctx, backup.BackupConfigID)
		if err != nil {
			return "", noop, fmt.Errorf("resolve backup destination: %w", err)
		}
		if err := client.Download(ctx, backup.RemoteKey.String, tmp); err != nil {
			return "", noop, err
		}
		return tmp, cleanup, nil
	}

	// Ad-hoc backup: the global env-var S3 target.
	if h.BackupService == nil || !h.BackupService.Enabled() {
		return "", noop, errors.New("backup is remote-only but S3 is not configured")
	}
	if err := h.BackupService.Download(ctx, backup.RemoteKey.String, tmp); err != nil {
		return "", noop, err
	}
	return tmp, cleanup, nil
}

func (h *TaskHandler) failDatabaseBackup(ctx context.Context, id pgtype.UUID, errMsg string) {
	h.failDatabaseBackupLog(ctx, id, errMsg, nil)
}

// failDatabaseBackupLog records a failed run with the accumulated step log
// (lg may be nil for callers without one, e.g. the upgrade flow).
func (h *TaskHandler) failDatabaseBackupLog(ctx context.Context, id pgtype.UUID, errMsg string, lg *runLog) {
	slog.Error("database backup failed", "backup_id", formatUUID(id), "error", errMsg)
	logText := ""
	if lg != nil {
		lg.fail("Backup failed: %s", errMsg)
		logText = lg.String()
	}
	h.finaliseDatabaseBackup(ctx, id, generated.UpdateDatabaseBackupParams{
		ID:         id,
		FinishedAt: pgtype.Timestamptz{Time: time.Now(), Valid: true},
		Status:     "failed",
		Error:      pgtype.Text{String: errMsg, Valid: true},
		Log:        logText,
	})
}

func (h *TaskHandler) finaliseDatabaseBackup(ctx context.Context, id pgtype.UUID, params generated.UpdateDatabaseBackupParams) {
	if !id.Valid {
		return
	}
	if err := h.Queries.UpdateDatabaseBackup(ctx, params); err != nil {
		slog.Warn("backup_db: failed to update run record", "backup_id", formatUUID(id), "error", err)
	}
}
