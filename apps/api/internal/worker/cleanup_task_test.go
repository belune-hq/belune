package worker_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/hibiken/asynq"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/belune-hq/belune/internal/naming"
	"github.com/belune-hq/belune/internal/runtime"
	"github.com/belune-hq/belune/internal/testutil"
	"github.com/belune-hq/belune/internal/worker"
)

// runFullCleanup runs the cleanup exactly as the daily scheduled job does: no
// Actions, which means every step including the orphan container sweep.
func runFullCleanup(t *testing.T, h *worker.TaskHandler) {
	t.Helper()
	payload, err := json.Marshal(map[string]any{})
	require.NoError(t, err)
	require.NoError(t, h.HandleCleanupTask(context.Background(), asynq.NewTask("cleanup", payload)))
}

// TestCleanupOrphanContainers_SparesManagedDatabases is the test whose absence
// let this ship: the sweep lists containers by the managed-by label — which
// databases carry — but built its allowlist from applications alone, so the
// daily run stopped and removed every managed database container. Volumes
// survive, so the data did, but each database needed re-provisioning.
func TestCleanupOrphanContainers_SparesManagedDatabases(t *testing.T) {
	rt := &testutil.MockContainerRuntime{}
	h := newTestHandler(rt, nil)

	// An application as well as a database, so the allowlist is never empty and
	// the empty-allowlist guard cannot be what saves the database here. What is
	// under test is the database being recognised, not the sweep being skipped.
	app, _ := seedApp(t)
	db := seedDatabase(t)
	old := time.Now().Add(-24 * time.Hour)

	// All three carry managed-by=belune and all are past the grace period. Only
	// the last belongs to nothing.
	rt.ListContainers_ = []runtime.ContainerInfo{
		{Name: app.Slug, CreatedAt: old},
		{Name: db.Slug, CreatedAt: old},
		{Name: "left-over-from-a-deleted-app", CreatedAt: old},
	}

	runFullCleanup(t, h)

	assert.NotContains(t, rt.RemoveCalls, db.Slug,
		"a live database's container must survive the orphan sweep")
	assert.NotContains(t, rt.StopCalls, db.Slug,
		"a live database's container must not even be stopped")
	assert.NotContains(t, rt.RemoveCalls, app.Slug,
		"a live application's container must survive too")
	assert.Contains(t, rt.RemoveCalls, "left-over-from-a-deleted-app",
		"a genuine orphan must still be reaped")
}

// TestCleanupOrphanContainers_SparesAHelperStillAtWork is the data-loss case.
// Helper containers are unnamed, so no allowlist can ever cover one; the sweep
// has to recognise them by label. A volume restore runs
// `find . -mindepth 1 -delete && tar xzf -` inside one, so killing it between
// those two commands leaves the volume empty or half-written — which is worse
// than the failed job it looks like.
func TestCleanupOrphanContainers_SparesAHelperStillAtWork(t *testing.T) {
	rt := &testutil.MockContainerRuntime{}
	h := newTestHandler(rt, nil)

	app, _ := seedApp(t)
	old := time.Now().Add(-24 * time.Hour)

	rt.ListContainers_ = []runtime.ContainerInfo{
		{Name: app.Slug, CreatedAt: old, Status: "running"},
		// A long restore: unnamed, labelled, and well past the grace period.
		{
			Name:      "eloquent_hopper",
			CreatedAt: old,
			Status:    "running",
			Labels:    map[string]string{"managed-by": "belune", runtime.LabelHelper: "true"},
		},
		// One that died and left its container behind is genuinely leftover.
		{
			Name:      "vigilant_mendel",
			CreatedAt: old,
			Status:    "exited",
			Labels:    map[string]string{"managed-by": "belune", runtime.LabelHelper: "true"},
		},
	}

	runFullCleanup(t, h)

	assert.NotContains(t, rt.RemoveCalls, "eloquent_hopper",
		"a helper still doing work must not be reaped mid-operation")
	assert.NotContains(t, rt.StopCalls, "eloquent_hopper")
	assert.Contains(t, rt.RemoveCalls, "vigilant_mendel",
		"a helper that has exited is leftover and should still be reclaimed")
}

