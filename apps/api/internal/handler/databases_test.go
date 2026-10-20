package handler_test

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/belune-hq/belune/internal/testutil"
)

func TestCreateDatabase(t *testing.T) {
	resetDB(t)
	token := env.SetupAdmin(t, "admin@test.com", "password123")
	project := env.CreateProject(t, token, "Test Project", "test-project")
	projectID := extractID(project["id"])

	resp := env.DoRequest(t, "POST", fmt.Sprintf("/api/projects/%s/databases", projectID), map[string]any{
		"name": "mydb",
		"type": "postgres",
	}, testutil.AuthHeader(token))
	assert.Equal(t, http.StatusAccepted, resp.StatusCode)
	result := testutil.ReadJSON(t, resp)
	assert.Equal(t, "mydb", result["name"])
	assert.Equal(t, "postgres", result["type"])
	assert.Equal(t, "creating", result["status"])

	// Verify provision task enqueued
	require.Len(t, env.Asynq.Tasks, 1)
	assert.Equal(t, "provision_db", env.Asynq.Tasks[0].TypeName)
}

func TestCreateDatabase_MySQLDefaultUserIsNotRoot(t *testing.T) {
	resetDB(t)
	token := env.SetupAdmin(t, "admin@test.com", "password123")
	project := env.CreateProject(t, token, "Test Project", "test-project")
	projectID := extractID(project["id"])

	// Display name intentionally has a space + capitals — the default database
	// name must still come out as the clean slug, not this raw name.
	resp := env.DoRequest(t, "POST", fmt.Sprintf("/api/projects/%s/databases", projectID), map[string]any{
		"name": "Laravel MySQL",
		"type": "mysql",
	}, testutil.AuthHeader(token))
	require.Equal(t, http.StatusAccepted, resp.StatusCode)
	db := testutil.ReadJSON(t, resp)
	dbID := extractID(db["id"])

	resp = env.DoRequest(t, "GET", fmt.Sprintf("/api/projects/%s/databases/%s/credentials/reveal", projectID, dbID), nil, testutil.AuthHeader(token))
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	result := testutil.ReadJSON(t, resp)
	creds, ok := result["credentials"].(map[string]any)
	require.True(t, ok, "credentials should be a map")
	// The base slug (naming.Slugify of the name), not the final DB row's
	// project-prefixed + ID-suffixed slug column.
	assert.Equal(t, "laravel-mysql", creds["user"])
	assert.Equal(t, "laravel-mysql", creds["database"])
	assert.NotEqual(t, "root", creds["user"])
	assert.NotEmpty(t, creds["root_password"])
	assert.NotEqual(t, creds["root_password"], creds["password"])
}

