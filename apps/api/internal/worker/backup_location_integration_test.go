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
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/localstack"

	"github.com/belune-hq/belune/internal/config"
	"github.com/belune-hq/belune/internal/pkg/crypto"
	"github.com/belune-hq/belune/internal/service"
	"github.com/belune-hq/belune/internal/service/backup"
	"github.com/belune-hq/belune/internal/store/generated"
	"github.com/belune-hq/belune/internal/testutil"
)

// s3Endpoint starts a LocalStack (S3-only) container and returns its
// host:port. MinIO used to serve this role, but its image is no longer
// anonymously pullable from either quay.io or Docker Hub — see startS3 in
// internal/service/backup/service_test.go for the fuller reasoning.
func s3Endpoint(t *testing.T) string {
	t.Helper()
	ctx := context.Background()
	// Pinned, not :latest — a moving tag is exactly how the MinIO removal
	// reached CI as a surprise in the first place.
	container, err := localstack.Run(ctx, "localstack/localstack:4.14.0",
		testcontainers.WithEnv(map[string]string{"SERVICES": "s3"}),
	)
	require.NoError(t, err)
	t.Cleanup(func() { _ = container.Terminate(context.Background()) })

	endpoint, err := container.PortEndpoint(ctx, "4566/tcp", "http")
	require.NoError(t, err)
	return strings.TrimPrefix(strings.TrimPrefix(endpoint, "http://"), "https://")
}

// makeBucket creates bucket on the S3-compatible endpoint. The
// DestinationClient has no create-bucket call (destinations are expected to
// pre-exist), so this borrows the global backup service's EnsureBucket to
// set the fixture up.
func makeBucket(t *testing.T, endpoint, bucket string) {
	t.Helper()
	svc := backup.New(&config.Config{
		BackupRemoteEnabled: true,
		BackupS3Endpoint:    endpoint,
		BackupS3Region:      "us-east-1",
		BackupS3Bucket:      bucket,
		BackupS3AccessKey:   "test",
		BackupS3SecretKey:   "test",
		BackupS3UseSSL:      false,
	})
	require.NoError(t, svc.EnsureBucket(context.Background()))
}

// seedDestination inserts a backup destination pointing at bucket on the
// S3-compatible endpoint, with the test keyring's encrypted credentials.
func seedDestination(t *testing.T, projectID pgtype.UUID, name, endpoint, bucket string) generated.BackupDestination {
	t.Helper()
	keyring, err := crypto.ParseKeyringEnv("", testutil.TestEncryptionKey, "")
	require.NoError(t, err)
	credsJSON, err := json.Marshal(map[string]string{
		"access_key": "test", "secret_key": "test",
	})
	require.NoError(t, err)
	enc, err := keyring.Encrypt(credsJSON)
	require.NoError(t, err)

	dest, err := testQueries.CreateBackupDestination(context.Background(), generated.CreateBackupDestinationParams{
		ProjectID:            projectID,
		Name:                 name,
		Provider:             "minio",
		Endpoint:             endpoint,
		Region:               "us-east-1",
		Bucket:               bucket,
		UseSsl:               false,
		CredentialsEncrypted: enc,
	})
	require.NoError(t, err)
	return dest
}

// TestRestoreFollowsRecordedLocation_NotRepointedConfig is the W1 regression: a
// backup must be restored from the bucket it was written to, not from wherever
// its config points at restore time. The config is a mutable pointer, so
// resolving through it sends the download at a bucket the object was never in.
func TestRestoreFollowsRecordedLocation_NotRepointedConfig(t *testing.T) {
	if os.Getenv("BELUNE_DOCKER_INTEGRATION") == "" {
		t.Skip("set BELUNE_DOCKER_INTEGRATION=1 to run the backup-location test (needs Docker)")
	}
	ctx := context.Background()

	endpoint := s3Endpoint(t)
	makeBucket(t, endpoint, "bucket-a")
	makeBucket(t, endpoint, "bucket-b")

	var restoredLen int
	rt := &testutil.MockContainerRuntime{
		ExecFunc: func(_ context.Context, _ string, _ []string, stdin io.Reader, stdout, _ io.Writer) (int, error) {
			if stdin != nil {
				b, _ := io.ReadAll(stdin)
				restoredLen = len(b)
				return 0, nil
			}
			if stdout != nil {
				_, _ = stdout.Write([]byte(strings.Repeat("SQL ", 100)))
			}
			return 0, nil
		},
	}
	keyring, err := crypto.ParseKeyringEnv("", testutil.TestEncryptionKey, "")
	require.NoError(t, err)

	h := newTestHandler(rt, nil)
	h.Config.DatabaseBackupDir = t.TempDir()
	h.BackupDestinations = service.NewBackupDestinationService(testQueries, keyring)

	db := seedDatabase(t)
	destA := seedDestination(t, db.ProjectID, "bucket-a", endpoint, "bucket-a")
	destB := seedDestination(t, db.ProjectID, "bucket-b", endpoint, "bucket-b")

	cfg, err := testQueries.CreateDatabaseBackupConfig(ctx, generated.CreateDatabaseBackupConfigParams{
		DatabaseID:    db.ID,
		DestinationID: destA.ID,
		Schedule:      "0 3 * * *",
		Enabled:       true,
	})
	require.NoError(t, err)

	// Back up through the config → the object lands in bucket-a and the local
	// copy is dropped, so restore has to go remote.
	bp, _ := json.Marshal(map[string]string{
		"database_id":      dbIDStr(db),
		"backup_config_id": uuidString(cfg.ID),
	})
	require.NoError(t, h.HandleBackupDBTask(ctx, asynq.NewTask("backup_db", bp)))

	backups, err := testQueries.ListDatabaseBackups(ctx, generated.ListDatabaseBackupsParams{
		DatabaseID: db.ID, Limit: 5,
	})
	require.NoError(t, err)
	require.NotEmpty(t, backups)
	require.True(t, backups[0].RemoteKey.Valid, "backup should have been uploaded to the destination")
	require.False(t, backups[0].LocalPath.Valid, "local copy should be dropped after a successful upload")

	// Repoint the config at a different destination. Everything already written
	// still lives in bucket-a; only new runs belong in bucket-b.
	_, err = testQueries.UpdateDatabaseBackupConfig(ctx, generated.UpdateDatabaseBackupConfigParams{
		ID:            cfg.ID,
		DestinationID: destB.ID,
		Prefix:        cfg.Prefix,
		Schedule:      cfg.Schedule,
		KeepLatest:    cfg.KeepLatest,
		Enabled:       cfg.Enabled,
	})
	require.NoError(t, err)

	rp, _ := json.Marshal(map[string]string{
		"database_id": dbIDStr(db),
		"backup_id":   uuidString(backups[0].ID),
	})
	require.NoError(t, h.HandleRestoreDBTask(ctx, asynq.NewTask("restore_db", rp)),
		"restore must download from the bucket the backup was written to, not the config's current one")
	require.NotZero(t, restoredLen, "restore streamed nothing to the database")
}

