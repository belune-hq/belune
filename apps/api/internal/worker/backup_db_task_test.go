package worker_test

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/hibiken/asynq"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/belune-hq/belune/internal/pkg/crypto"
	"github.com/belune-hq/belune/internal/runtime"
	"github.com/belune-hq/belune/internal/service"
	"github.com/belune-hq/belune/internal/status"
	"github.com/belune-hq/belune/internal/store/generated"
	"github.com/belune-hq/belune/internal/testutil"
	"github.com/belune-hq/belune/internal/worker"
)

// otherVolumeSnapshot / otherCommand configure a seeded "other" database for the
// respective backup mode.
func otherVolumeSnapshot(p *generated.CreateDatabaseParams) {
	p.Type = "other"
	p.BackupMode = "volume_snapshot"
	p.Image = pgtype.Text{String: "clickhouse/clickhouse-server:24.3", Valid: true}
	p.ContainerPort = pgtype.Int4{Int32: 9000, Valid: true}
	p.DataDir = pgtype.Text{String: "/var/lib/clickhouse", Valid: true}
}

func otherCommand(p *generated.CreateDatabaseParams) {
	p.Type = "other"
	p.BackupMode = "command"
	p.Image = pgtype.Text{String: "myimage:1", Valid: true}
	p.ContainerPort = pgtype.Int4{Int32: 1234, Valid: true}
	p.DataDir = pgtype.Text{String: "/data", Valid: true}
	p.BackupCommand = pgtype.Text{String: "mydump", Valid: true}
	p.RestoreCommand = pgtype.Text{String: "myrestore", Valid: true}
}

// backupAndList runs a backup task for db and returns its recorded backup rows.
func backupAndList(t *testing.T, h *worker.TaskHandler, db generated.Database) []generated.DatabaseBackup {
	t.Helper()
	ctx := context.Background()
	bp, _ := json.Marshal(map[string]string{"database_id": dbIDStr(db)})
	require.NoError(t, h.HandleBackupDBTask(ctx, asynq.NewTask("backup_db", bp)))
	backups, err := testQueries.ListDatabaseBackups(ctx, generated.ListDatabaseBackupsParams{DatabaseID: db.ID, Limit: 5})
	require.NoError(t, err)
	require.NotEmpty(t, backups)
	return backups
}

// seedDatabase inserts a user, project, and a running database row with
// encrypted credentials. opts can override the create params (type, backup
// mode, data dir, etc.) before insert.
func seedDatabase(t *testing.T, opts ...func(*generated.CreateDatabaseParams)) generated.Database {
	t.Helper()
	ctx := context.Background()
	suffix := randomSuffix(t)

	user, err := testQueries.CreateUser(ctx, generated.CreateUserParams{
		Email:        "db-" + suffix + "@test.com",
		PasswordHash: "x",
		Role:         "admin",
	})
	require.NoError(t, err)
	project, err := testQueries.CreateProject(ctx, generated.CreateProjectParams{
		Name:     "DB Project",
		Slug:     "dbproj-" + suffix,
		UserID:   user.ID,
		ServerID: testutil.LocalServerID(t, ctx, testQueries),
	})
	require.NoError(t, err)

	keyring, err := crypto.ParseKeyringEnv("", testutil.TestEncryptionKey, "")
	require.NoError(t, err)
	credsJSON, err := json.Marshal(map[string]string{
		"user": "u", "password": "p", "database": "d", "username": "u",
		"root_password": "rootp",
	})
	require.NoError(t, err)
	enc, err := keyring.Encrypt(credsJSON)
	require.NoError(t, err)

	params := generated.CreateDatabaseParams{
		ProjectID:            project.ID,
		Type:                 "postgres",
		Name:                 "db-" + suffix,
		Slug:                 project.Slug + "-db",
		Version:              "16",
		Status:               status.DatabaseRunning,
		InternalHost:         pgtype.Text{String: project.Slug + "-db", Valid: true},
		InternalPort:         pgtype.Int4{Int32: 5432, Valid: true},
		CredentialsEncrypted: enc,
		BackupMode:           "none",
	}
	for _, fn := range opts {
		fn(&params)
	}
	db, err := testQueries.CreateDatabase(ctx, params)
	require.NoError(t, err)
	return db
}

