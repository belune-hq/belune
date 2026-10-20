package worker_test

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/hibiken/asynq"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/belune-hq/belune/internal/naming"
	"github.com/belune-hq/belune/internal/runtime/docker"
	"github.com/belune-hq/belune/internal/store/generated"
)

// TestUpgradeRoundTrip_RealDocker provisions a real Postgres 16 container,
// writes data, runs the guarded upgrade to 17 (real pg_dump → wipe → reprovision
// → restore), and verifies the data survived. It exercises the destructive
// upgrade path against real engine clients — the part mock tests cannot cover.
//
// Gated behind BELUNE_DOCKER_INTEGRATION because it pulls images and is slow.
//
//	Run with: BELUNE_DOCKER_INTEGRATION=1 go test ./internal/worker/ \
//		-run TestUpgradeRoundTrip_RealDocker -timeout 600s -v
func TestUpgradeRoundTrip_RealDocker(t *testing.T) {
	if os.Getenv("BELUNE_DOCKER_INTEGRATION") == "" {
		t.Skip("set BELUNE_DOCKER_INTEGRATION=1 to run the real-Docker upgrade round-trip")
	}

	ctx := context.Background()
	rt, err := docker.New()
	require.NoError(t, err)

	h := newTestHandler(rt, nil)
	h.Config.DatabaseBackupDir = t.TempDir()

	db := seedDatabase(t, func(p *generated.CreateDatabaseParams) {
		p.Type = "postgres"
		p.Version = "16"
		p.Status = "creating"
		p.InternalHost = pgtype.Text{}
		p.InternalPort = pgtype.Int4{}
	})
	network := naming.ProjectNetworkName(strings.TrimSuffix(db.Slug, "-db"))

	t.Cleanup(func() {
		_ = rt.StopContainer(context.Background(), db.Slug)
		_ = rt.RemoveContainer(context.Background(), db.Slug)
		_ = rt.RemoveVolume(context.Background(), db.Slug+"-vol")
		_ = rt.RemoveNetwork(context.Background(), network)
	})

	// 1. Provision the real Postgres 16 container.
	provPayload, _ := json.Marshal(map[string]string{"database_id": dbIDStr(db)})
	require.NoError(t, h.HandleProvisionDBTask(ctx, asynq.NewTask("provision_db", provPayload)))

	// 2. Wait until it accepts connections, then write a row.
	psql := func(sql string, out *bytes.Buffer) (int, error) {
		cmd := []string{"sh", "-c", "PGPASSWORD=p psql -U u -d d -tAc " + shellQuote(sql)}
		// Pass an untyped nil writer (not a typed-nil *bytes.Buffer) when no
		// capture is wanted, so ContainerExec's nil check works.
		if out == nil {
			return rt.ContainerExec(ctx, db.Slug, cmd, nil, nil, nil)
		}
		return rt.ContainerExec(ctx, db.Slug, cmd, nil, out, nil)
	}
	requireReady(t, func() bool {
		exit, err := psql("SELECT 1", nil)
		return err == nil && exit == 0
	})
	exit, err := psql("CREATE TABLE t (id int); INSERT INTO t VALUES (42);", nil)
	require.NoError(t, err)
	require.Equal(t, 0, exit)

	// 3. Guarded upgrade 16 -> 17.
	upPayload, _ := json.Marshal(map[string]string{"database_id": dbIDStr(db), "target_version": "17"})
	require.NoError(t, h.HandleUpgradeDBTask(ctx, asynq.NewTask("upgrade_db", upPayload)))

	updated, err := testQueries.GetDatabase(ctx, db.ID)
	require.NoError(t, err)
	assert.Equal(t, "17", updated.Version)
	assert.Equal(t, "running", updated.Status)

	// 4. The row must have survived the dump → wipe → restore.
	requireReady(t, func() bool {
		exit, err := psql("SELECT 1", nil)
		return err == nil && exit == 0
	})
	var out bytes.Buffer
	exit, err = psql("SELECT id FROM t", &out)
	require.NoError(t, err)
	require.Equal(t, 0, exit)
	assert.Contains(t, out.String(), "42", "data did not survive the upgrade")
}