// TestCleanupOrphanContainers_ReapsOnAnEmptyInstall pins the case an earlier
// guard got wrong. Both lookups return on error, so an empty allowlist means
// there are no applications and no databases — not that the allowlist failed to
// build. Every managed container is then genuinely leftover, and refusing to
// act would leave it holding its name and ports for good.
func TestCleanupOrphanContainers_ReapsOnAnEmptyInstall(t *testing.T) {
	rt := &testutil.MockContainerRuntime{}
	h := newTestHandler(rt, nil)

	// Worker tests share a database, so the install is only genuinely empty
	// once the rows other tests seeded are gone.
	require.NoError(t, testutil.TruncateAll(context.Background(), testPool))

	rt.ListContainers_ = []runtime.ContainerInfo{
		{Name: "left-behind-by-a-deleted-app", CreatedAt: time.Now().Add(-24 * time.Hour)},
		// A helper still at work is spared even here, which is what makes
		// reaping on an empty install safe.
		{
			Name:      "busy_helper",
			CreatedAt: time.Now().Add(-24 * time.Hour),
			Status:    "running",
			Labels:    map[string]string{"managed-by": "belune", runtime.LabelHelper: "true"},
		},
	}

	runFullCleanup(t, h)

	assert.Contains(t, rt.RemoveCalls, "left-behind-by-a-deleted-app",
		"an install with nothing in it must still reclaim its leftovers")
	assert.NotContains(t, rt.RemoveCalls, "busy_helper",
		"a helper still at work is spared by label, not by the allowlist")
}

// TestCleanupOrphanContainers_SparesPreLabelContainers is trap #1 of the
// rewrite. Belune labelled nothing with an application-id or database-id for
// its first releases, and containers survive an upgrade untouched — so a
// label-only rule would have reaped every container on an install that had not
// redeployed since. The event watcher keeps the same name fallback for exactly
// this reason.
func TestCleanupOrphanContainers_SparesPreLabelContainers(t *testing.T) {
	rt := &testutil.MockContainerRuntime{}
	h := newTestHandler(rt, nil)

	app, _ := seedApp(t)
	db := seedDatabase(t)
	appID := uuidString(app.ID)
	// seedApp names the app "{projectSlug}-app", and the legacy formats are
	// built from the project slug — so it has to be the real one, not a guess.
	projectSlug := strings.TrimSuffix(app.Slug, "-app")
	old := time.Now().Add(-24 * time.Hour)

	// Not one of these carries an id label; each is a naming format Belune has
	// actually shipped.
	rt.ListContainers_ = []runtime.ContainerInfo{
		{Name: app.Slug, CreatedAt: old},
		{Name: naming.IntermediateContainerName(projectSlug, appID), CreatedAt: old},
		{Name: naming.OldContainerName(appID), CreatedAt: old},
		{Name: db.Slug, CreatedAt: old},
	}

	runFullCleanup(t, h)

	assert.Empty(t, rt.RemoveCalls,
		"containers predating the id labels must still be recognised by name")
}

// TestCleanupOrphanContainers_ReapsByLabelWhateverTheNameIs is the point of the
// rewrite. The old sweep matched names alone, so a container whose name had
// moved on was invisible to it and leaked forever. Matching the id label makes
// a container naming a resource that no longer exists an orphan whatever it is
// called — which is what carries this across the 0.3.0 container rename.
func TestCleanupOrphanContainers_ReapsByLabelWhateverTheNameIs(t *testing.T) {
	rt := &testutil.MockContainerRuntime{}
	h := newTestHandler(rt, nil)

	app, _ := seedApp(t)
	db := seedDatabase(t)
	old := time.Now().Add(-24 * time.Hour)

	rt.ListContainers_ = []runtime.ContainerInfo{
		// Live rows, but under names no allowlist would ever have produced.
		{
			Name:      app.Slug + "-web-2",
			CreatedAt: old,
			Labels:    map[string]string{runtime.LabelApplicationID: uuidString(app.ID)},
		},
		{
			Name:      db.Slug + "-primary",
			CreatedAt: old,
			Labels:    map[string]string{runtime.LabelDatabaseID: uuidString(db.ID)},
		},
		// Deleted rows. The names are irrelevant; the labels are the evidence.
		{
			Name:      "some-plausible-name",
			CreatedAt: old,
			Labels:    map[string]string{runtime.LabelApplicationID: "6f1c2f6e-0000-4000-8000-000000000000"},
		},
		{
			Name:      "another-plausible-name",
			CreatedAt: old,
			Labels:    map[string]string{runtime.LabelDatabaseID: "6f1c2f6e-0000-4000-8000-000000000001"},
		},
	}

	runFullCleanup(t, h)

	assert.NotContains(t, rt.RemoveCalls, app.Slug+"-web-2",
		"a live application's container is claimed by its label, not its name")
	assert.NotContains(t, rt.RemoveCalls, db.Slug+"-primary",
		"a live database's container is claimed by its label, not its name")
	assert.Contains(t, rt.RemoveCalls, "some-plausible-name",
		"an application-id pointing at nothing is an orphan whatever it is called")
	assert.Contains(t, rt.RemoveCalls, "another-plausible-name",
		"a database-id pointing at nothing is an orphan whatever it is called")
}