func dbIDStr(db generated.Database) string { return uuidString(db.ID) }

func TestHandleBackupDBTask_Logical(t *testing.T) {
	ctx := context.Background()
	rt := &testutil.MockContainerRuntime{
		ExecFunc: func(_ context.Context, _ string, _ []string, _ io.Reader, stdout, _ io.Writer) (int, error) {
			_, _ = stdout.Write([]byte("-- SQL DUMP --"))
			return 0, nil
		},
	}
	h := newTestHandler(rt, nil)
	h.Config.DatabaseBackupDir = t.TempDir()

	db := seedDatabase(t)
	payload, _ := json.Marshal(map[string]string{"database_id": dbIDStr(db)})
	require.NoError(t, h.HandleBackupDBTask(ctx, asynq.NewTask("backup_db", payload)))

	backups, err := testQueries.ListDatabaseBackups(ctx, generated.ListDatabaseBackupsParams{DatabaseID: db.ID, Limit: 10})
	require.NoError(t, err)
	require.Len(t, backups, 1)
	assert.Equal(t, "succeeded", backups[0].Status)
	assert.Greater(t, backups[0].SizeBytes, int64(0))
	assert.True(t, backups[0].LocalPath.Valid)
	_, statErr := os.Stat(backups[0].LocalPath.String)
	assert.NoError(t, statErr)
}

// A copy on disk names the host holding it; a copy that only exists in a bucket
// names none. The servers migration backfilled existing rows on exactly that
// rule, so the writer has to follow it too — otherwise every backup taken after
// the upgrade records NULL and claims, in the column's own vocabulary, to be
// somewhere other than this host.
func TestHandleBackupDBTask_LocalCopyRecordsItsServer(t *testing.T) {
	ctx := context.Background()
	rt := &testutil.MockContainerRuntime{
		ExecFunc: func(_ context.Context, _ string, _ []string, _ io.Reader, stdout, _ io.Writer) (int, error) {
			_, _ = stdout.Write([]byte("-- SQL DUMP --"))
			return 0, nil
		},
	}
	h := newTestHandler(rt, nil)
	h.Config.DatabaseBackupDir = t.TempDir()
	keyring, err := crypto.ParseKeyringEnv("", testutil.TestEncryptionKey, "")
	require.NoError(t, err)
	h.BackupDestinations = service.NewBackupDestinationService(testQueries, keyring)

	db := seedDatabase(t)
	// A local destination keeps the staged archive on-host, which is the only
	// way to get a location row with a local_path.
	dest, err := testQueries.CreateBackupDestination(ctx, generated.CreateBackupDestinationParams{
		ProjectID:            db.ProjectID,
		Name:                 "on-host",
		Provider:             "local",
		CredentialsEncrypted: []byte{},
	})
	require.NoError(t, err)
	cfg, err := testQueries.CreateDatabaseBackupConfig(ctx, generated.CreateDatabaseBackupConfigParams{
		DatabaseID:    db.ID,
		DestinationID: dest.ID,
		Schedule:      "0 3 * * *",
		KeepLatest:    pgtype.Int4{Int32: 3, Valid: true},
		Enabled:       true,
	})
	require.NoError(t, err)

	payload, _ := json.Marshal(map[string]string{
		"database_id":      dbIDStr(db),
		"backup_config_id": uuidString(cfg.ID),
	})
	require.NoError(t, h.HandleBackupDBTask(ctx, asynq.NewTask("backup_db", payload)))

	backups, err := testQueries.ListDatabaseBackups(ctx, generated.ListDatabaseBackupsParams{DatabaseID: db.ID, Limit: 5})
	require.NoError(t, err)
	require.NotEmpty(t, backups)

	locations, err := testQueries.ListLocationsForDatabaseBackup(ctx, backups[0].ID)
	require.NoError(t, err)
	require.Len(t, locations, 1)
	require.True(t, locations[0].LocalPath.Valid, "a local destination keeps the archive on-host")
	assert.Equal(t, testutil.LocalServerID(t, ctx, testQueries), locations[0].ServerID,
		"an on-disk copy must record the host that holds it")
}