// TestUpgradeRoundTrip_MySQL_RealDocker is the MySQL analogue (8.0 → 8.4),
// exercising mysqldump/mysql restore-as-root against real containers.
func TestUpgradeRoundTrip_MySQL_RealDocker(t *testing.T) {
	if os.Getenv("BELUNE_DOCKER_INTEGRATION") == "" {
		t.Skip("set BELUNE_DOCKER_INTEGRATION=1 to run the real-Docker MySQL upgrade round-trip")
	}

	ctx := context.Background()
	rt, err := docker.New()
	require.NoError(t, err)

	h := newTestHandler(rt, nil)
	h.Config.DatabaseBackupDir = t.TempDir()

	db := seedDatabase(t, func(p *generated.CreateDatabaseParams) {
		p.Type = "mysql"
		p.Version = "8.0"
		p.Status = "creating"
		p.InternalHost = pgtype.Text{}
		p.InternalPort = pgtype.Int4{}
	})
	network := naming.ProjectNetworkName(strings.TrimSuffix(db.Slug, "-db"))
	t.Cleanup(func() {
		_ = rt.StopContainer(context.Background(), db.Slug)
		_ = rt.RemoveContainer(context.Background(), db.Slug)
		_ = rt.RemoveVolume(context.Background(), db.Slug+"-vol")
		_ = rt.RemoveNetwork(context.Background(), network)
	})

	provPayload, _ := json.Marshal(map[string]string{"database_id": dbIDStr(db)})
	require.NoError(t, h.HandleProvisionDBTask(ctx, asynq.NewTask("provision_db", provPayload)))

	mysql := func(sql string, out *bytes.Buffer) (int, error) {
		cmd := []string{"sh", "-c", "MYSQL_PWD=rootp mysql -u root -N -e " + shellQuote(sql) + " d"}
		if out == nil {
			return rt.ContainerExec(ctx, db.Slug, cmd, nil, nil, nil)
		}
		return rt.ContainerExec(ctx, db.Slug, cmd, nil, out, nil)
	}
	requireReady(t, func() bool {
		exit, err := mysql("SELECT 1", nil)
		return err == nil && exit == 0
	})
	exit, err := mysql("CREATE TABLE t (id int); INSERT INTO t VALUES (42);", nil)
	require.NoError(t, err)
	require.Equal(t, 0, exit)

	upPayload, _ := json.Marshal(map[string]string{"database_id": dbIDStr(db), "target_version": "8.4"})
	require.NoError(t, h.HandleUpgradeDBTask(ctx, asynq.NewTask("upgrade_db", upPayload)))

	updated, err := testQueries.GetDatabase(ctx, db.ID)
	require.NoError(t, err)
	assert.Equal(t, "8.4", updated.Version)
	assert.Equal(t, "running", updated.Status)

	requireReady(t, func() bool {
		exit, err := mysql("SELECT 1", nil)
		return err == nil && exit == 0
	})
	var out bytes.Buffer
	exit, err = mysql("SELECT id FROM t", &out)
	require.NoError(t, err)
	require.Equal(t, 0, exit)
	assert.Contains(t, out.String(), "42", "data did not survive the MySQL upgrade")
}

// TestUpgradeRoundTrip_Mongo_RealDocker is the MongoDB analogue (7 → 8),
// exercising mongodump/mongorestore against real containers.
func TestUpgradeRoundTrip_Mongo_RealDocker(t *testing.T) {
	if os.Getenv("BELUNE_DOCKER_INTEGRATION") == "" {
		t.Skip("set BELUNE_DOCKER_INTEGRATION=1 to run the real-Docker Mongo upgrade round-trip")
	}

	ctx := context.Background()
	rt, err := docker.New()
	require.NoError(t, err)

	h := newTestHandler(rt, nil)
	h.Config.DatabaseBackupDir = t.TempDir()

	db := seedDatabase(t, func(p *generated.CreateDatabaseParams) {
		p.Type = "mongo"
		p.Version = "7"
		p.Status = "creating"
		p.InternalHost = pgtype.Text{}
		p.InternalPort = pgtype.Int4{}
	})
	network := naming.ProjectNetworkName(strings.TrimSuffix(db.Slug, "-db"))
	t.Cleanup(func() {
		_ = rt.StopContainer(context.Background(), db.Slug)
		_ = rt.RemoveContainer(context.Background(), db.Slug)
		_ = rt.RemoveVolume(context.Background(), db.Slug+"-vol")
		_ = rt.RemoveNetwork(context.Background(), network)
	})

	provPayload, _ := json.Marshal(map[string]string{"database_id": dbIDStr(db)})
	require.NoError(t, h.HandleProvisionDBTask(ctx, asynq.NewTask("provision_db", provPayload)))

	// mongosh authenticated as the root user; --quiet keeps stdout to the eval result.
	mongo := func(js string, out *bytes.Buffer) (int, error) {
		cmd := []string{"sh", "-c",
			"mongosh --username u --password p --authenticationDatabase admin --quiet --eval " + shellQuote(js)}
		if out == nil {
			return rt.ContainerExec(ctx, db.Slug, cmd, nil, nil, nil)
		}
		return rt.ContainerExec(ctx, db.Slug, cmd, nil, out, nil)
	}
	requireReady(t, func() bool {
		exit, err := mongo("db.runCommand({ping:1})", nil)
		return err == nil && exit == 0
	})
	exit, err := mongo("db.getSiblingDB('app').t.insertOne({id:42})", nil)
	require.NoError(t, err)
	require.Equal(t, 0, exit)

	upPayload, _ := json.Marshal(map[string]string{"database_id": dbIDStr(db), "target_version": "8"})
	require.NoError(t, h.HandleUpgradeDBTask(ctx, asynq.NewTask("upgrade_db", upPayload)))

	updated, err := testQueries.GetDatabase(ctx, db.ID)
	require.NoError(t, err)
	assert.Equal(t, "8", updated.Version)
	assert.Equal(t, "running", updated.Status)

	requireReady(t, func() bool {
		exit, err := mongo("db.runCommand({ping:1})", nil)
		return err == nil && exit == 0
	})
	var out bytes.Buffer
	exit, err = mongo("print(db.getSiblingDB('app').t.findOne().id)", &out)
	require.NoError(t, err)
	require.Equal(t, 0, exit)
	assert.Contains(t, out.String(), "42", "data did not survive the Mongo upgrade")
}

func requireReady(t *testing.T, ready func() bool) {
	t.Helper()
	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		if ready() {
			return
		}
		time.Sleep(2 * time.Second)
	}
	t.Fatal("database did not become ready in time")
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