func TestCreateDatabase_MySQLRejectsRootUserOverride(t *testing.T) {
	resetDB(t)
	token := env.SetupAdmin(t, "admin@test.com", "password123")
	project := env.CreateProject(t, token, "Test Project", "test-project")
	projectID := extractID(project["id"])

	resp := env.DoRequest(t, "POST", fmt.Sprintf("/api/projects/%s/databases", projectID), map[string]any{
		"name": "mydb",
		"type": "mysql",
		"credentials": map[string]any{
			"user": "root",
		},
	}, testutil.AuthHeader(token))
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

func TestGetDatabase(t *testing.T) {
	resetDB(t)
	token := env.SetupAdmin(t, "admin@test.com", "password123")
	project := env.CreateProject(t, token, "Test Project", "test-project")
	projectID := extractID(project["id"])

	// Create database
	resp := env.DoRequest(t, "POST", fmt.Sprintf("/api/projects/%s/databases", projectID), map[string]any{
		"name": "mydb",
		"type": "postgres",
	}, testutil.AuthHeader(token))
	db := testutil.ReadJSON(t, resp)
	dbID := extractID(db["id"])

	// GetDatabase must NOT carry credentials — that's a live, directly usable
	// secret and sits behind plain read scope; see RevealDatabaseCredentials.
	resp = env.DoRequest(t, "GET", fmt.Sprintf("/api/projects/%s/databases/%s", projectID, dbID), nil, testutil.AuthHeader(token))
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	result := testutil.ReadJSON(t, resp)
	assert.Equal(t, "mydb", result["name"])
	assert.Nil(t, result["credentials"], "GetDatabase must not return credentials")
	assert.Nil(t, result["connection_string"], "GetDatabase must not return a connection string")

	// The dedicated reveal endpoint decrypts them, and requires a session.
	resp = env.DoRequest(t, "GET", fmt.Sprintf("/api/projects/%s/databases/%s/credentials/reveal", projectID, dbID), nil, testutil.AuthHeader(token))
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	revealed := testutil.ReadJSON(t, resp)
	creds, ok := revealed["credentials"].(map[string]any)
	require.True(t, ok, "credentials should be a map")
	assert.NotEmpty(t, creds["user"])
	assert.NotEmpty(t, creds["password"])
	assert.NotEmpty(t, revealed["connection_string"])
}

func TestUpdateDatabaseResources(t *testing.T) {
	resetDB(t)
	token := env.SetupAdmin(t, "admin@test.com", "password123")
	project := env.CreateProject(t, token, "Test Project", "test-project")
	projectID := extractID(project["id"])

	resp := env.DoRequest(t, "POST", fmt.Sprintf("/api/projects/%s/databases", projectID), map[string]any{
		"name": "mydb",
		"type": "postgres",
	}, testutil.AuthHeader(token))
	db := testutil.ReadJSON(t, resp)
	dbID := extractID(db["id"])

	// Update resource limits.
	resp = env.DoRequest(t, "PUT", fmt.Sprintf("/api/projects/%s/databases/%s/resources", projectID, dbID), map[string]any{
		"cpu_limit":    0.5,
		"memory_limit": 536870912,
	}, testutil.AuthHeader(token))
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	updated := testutil.ReadJSON(t, resp)
	assert.Equal(t, 0.5, updated["cpu_limit"])
	assert.Equal(t, float64(536870912), updated["memory_limit"])

	// Negative values are rejected.
	resp = env.DoRequest(t, "PUT", fmt.Sprintf("/api/projects/%s/databases/%s/resources", projectID, dbID), map[string]any{
		"cpu_limit":    -1,
		"memory_limit": 0,
	}, testutil.AuthHeader(token))
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
	resp.Body.Close()
}

func TestSetDatabaseExternalAccess_RequiresRunning(t *testing.T) {
	resetDB(t)
	token := env.SetupAdmin(t, "admin@test.com", "password123")
	project := env.CreateProject(t, token, "Test Project", "test-project")
	projectID := extractID(project["id"])

	resp := env.DoRequest(t, "POST", fmt.Sprintf("/api/projects/%s/databases", projectID), map[string]any{
		"name": "mydb",
		"type": "postgres",
	}, testutil.AuthHeader(token))
	db := testutil.ReadJSON(t, resp)
	dbID := extractID(db["id"])

	// Provisioning is async + mocked in tests, so the database stays "creating";
	// toggling external access must be refused until it is running.
	resp = env.DoRequest(t, "POST",
		fmt.Sprintf("/api/projects/%s/databases/%s/external-access", projectID, dbID),
		map[string]any{"enabled": true}, testutil.AuthHeader(token))
	assert.Equal(t, http.StatusConflict, resp.StatusCode)
	resp.Body.Close()
}

func TestDeleteDatabase(t *testing.T) {
	resetDB(t)
	token := env.SetupAdmin(t, "admin@test.com", "password123")
	project := env.CreateProject(t, token, "Test Project", "test-project")
	projectID := extractID(project["id"])

	// Create database
	resp := env.DoRequest(t, "POST", fmt.Sprintf("/api/projects/%s/databases", projectID), map[string]any{
		"name": "todelete",
		"type": "redis",
	}, testutil.AuthHeader(token))
	db := testutil.ReadJSON(t, resp)
	dbID := extractID(db["id"])

	// Reset mock calls
	env.Runtime.StopCalls = nil
	env.Runtime.RemoveCalls = nil

	resp = env.DoRequest(t, "DELETE", fmt.Sprintf("/api/projects/%s/databases/%s", projectID, dbID), nil, testutil.AuthHeader(token))
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	resp.Body.Close()

	// Verify runtime stop/remove was called
	require.Greater(t, len(env.Runtime.StopCalls), 0)
	require.Greater(t, len(env.Runtime.RemoveCalls), 0)
}

// seedBackupRow inserts a succeeded backup row directly. The backup worker is
// mocked in handler tests, so the artifact is what matters here, not the run.
func seedBackupRow(t *testing.T, dbID string) string {
	t.Helper()
	var id string
	err := env.Pool.QueryRow(context.Background(),
		`INSERT INTO database_backups (database_id, status, remote_key, size_bytes)
		 VALUES ($1, 'succeeded', $2, 1024) RETURNING id`,
		dbID, "backups/"+dbID+".sql.gz").Scan(&id)
	require.NoError(t, err)
	return id
}

// TestDeleteDatabase_KeepsBackupsByDefault is the reversal this release exists
// for. Deleting a database used to destroy every backup of it; the plain
// request now keeps them, and they stay restorable through a tombstone that
// records what the database was.
func TestDeleteDatabase_KeepsBackupsByDefault(t *testing.T) {
	resetDB(t)
	token := env.SetupAdmin(t, "admin@test.com", "password123")
	project := env.CreateProject(t, token, "Test Project", "test-project")
	projectID := extractID(project["id"])

	resp := env.DoRequest(t, "POST", fmt.Sprintf("/api/projects/%s/databases", projectID), map[string]any{
		"name": "keepme",
		"type": "postgres",
	}, testutil.AuthHeader(token))
	db := testutil.ReadJSON(t, resp)
	dbID := extractID(db["id"])
	backupID := seedBackupRow(t, dbID)

	resp = env.DoRequest(t, "DELETE", fmt.Sprintf("/api/projects/%s/databases/%s", projectID, dbID), nil, testutil.AuthHeader(token))
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	resp.Body.Close()

	ctx := context.Background()

	// The database is gone.
	var databases int
	require.NoError(t, env.Pool.QueryRow(ctx,
		`SELECT count(*) FROM databases WHERE id = $1`, dbID).Scan(&databases))
	assert.Equal(t, 0, databases, "the database itself must still be deleted")

	// The backup survived it, re-pointed onto a tombstone.
	var tombstoneID *string
	require.NoError(t, env.Pool.QueryRow(ctx,
		`SELECT tombstone_id FROM database_backups WHERE id = $1`, backupID).Scan(&tombstoneID),
		"the backup row must outlive the database")
	require.NotNil(t, tombstoneID, "a kept backup must hang off a tombstone, never nothing")

	// And the tombstone carries what a replacement restore needs.
	var slug, name, dbType string
	var creds []byte
	require.NoError(t, env.Pool.QueryRow(ctx,
		`SELECT slug, name, type, credentials_encrypted FROM database_tombstones WHERE id = $1`,
		*tombstoneID).Scan(&slug, &name, &dbType, &creds))
	assert.Equal(t, db["slug"], slug, "the original slug is the hostname applications connect to")
	assert.Equal(t, "keepme", name)
	assert.Equal(t, "postgres", dbType)
	assert.NotEmpty(t, creds, "credentials must be carried across or applications cannot reconnect")
}

// TestDeleteDatabase_DestroysBackupsWhenAsked is the other half: the choice has
// to actually work, or the checkbox is theatre.
func TestDeleteDatabase_DestroysBackupsWhenAsked(t *testing.T) {
	resetDB(t)
	token := env.SetupAdmin(t, "admin@test.com", "password123")
	project := env.CreateProject(t, token, "Test Project", "test-project")
	projectID := extractID(project["id"])

	resp := env.DoRequest(t, "POST", fmt.Sprintf("/api/projects/%s/databases", projectID), map[string]any{
		"name": "burnme",
		"type": "postgres",
	}, testutil.AuthHeader(token))
	db := testutil.ReadJSON(t, resp)
	dbID := extractID(db["id"])
	backupID := seedBackupRow(t, dbID)

	resp = env.DoRequest(t, "DELETE",
		fmt.Sprintf("/api/projects/%s/databases/%s?delete_backups=true", projectID, dbID),
		nil, testutil.AuthHeader(token))
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	resp.Body.Close()

	ctx := context.Background()
	var backups, tombstones int
	require.NoError(t, env.Pool.QueryRow(ctx,
		`SELECT count(*) FROM database_backups WHERE id = $1`, backupID).Scan(&backups))
	assert.Equal(t, 0, backups, "an explicit delete must actually destroy the backup")
	require.NoError(t, env.Pool.QueryRow(ctx,
		`SELECT count(*) FROM database_tombstones`).Scan(&tombstones))
	assert.Equal(t, 0, tombstones, "nothing was kept, so there is nothing to describe")
}

// TestDeleteProject_StillPurgesKeptBackups pins the boundary the plan draws:
// project deletion has no keep option, because a tombstone is project-scoped
// and cascades with it. The dialog promises destruction; this is that promise.
func TestDeleteProject_StillPurgesKeptBackups(t *testing.T) {
	resetDB(t)
	token := env.SetupAdmin(t, "admin@test.com", "password123")
	project := env.CreateProject(t, token, "Test Project", "test-project")
	projectID := extractID(project["id"])

	resp := env.DoRequest(t, "POST", fmt.Sprintf("/api/projects/%s/databases", projectID), map[string]any{
		"name": "doomed",
		"type": "postgres",
	}, testutil.AuthHeader(token))
	db := testutil.ReadJSON(t, resp)
	dbID := extractID(db["id"])
	seedBackupRow(t, dbID)

	// Keep them first, so the project delete is genuinely walking over a
	// tombstone rather than an empty table.
	resp = env.DoRequest(t, "DELETE", fmt.Sprintf("/api/projects/%s/databases/%s", projectID, dbID), nil, testutil.AuthHeader(token))
	require.Equal(t, http.StatusOK, resp.StatusCode)
	resp.Body.Close()

	resp = env.DoRequest(t, "DELETE", fmt.Sprintf("/api/projects/%s", projectID), nil, testutil.AuthHeader(token))
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	resp.Body.Close()

	ctx := context.Background()
	var backups, tombstones int
	require.NoError(t, env.Pool.QueryRow(ctx, `SELECT count(*) FROM database_backups`).Scan(&backups))
	require.NoError(t, env.Pool.QueryRow(ctx, `SELECT count(*) FROM database_tombstones`).Scan(&tombstones))
	assert.Equal(t, 0, backups, "deleting a project destroys every backup in it")
	assert.Equal(t, 0, tombstones, "and the tombstones describing them")
}

// TestDeleteProject_ErasesKeptBackupArchives is the row-count assertion above
// done properly, and it exists because that one is blind to the failure that
// actually matters.
//
// A kept backup's row cascades away with its tombstone inside Postgres. If
// nothing erases the archive first, the row vanishes and the storage stays —
// unreachable, unprunable, still billed — while the delete dialog promises the
// opposite. Asserting on rows would pass against exactly that bug, which is why
// this asserts on the FILE.
func TestDeleteProject_ErasesKeptBackupArchives(t *testing.T) {
	resetDB(t)
	token := env.SetupAdmin(t, "admin@test.com", "password123")
	project := env.CreateProject(t, token, "Test Project", "test-project")
	projectID := extractID(project["id"])

	resp := env.DoRequest(t, "POST", fmt.Sprintf("/api/projects/%s/databases", projectID), map[string]any{
		"name": "kept", "type": "postgres",
	}, testutil.AuthHeader(token))
	db := testutil.ReadJSON(t, resp)
	dbID := extractID(db["id"])

	// A real archive on disk, standing in for the stored artifact.
	archive := filepath.Join(t.TempDir(), "kept.sql.gz")
	require.NoError(t, os.WriteFile(archive, []byte("ARCHIVE"), 0o600))
	ctx := context.Background()
	_, err := env.Pool.Exec(ctx,
		`INSERT INTO database_backups (database_id, status, local_path, size_bytes)
		 VALUES ($1, 'succeeded', $2, 7)`, dbID, archive)
	require.NoError(t, err)

	// Keep it, so the project delete walks over a tombstone rather than a live
	// database — the case the loop over existing databases cannot see.
	resp = env.DoRequest(t, "DELETE", fmt.Sprintf("/api/projects/%s/databases/%s", projectID, dbID), nil, testutil.AuthHeader(token))
	require.Equal(t, http.StatusOK, resp.StatusCode)
	resp.Body.Close()
	require.FileExists(t, archive, "keeping a backup must not erase it")

	resp = env.DoRequest(t, "DELETE", fmt.Sprintf("/api/projects/%s", projectID), nil, testutil.AuthHeader(token))
	require.Equal(t, http.StatusOK, resp.StatusCode)
	resp.Body.Close()

	_, statErr := os.Stat(archive)
	assert.True(t, os.IsNotExist(statErr),
		"deleting a project must erase the archives of databases already deleted from it, not just their rows")
}

// TestRestoreFromTombstone_RecreatesWithOriginalIdentity is what makes keeping
// backups worth anything. A replacement has to come back as the SAME database:
// the slug is the container name and therefore the hostname applications
// resolve, and attaching a database injects no connection env vars — so a
// replacement under a new name leaves every dependent application pointing at a
// host that is not there.
func TestRestoreFromTombstone_RecreatesWithOriginalIdentity(t *testing.T) {
	resetDB(t)
	token := env.SetupAdmin(t, "admin@test.com", "password123")
	project := env.CreateProject(t, token, "Test Project", "test-project")
	projectID := extractID(project["id"])

	resp := env.DoRequest(t, "POST", fmt.Sprintf("/api/projects/%s/databases", projectID), map[string]any{
		"name": "orders",
		"type": "postgres",
	}, testutil.AuthHeader(token))
	db := testutil.ReadJSON(t, resp)
	dbID := extractID(db["id"])
	originalSlug := db["slug"].(string)
	backupID := seedBackupRow(t, dbID)

	ctx := context.Background()
	var originalCreds []byte
	require.NoError(t, env.Pool.QueryRow(ctx,
		`SELECT credentials_encrypted FROM databases WHERE id = $1`, dbID).Scan(&originalCreds))

	// Delete, keeping the backups.
	resp = env.DoRequest(t, "DELETE", fmt.Sprintf("/api/projects/%s/databases/%s", projectID, dbID), nil, testutil.AuthHeader(token))
	require.Equal(t, http.StatusOK, resp.StatusCode)
	resp.Body.Close()

	// Restore a replacement from the surviving backup.
	resp = env.DoRequest(t, "POST",
		fmt.Sprintf("/api/projects/%s/orphaned-backups/%s/restore", projectID, backupID),
		nil, testutil.AuthHeader(token))
	require.Equal(t, http.StatusAccepted, resp.StatusCode)
	replacement := testutil.ReadJSON(t, resp)

	assert.Equal(t, originalSlug, replacement["slug"],
		"the replacement must reuse the original slug — it is the hostname applications connect to")
	assert.Equal(t, "orders", replacement["name"])
	assert.Equal(t, "postgres", replacement["type"])

	newID := extractID(replacement["id"])
	var newCreds []byte
	require.NoError(t, env.Pool.QueryRow(ctx,
		`SELECT credentials_encrypted FROM databases WHERE id = $1`, newID).Scan(&newCreds))
	assert.Equal(t, originalCreds, newCreds,
		"credentials must come back byte-identical or applications reconnect and are rejected")

	// The backup belongs to the live database again, and the tombstone is gone.
	var ownerID *string
	require.NoError(t, env.Pool.QueryRow(ctx,
		`SELECT database_id FROM database_backups WHERE id = $1`, backupID).Scan(&ownerID))
	require.NotNil(t, ownerID)
	assert.Equal(t, newID, *ownerID, "the replacement inherits its own backup history")

	var tombstones int
	require.NoError(t, env.Pool.QueryRow(ctx, `SELECT count(*) FROM database_tombstones`).Scan(&tombstones))
	assert.Equal(t, 0, tombstones, "a tombstone must not linger describing a database that exists again")
}

// TestRestoreFromTombstone_RefusesALiveBackup pins the boundary between the two
// restore paths. A backup still attached to a running database is restored from
// that database; coming through the orphan route would recreate a second copy
// of something that never went away.
func TestRestoreFromTombstone_RefusesALiveBackup(t *testing.T) {
	resetDB(t)
	token := env.SetupAdmin(t, "admin@test.com", "password123")
	project := env.CreateProject(t, token, "Test Project", "test-project")
	projectID := extractID(project["id"])

	resp := env.DoRequest(t, "POST", fmt.Sprintf("/api/projects/%s/databases", projectID), map[string]any{
		"name": "cache", "type": "redis",
	}, testutil.AuthHeader(token))
	db := testutil.ReadJSON(t, resp)
	backupID := seedBackupRow(t, extractID(db["id"]))

	resp = env.DoRequest(t, "POST",
		fmt.Sprintf("/api/projects/%s/orphaned-backups/%s/restore", projectID, backupID),
		nil, testutil.AuthHeader(token))
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode,
		"a backup whose database is still running is not an orphan")
	resp.Body.Close()
}