// orderRuntime records the relative order of stop/start/tar so the cold-snapshot
// bracket (stop → tar → start) can be asserted.
type orderRuntime struct {
	*testutil.MockContainerRuntime
	order *[]string
}

func (r *orderRuntime) StopContainer(ctx context.Context, id string) error {
	*r.order = append(*r.order, "stop")
	return r.MockContainerRuntime.StopContainer(ctx, id)
}

func (r *orderRuntime) StartContainer(ctx context.Context, id string) error {
	*r.order = append(*r.order, "start")
	return r.MockContainerRuntime.StartContainer(ctx, id)
}

func TestHandleBackupDBTask_VolumeSnapshotOrdering(t *testing.T) {
	ctx := context.Background()
	var order []string
	mock := &testutil.MockContainerRuntime{
		RunHelperFunc: func(_ context.Context, _ runtime.ContainerConfig, _ io.Reader, stdout, _ io.Writer) (int, error) {
			order = append(order, "tar")
			_, _ = stdout.Write([]byte("TAR ARCHIVE"))
			return 0, nil
		},
	}
	rt := &orderRuntime{MockContainerRuntime: mock, order: &order}
	h := newTestHandler(rt, nil)
	h.Config.DatabaseBackupDir = t.TempDir()
	h.Config.DatabaseBackupHelperImage = "alpine:3.20"

	db := seedDatabase(t, otherVolumeSnapshot)
	payload, _ := json.Marshal(map[string]string{"database_id": dbIDStr(db)})
	require.NoError(t, h.HandleBackupDBTask(ctx, asynq.NewTask("backup_db", payload)))

	// Cold snapshot must stop the DB, tar the volume, then restart it.
	assert.Equal(t, []string{"stop", "tar", "start"}, order)

	backups, err := testQueries.ListDatabaseBackups(ctx, generated.ListDatabaseBackupsParams{DatabaseID: db.ID, Limit: 10})
	require.NoError(t, err)
	require.Len(t, backups, 1)
	assert.Equal(t, "succeeded", backups[0].Status)
}

func TestHandleBackupDBTask_Command(t *testing.T) {
	ctx := context.Background()
	var cmds []string
	rt := &testutil.MockContainerRuntime{
		ExecFunc: func(_ context.Context, _ string, cmd []string, _ io.Reader, stdout, _ io.Writer) (int, error) {
			cmds = append(cmds, strings.Join(cmd, " "))
			if stdout != nil {
				_, _ = stdout.Write([]byte("ARCHIVE"))
			}
			return 0, nil
		},
	}
	h := newTestHandler(rt, nil)
	h.Config.DatabaseBackupDir = t.TempDir()

	db := seedDatabase(t, otherCommand)
	payload, _ := json.Marshal(map[string]string{"database_id": dbIDStr(db)})
	require.NoError(t, h.HandleBackupDBTask(ctx, asynq.NewTask("backup_db", payload)))

	// Expect: prep (runs user command), tar the dir, cleanup.
	require.Len(t, cmds, 3)
	assert.Contains(t, cmds[0], "mydump")
	assert.Contains(t, cmds[0], "BELUNE_BACKUP_DIR")
	assert.Contains(t, cmds[1], "tar czf -")
	assert.Contains(t, cmds[2], "rm -rf")
}