// TestCleanupOrphanContainers_SparesAnUnreadableLabel keeps the sweep from
// treating what it cannot parse as garbage. A container we cannot read is not
// thereby leftover, so an unusable label falls back to the name check rather
// than deciding on its own that nothing claims the container.
func TestCleanupOrphanContainers_SparesAnUnreadableLabel(t *testing.T) {
	rt := &testutil.MockContainerRuntime{}
	h := newTestHandler(rt, nil)

	app, _ := seedApp(t)
	old := time.Now().Add(-24 * time.Hour)

	rt.ListContainers_ = []runtime.ContainerInfo{
		{
			Name:      app.Slug,
			CreatedAt: old,
			Labels:    map[string]string{runtime.LabelApplicationID: "not-a-uuid"},
		},
	}

	runFullCleanup(t, h)

	assert.Empty(t, rt.RemoveCalls,
		"an unparseable label must fall back to the name, not condemn the container")
}

// TestCleanupOrphanContainers_SparesContainersClaimedByAnotherServer is the
// trap that per-host sweeping introduces. The phase-1 resolver returns the same
// local daemon for every server id, so the moment a second server row exists
// every host sees the same container list — and sweeping host B against host
// B's rows alone would delete everything belonging to host A.
func TestCleanupOrphanContainers_SparesContainersClaimedByAnotherServer(t *testing.T) {
	ctx := context.Background()
	rt := &testutil.MockContainerRuntime{}
	h := newTestHandler(rt, nil)

	app, _ := seedApp(t)
	db := seedDatabase(t)

	// A second managed host, with nothing placed on it. TruncateAll leaves
	// servers alone on purpose — the seeded local row has to survive, since
	// projects.server_id is NOT NULL — so this one has to clean up after
	// itself or every later test in the package sweeps two hosts.
	serverName := "second-" + randomSuffix(t)
	_, err := testPool.Exec(ctx,
		`INSERT INTO servers (name, is_local, lifecycle, enrolled_at) VALUES ($1, false, 'active', NOW())`,
		serverName)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, err := testPool.Exec(context.Background(), `DELETE FROM servers WHERE name = $1`, serverName)
		require.NoError(t, err)
	})

	old := time.Now().Add(-24 * time.Hour)
	rt.ListContainers_ = []runtime.ContainerInfo{
		{Name: app.Slug, CreatedAt: old, Labels: map[string]string{runtime.LabelApplicationID: uuidString(app.ID)}},
		{Name: db.Slug, CreatedAt: old, Labels: map[string]string{runtime.LabelDatabaseID: uuidString(db.ID)}},
		{Name: "genuinely-nobodys", CreatedAt: old},
	}

	runFullCleanup(t, h)

	assert.NotContains(t, rt.RemoveCalls, app.Slug,
		"a container claimed by another host must survive that host's sweep")
	assert.NotContains(t, rt.RemoveCalls, db.Slug,
		"a database claimed by another host must survive too")
	assert.Contains(t, rt.RemoveCalls, "genuinely-nobodys",
		"a container no host claims is still an orphan")
}