// TestRestoreSurvivesDeletedConfig covers the orphan population: deleting a
// config nulls backup_config_id on every run it produced, which used to leave
// the destination unknowable. The recorded location outlives the config.
func TestRestoreSurvivesDeletedConfig(t *testing.T) {
	if os.Getenv("BELUNE_DOCKER_INTEGRATION") == "" {
		t.Skip("set BELUNE_DOCKER_INTEGRATION=1 to run the backup-location test (needs Docker)")
	}
	ctx := context.Background()

	endpoint := s3Endpoint(t)
	makeBucket(t, endpoint, "orphan-bucket")

	var restoredLen int
	rt := &testutil.MockContainerRuntime{
		ExecFunc: func(_ context.Context, _ string, _ []string, stdin io.Reader, stdout, _ io.Writer) (int, error) {
			if stdin != nil {
				b, _ := io.ReadAll(stdin)
				restoredLen = len(b)
				return 0, nil
			}
			if stdout != nil {
				_, _ = stdout.Write([]byte(strings.Repeat("SQL ", 100)))
			}
			return 0, nil
		},
	}
	keyring, err := crypto.ParseKeyringEnv("", testutil.TestEncryptionKey, "")
	require.NoError(t, err)

	h := newTestHandler(rt, nil)
	h.Config.DatabaseBackupDir = t.TempDir()
	h.BackupDestinations = service.NewBackupDestinationService(testQueries, keyring)

	db := seedDatabase(t)
	dest := seedDestination(t, db.ProjectID, "orphan-bucket", endpoint, "orphan-bucket")
	cfg, err := testQueries.CreateDatabaseBackupConfig(ctx, generated.CreateDatabaseBackupConfigParams{
		DatabaseID:    db.ID,
		DestinationID: dest.ID,
		Schedule:      "0 3 * * *",
		Enabled:       true,
	})
	require.NoError(t, err)

	bp, _ := json.Marshal(map[string]string{
		"database_id":      dbIDStr(db),
		"backup_config_id": uuidString(cfg.ID),
	})
	require.NoError(t, h.HandleBackupDBTask(ctx, asynq.NewTask("backup_db", bp)))
	backups, err := testQueries.ListDatabaseBackups(ctx, generated.ListDatabaseBackupsParams{
		DatabaseID: db.ID, Limit: 5,
	})
	require.NoError(t, err)
	require.NotEmpty(t, backups)

	// Drop the config directly: ON DELETE SET NULL orphans the run, so nothing
	// is left to re-derive the destination from except the location row.
	require.NoError(t, testQueries.DeleteDatabaseBackupConfig(ctx, cfg.ID))
	orphaned, err := testQueries.GetDatabaseBackup(ctx, backups[0].ID)
	require.NoError(t, err)
	require.False(t, orphaned.BackupConfigID.Valid, "deleting the config should have nulled the run's config id")

	rp, _ := json.Marshal(map[string]string{
		"database_id": dbIDStr(db),
		"backup_id":   uuidString(backups[0].ID),
	})
	require.NoError(t, h.HandleRestoreDBTask(ctx, asynq.NewTask("restore_db", rp)),
		"a backup orphaned by config deletion must still restore from its recorded location")
	require.NotZero(t, restoredLen, "restore streamed nothing to the database")
}