func TestHandleRestoreDBTask_Logical(t *testing.T) {
	ctx := context.Background()
	var restoredLen int
	rt := &testutil.MockContainerRuntime{
		ExecFunc: func(_ context.Context, _ string, _ []string, stdin io.Reader, stdout, _ io.Writer) (int, error) {
			if stdin != nil { // restore streams the dump in
				b, _ := io.ReadAll(stdin)
				restoredLen = len(b)
				return 0, nil
			}
			if stdout != nil { // dump
				_, _ = stdout.Write([]byte(strings.Repeat("SQL ", 100)))
			}
			return 0, nil
		},
	}
	h := newTestHandler(rt, nil)
	h.Config.DatabaseBackupDir = t.TempDir()

	db := seedDatabase(t)
	backups := backupAndList(t, h, db)
	rp, _ := json.Marshal(map[string]string{"database_id": dbIDStr(db), "backup_id": uuidString(backups[0].ID)})
	require.NoError(t, h.HandleRestoreDBTask(ctx, asynq.NewTask("restore_db", rp)))
	assert.Greater(t, restoredLen, 0, "restore should stream the dump via stdin")
}

func TestHandleRestoreDBTask_VolumeSnapshot(t *testing.T) {
	ctx := context.Background()
	var order []string
	var restoreStdinLen int
	mock := &testutil.MockContainerRuntime{
		RunHelperFunc: func(_ context.Context, _ runtime.ContainerConfig, stdin io.Reader, stdout, _ io.Writer) (int, error) {
			order = append(order, "tar")
			if stdin != nil { // restore extracts the archive
				b, _ := io.ReadAll(stdin)
				restoreStdinLen = len(b)
				return 0, nil
			}
			_, _ = stdout.Write([]byte("TAR ARCHIVE DATA"))
			return 0, nil
		},
	}
	rt := &orderRuntime{MockContainerRuntime: mock, order: &order}
	h := newTestHandler(rt, nil)
	h.Config.DatabaseBackupDir = t.TempDir()
	h.Config.DatabaseBackupHelperImage = "alpine:3.20"

	db := seedDatabase(t, otherVolumeSnapshot)
	backups := backupAndList(t, h, db)
	order = nil // isolate the restore's stop/tar/start

	rp, _ := json.Marshal(map[string]string{"database_id": dbIDStr(db), "backup_id": uuidString(backups[0].ID)})
	require.NoError(t, h.HandleRestoreDBTask(ctx, asynq.NewTask("restore_db", rp)))
	assert.Equal(t, []string{"stop", "tar", "start"}, order)
	assert.Greater(t, restoreStdinLen, 0)
}

func TestHandleRestoreDBTask_Command(t *testing.T) {
	ctx := context.Background()
	var cmds []string
	rt := &testutil.MockContainerRuntime{
		ExecFunc: func(_ context.Context, _ string, cmd []string, stdin io.Reader, stdout, _ io.Writer) (int, error) {
			cmds = append(cmds, strings.Join(cmd, " "))
			if stdout != nil {
				_, _ = stdout.Write([]byte("ARCHIVE"))
			}
			if stdin != nil {
				_, _ = io.ReadAll(stdin)
			}
			return 0, nil
		},
	}
	h := newTestHandler(rt, nil)
	h.Config.DatabaseBackupDir = t.TempDir()

	db := seedDatabase(t, otherCommand)
	backups := backupAndList(t, h, db)
	cmds = nil // isolate the restore exec sequence

	rp, _ := json.Marshal(map[string]string{"database_id": dbIDStr(db), "backup_id": uuidString(backups[0].ID)})
	require.NoError(t, h.HandleRestoreDBTask(ctx, asynq.NewTask("restore_db", rp)))
	require.Len(t, cmds, 3)
	assert.Contains(t, cmds[0], "tar xzf")
	assert.Contains(t, cmds[1], "myrestore")
	assert.Contains(t, cmds[2], "rm -rf")
}