// TestUpdateDatabase_OldPathIsGone pins the v0.1.20 break deliberately rather
// than leaving it to be rediscovered. PUT .../databases/{databaseId} was a
// published path that only ever set cpu_limit and memory_limit; it moved to
// .../resources to match the application endpoint.
//
// The answer is 404, not the 405 chi would give for a known pattern with an
// unregistered method: server.apiAwareCatchAll answers every unmatched /api/
// path itself, so an API caller gets a JSON body rather than the SPA. If this
// ever returns 200 again, an alias has been reintroduced and the changelog's
// breaking-change entry is wrong.
func TestUpdateDatabase_OldPathIsGone(t *testing.T) {
	resetDB(t)
	token := env.SetupAdmin(t, "db-oldpath@test.com", "password123")
	projectID := extractID(env.CreateProject(t, token, "P", "p")["id"])
	dbID := seedDatabaseRow(t, projectID, "oldpath-db")

	resp := env.DoRequest(t, "PUT", fmt.Sprintf("/api/projects/%s/databases/%s", projectID, dbID),
		map[string]any{"cpu_limit": 0.5, "memory_limit": 536870912}, testutil.AuthHeader(token))
	defer resp.Body.Close()
	assert.Equal(t, http.StatusNotFound, resp.StatusCode,
		"the old database update path must no longer accept a PUT")
	assert.Equal(t, "no such endpoint", testutil.ReadJSON(t, resp)["error"],
		"an API caller must get the API's own 404, not the SPA")
}