// seedTombstonedBackup creates a database, a backup of it, then deletes the
// database keeping the backup — leaving exactly the orphan retention sweeps.
// deletedDaysAgo backdates the tombstone so expiry can be exercised without
// waiting.
func seedTombstonedBackup(t *testing.T, deletedDaysAgo int) (backupID, tombstoneID string) {
	t.Helper()
	ctx := context.Background()
	db := seedDatabase(t)

	var bID string
	require.NoError(t, testPool.QueryRow(ctx,
		`INSERT INTO database_backups (database_id, status, size_bytes)
		 VALUES ($1, 'succeeded', 512) RETURNING id`, uuidString(db.ID)).Scan(&bID))

	var tID string
	require.NoError(t, testPool.QueryRow(ctx,
		`INSERT INTO database_tombstones (project_id, original_id, slug, name, type, version, deleted_at)
		 VALUES ($1, $2, $3, $4, $5, $6, NOW() - make_interval(days => $7)) RETURNING id`,
		uuidString(db.ProjectID), uuidString(db.ID), db.Slug, db.Name, db.Type, db.Version,
		deletedDaysAgo).Scan(&tID))

	_, err := testPool.Exec(ctx,
		`UPDATE database_backups SET database_id = NULL, tombstone_id = $1 WHERE id = $2`, tID, bID)
	require.NoError(t, err)
	_, err = testPool.Exec(ctx, `DELETE FROM databases WHERE id = $1`, uuidString(db.ID))
	require.NoError(t, err)

	return bID, tID
}

// TestCleanupOrphanedBackups_ExpiresPastRetention is the counterweight to
// keeping backups by default. Without a bound, reversing the default would mean
// remote storage that only ever grows — its own kind of surprise bill.
func TestCleanupOrphanedBackups_ExpiresPastRetention(t *testing.T) {
	rt := &testutil.MockContainerRuntime{}
	h := newTestHandler(rt, nil)
	ctx := context.Background()

	expired, expiredTombstone := seedTombstonedBackup(t, 120)
	kept, keptTombstone := seedTombstonedBackup(t, 3)

	runFullCleanup(t, h)

	var n int
	require.NoError(t, testPool.QueryRow(ctx,
		`SELECT count(*) FROM database_backups WHERE id = $1`, expired).Scan(&n))
	assert.Equal(t, 0, n, "a backup kept past the retention window is expired")
	require.NoError(t, testPool.QueryRow(ctx,
		`SELECT count(*) FROM database_tombstones WHERE id = $1`, expiredTombstone).Scan(&n))
	assert.Equal(t, 0, n, "the tombstone goes with its last backup — it describes nothing")

	require.NoError(t, testPool.QueryRow(ctx,
		`SELECT count(*) FROM database_backups WHERE id = $1`, kept).Scan(&n))
	assert.Equal(t, 1, n, "a recently deleted database's backup is still inside the window")
	require.NoError(t, testPool.QueryRow(ctx,
		`SELECT count(*) FROM database_tombstones WHERE id = $1`, keptTombstone).Scan(&n))
	assert.Equal(t, 1, n, "and its tombstone must stay, or the backup loses its parent")
}

// TestCleanupOrphanedBackups_RetentionCanBeDisabled pins the escape hatch. An
// operator who would rather decide by hand sets 0, and nothing expires.
func TestCleanupOrphanedBackups_RetentionCanBeDisabled(t *testing.T) {
	rt := &testutil.MockContainerRuntime{}
	h := newTestHandler(rt, nil)
	ctx := context.Background()

	_, err := testPool.Exec(ctx,
		`INSERT INTO settings (key, value) VALUES ('orphaned_backup_retention_days', '0')
		 ON CONFLICT (key) DO UPDATE SET value = '0'`)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, err := testPool.Exec(context.Background(),
			`DELETE FROM settings WHERE key = 'orphaned_backup_retention_days'`)
		require.NoError(t, err)
	})

	ancient, _ := seedTombstonedBackup(t, 3650)

	runFullCleanup(t, h)

	var n int
	require.NoError(t, testPool.QueryRow(ctx,
		`SELECT count(*) FROM database_backups WHERE id = $1`, ancient).Scan(&n))
	assert.Equal(t, 1, n, "retention 0 means keep, not expire immediately")
}