func TestHandleUpgradeDBTask_RollbackOnRestoreFailure(t *testing.T) {
	ctx := context.Background()
	dumpBody := []byte(strings.Repeat(
		"CREATE TABLE t (id int, name text, created timestamptz);\n"+
			"INSERT INTO t VALUES (1, 'alpha', now()), (2, 'beta', now());\n", 40))
	restoreCalls := 0
	rt := &testutil.MockContainerRuntime{
		ExecFunc: func(_ context.Context, _ string, _ []string, stdin io.Reader, stdout, _ io.Writer) (int, error) {
			if stdin != nil { // a restore: fail the forward one, succeed the rollback
				restoreCalls++
				if restoreCalls == 1 {
					return 1, nil
				}
				return 0, nil
			}
			if stdout != nil { // dump
				_, _ = stdout.Write(dumpBody)
			}
			return 0, nil
		},
	}
	h := newTestHandler(rt, nil)
	h.Config.DatabaseBackupDir = t.TempDir()

	db := seedDatabase(t) // postgres 16
	payload, _ := json.Marshal(map[string]string{"database_id": dbIDStr(db), "target_version": "17"})
	// Rollback succeeds → task returns nil (DB healthy at old version).
	require.NoError(t, h.HandleUpgradeDBTask(ctx, asynq.NewTask("upgrade_db", payload)))

	updated, err := testQueries.GetDatabase(ctx, db.ID)
	require.NoError(t, err)
	assert.Equal(t, "16", updated.Version, "version must roll back to the original")
	assert.Equal(t, status.DatabaseRunning, updated.Status)
	assert.Equal(t, 2, restoreCalls, "forward restore + rollback restore")
}

func TestReconcileInterruptedUpgrades(t *testing.T) {
	ctx := context.Background()
	h := newTestHandler(&testutil.MockContainerRuntime{}, nil)

	stuck := seedDatabase(t, func(p *generated.CreateDatabaseParams) { p.Status = "upgrading" })
	snapshotting := seedDatabase(t, func(p *generated.CreateDatabaseParams) { p.Status = "backing_up" })
	running := seedDatabase(t) // must be left alone

	h.ReconcileInterruptedUpgrades(ctx)

	got, err := testQueries.GetDatabase(ctx, stuck.ID)
	require.NoError(t, err)
	assert.Equal(t, status.DatabaseFailed, got.Status, "interrupted upgrade should be marked failed")

	snap, err := testQueries.GetDatabase(ctx, snapshotting.ID)
	require.NoError(t, err)
	assert.Equal(t, status.DatabaseRunning, snap.Status, "interrupted snapshot should recover to running")

	ok, err := testQueries.GetDatabase(ctx, running.ID)
	require.NoError(t, err)
	assert.Equal(t, status.DatabaseRunning, ok.Status, "running databases must be untouched")
}

func TestHandleUpgradeDBTask_HappyPath(t *testing.T) {
	ctx := context.Background()
	// A realistic dump body so the gzipped archive clears the pre-upgrade
	// dump-validity floor (the destructive wipe is gated on a non-empty dump).
	dumpBody := []byte(strings.Repeat(
		"CREATE TABLE t (id int, name text, created timestamptz);\n"+
			"INSERT INTO t VALUES (1, 'alpha', now()), (2, 'beta', now());\n", 40))
	rt := &testutil.MockContainerRuntime{
		ExecFunc: func(_ context.Context, _ string, _ []string, _ io.Reader, stdout, _ io.Writer) (int, error) {
			if stdout != nil {
				_, _ = stdout.Write(dumpBody)
			}
			return 0, nil
		},
	}
	h := newTestHandler(rt, nil)
	h.Config.DatabaseBackupDir = t.TempDir()

	db := seedDatabase(t)
	payload, _ := json.Marshal(map[string]string{"database_id": dbIDStr(db), "target_version": "17"})
	require.NoError(t, h.HandleUpgradeDBTask(ctx, asynq.NewTask("upgrade_db", payload)))

	updated, err := testQueries.GetDatabase(ctx, db.ID)
	require.NoError(t, err)
	assert.Equal(t, "17", updated.Version)
	assert.Equal(t, status.DatabaseRunning, updated.Status)

	// The pre-upgrade dump is recorded as a succeeded backup (rollback artifact).
	backups, err := testQueries.ListDatabaseBackups(ctx, generated.ListDatabaseBackupsParams{DatabaseID: db.ID, Limit: 10})
	require.NoError(t, err)
	require.Len(t, backups, 1)
	assert.Equal(t, "succeeded", backups[0].Status)
}
