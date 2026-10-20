package worker_test

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/hibiken/asynq"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/localstack"

	"github.com/belune-hq/belune/internal/config"
	"github.com/belune-hq/belune/internal/service/backup"
	"github.com/belune-hq/belune/internal/testutil"
)

// TestRestoreFromS3_RealDocker verifies the resolveBackupFile remote branch: a
// backup is uploaded to S3, its local copy is deleted, and the restore then
// downloads it from S3 and feeds it to the restore client.
func TestRestoreFromS3_RealDocker(t *testing.T) {
	if os.Getenv("BELUNE_DOCKER_INTEGRATION") == "" {
		t.Skip("set BELUNE_DOCKER_INTEGRATION=1 to run the restore-from-S3 test (needs Docker)")
	}
	ctx := context.Background()

	// LocalStack, not MinIO: MinIO's image is no longer anonymously pullable
	// from either quay.io or Docker Hub — see startS3 in
	// internal/service/backup/service_test.go for the fuller reasoning.
	// Pinned, not :latest — a moving tag is exactly how the MinIO removal
	// reached CI as a surprise in the first place.
	container, err := localstack.Run(ctx, "localstack/localstack:4.14.0",
		testcontainers.WithEnv(map[string]string{"SERVICES": "s3"}),
	)
	require.NoError(t, err)
	t.Cleanup(func() { _ = container.Terminate(ctx) })

	endpoint, err := container.PortEndpoint(ctx, "4566/tcp", "http")
	require.NoError(t, err)
	endpoint = strings.TrimPrefix(strings.TrimPrefix(endpoint, "http://"), "https://")

	svc := backup.New(&config.Config{
		BackupRemoteEnabled: true,
		BackupS3Endpoint:    endpoint,
		BackupS3Region:      "us-east-1",
		BackupS3Bucket:      "test-backups",
		BackupS3AccessKey:   "test",
		BackupS3SecretKey:   "test",
		BackupS3Prefix:      "belune/",
		BackupS3UseSSL:      false,
	})
	require.NoError(t, svc.EnsureBucket(ctx))

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
	h := newTestHandler(rt, nil)
	h.Config.DatabaseBackupDir = t.TempDir()
	h.BackupService = svc

	db := seedDatabase(t)

	// Back up → uploads to S3 (remote_key set). backupAndList runs the task
	// itself, so it is the only call needed.
	backups := backupAndList(t, h, db)
	require.True(t, backups[0].RemoteKey.Valid, "backup should have been uploaded to S3")

	// A successful upload is authoritative, so the worker already dropped the
	// local archive — otherwise the server disk would mirror the bucket. That
	// is what forces restore down the S3 download path, and asserting it here
	// is what makes this a test of that path rather than of a file we deleted.
	require.False(t, backups[0].LocalPath.Valid,
		"local copy should have been dropped after a successful S3 upload")

	rp, _ := json.Marshal(map[string]string{"database_id": dbIDStr(db), "backup_id": uuidString(backups[0].ID)})
	require.NoError(t, h.HandleRestoreDBTask(ctx, asynq.NewTask("restore_db", rp)))
	assert.Greater(t, restoredLen, 0, "restore should have streamed the S3-downloaded dump")
}
