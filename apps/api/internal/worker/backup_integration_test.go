package worker_test

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/hibiken/asynq"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/belune-hq/belune/internal/naming"
	"github.com/belune-hq/belune/internal/runtime/docker"
	"github.com/belune-hq/belune/internal/store/generated"
)

// otherRedisDB seeds an "other"-type database backed by redis:7-alpine (a small,
// long-running image with sh + tar that persists to /data), configured for the
// given backup mode.
func otherRedisDB(t *testing.T, mode string, configure func(*generated.CreateDatabaseParams)) generated.Database {
	return seedDatabase(t, func(p *generated.CreateDatabaseParams) {
		p.Type = "other"
		p.Version = ""
		p.Status = "creating"
		p.Image = pgtype.Text{String: "redis:7-alpine", Valid: true}
		p.ContainerPort = pgtype.Int4{Int32: 6379, Valid: true}
		p.DataDir = pgtype.Text{String: "/data", Valid: true}
		p.BackupMode = mode
		p.InternalHost = pgtype.Text{}
		p.InternalPort = pgtype.Int4{}
		if configure != nil {
			configure(p)
		}
	})
}

func provisionAndCleanup(t *testing.T, h interface {
	HandleProvisionDBTask(context.Context, *asynq.Task) error
}, rt *docker.Client, db generated.Database) {
	t.Helper()
	ctx := context.Background()
	network := naming.ProjectNetworkName(strings.TrimSuffix(db.Slug, "-db"))
	t.Cleanup(func() {
		_ = rt.StopContainer(context.Background(), db.Slug)
		_ = rt.RemoveContainer(context.Background(), db.Slug)
		_ = rt.RemoveVolume(context.Background(), db.Slug+"-vol")
		_ = rt.RemoveNetwork(context.Background(), network)
	})
	prov, _ := json.Marshal(map[string]string{"database_id": dbIDStr(db)})
	require.NoError(t, h.HandleProvisionDBTask(ctx, asynq.NewTask("provision_db", prov)))
}

// TestBackupRestore_OtherVolumeSnapshot_RealDocker exercises the cold volume
// snapshot path (RunHelper tar → wipe → untar) against a real container: write
// data, back up, change it, restore, and confirm the original value is back.
func TestBackupRestore_OtherVolumeSnapshot_RealDocker(t *testing.T) {
	if os.Getenv("BELUNE_DOCKER_INTEGRATION") == "" {
		t.Skip("set BELUNE_DOCKER_INTEGRATION=1 to run the real-Docker volume-snapshot round-trip")
	}
	ctx := context.Background()
	rt, err := docker.New()
	require.NoError(t, err)
	h := newTestHandler(rt, nil)
	h.Config.DatabaseBackupDir = t.TempDir()
	h.Config.DatabaseBackupHelperImage = "alpine:3.20"

	db := otherRedisDB(t, "volume_snapshot", nil)
	provisionAndCleanup(t, h, rt, db)

	redis := func(cmd string, out *bytes.Buffer) (int, error) {
		c := []string{"sh", "-c", "redis-cli " + cmd}
		if out == nil {
			return rt.ContainerExec(ctx, db.Slug, c, nil, nil, nil)
		}
		return rt.ContainerExec(ctx, db.Slug, c, nil, out, nil)
	}
	requireReady(t, func() bool {
		exit, err := redis("PING", nil)
		return err == nil && exit == 0
	})

	// Persist k=42 to the RDB on the volume, then back up.
	_, err = redis("SET k 42", nil)
	require.NoError(t, err)
	_, err = redis("SAVE", nil)
	require.NoError(t, err)
	backups := backupAndList(t, h, db)

	// Change the data, then restore the snapshot.
	_, err = redis("SET k 99", nil)
	require.NoError(t, err)
	_, err = redis("SAVE", nil)
	require.NoError(t, err)

	rp, _ := json.Marshal(map[string]string{"database_id": dbIDStr(db), "backup_id": uuidString(backups[0].ID)})
	require.NoError(t, h.HandleRestoreDBTask(ctx, asynq.NewTask("restore_db", rp)))

	// Container was restarted by restoreVolume; wait for it, then verify the
	// restored RDB brought back k=42.
	requireReady(t, func() bool {
		exit, err := redis("PING", nil)
		return err == nil && exit == 0
	})
	var out bytes.Buffer
	_, err = redis("GET k", &out)
	require.NoError(t, err)
	assert.Contains(t, out.String(), "42", "snapshot restore did not revert the data")
}

// TestBackupRestore_OtherCommand_RealDocker exercises the BYO command path: the
// backup command writes into $BELUNE_BACKUP_DIR (the system tars it out), and the
// restore command reads it back after the system untars it.
func TestBackupRestore_OtherCommand_RealDocker(t *testing.T) {
	if os.Getenv("BELUNE_DOCKER_INTEGRATION") == "" {
		t.Skip("set BELUNE_DOCKER_INTEGRATION=1 to run the real-Docker command-mode round-trip")
	}
	ctx := context.Background()
	rt, err := docker.New()
	require.NoError(t, err)
	h := newTestHandler(rt, nil)
	h.Config.DatabaseBackupDir = t.TempDir()

	db := otherRedisDB(t, "command", func(p *generated.CreateDatabaseParams) {
		p.BackupCommand = pgtype.Text{String: `echo marker-42 > "$BELUNE_BACKUP_DIR/d.txt"`, Valid: true}
		p.RestoreCommand = pgtype.Text{String: `cp "$BELUNE_BACKUP_DIR/d.txt" /data/restored.txt`, Valid: true}
	})
	provisionAndCleanup(t, h, rt, db)

	requireReady(t, func() bool {
		exit, err := rt.ContainerExec(ctx, db.Slug, []string{"sh", "-c", "redis-cli PING"}, nil, nil, nil)
		return err == nil && exit == 0
	})

	// Backup runs the user command (writes d.txt into the scratch dir).
	backups := backupAndList(t, h, db)

	// Restore untars the archive and runs the restore command (writes /data/restored.txt).
	rp, _ := json.Marshal(map[string]string{"database_id": dbIDStr(db), "backup_id": uuidString(backups[0].ID)})
	require.NoError(t, h.HandleRestoreDBTask(ctx, asynq.NewTask("restore_db", rp)))

	var out bytes.Buffer
	exit, err := rt.ContainerExec(ctx, db.Slug, []string{"sh", "-c", "cat /data/restored.txt"}, nil, &out, nil)
	require.NoError(t, err)
	require.Equal(t, 0, exit)
	assert.Contains(t, out.String(), "marker-42", "command-mode restore did not round-trip the archive")
}
