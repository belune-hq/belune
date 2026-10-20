package handler_test

import (
	"context"
	"net/http"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/belune-hq/belune/internal/store/generated"
	"github.com/belune-hq/belune/internal/testutil"
)

// seedDatabaseWithBackup inserts a database plus one backup recorded in destName
// and returns the database id.
func seedDatabaseWithBackup(t *testing.T, projectID pgtype.UUID, destName string) string {
	t.Helper()
	ctx := context.Background()

	db, err := env.Queries.CreateDatabase(ctx, generated.CreateDatabaseParams{
		ProjectID:            projectID,
		Type:                 "postgres",
		Name:                 "doomed",
		Slug:                 "doomed-db",
		Version:              "16",
		Status:               "running",
		CredentialsEncrypted: []byte{0x00},
		BackupMode:           "none",
	})
	require.NoError(t, err)

	dest, err := env.Queries.CreateBackupDestination(ctx, generated.CreateBackupDestinationParams{
		ProjectID:            projectID,
		Name:                 destName,
		Provider:             "s3",
		Endpoint:             "s3.example.com",
		Region:               "us-east-1",
		Bucket:               "b",
		CredentialsEncrypted: []byte{0x00},
	})
	require.NoError(t, err)

	run, err := env.Queries.InsertDatabaseBackup(ctx, generated.InsertDatabaseBackupParams{
		DatabaseID: db.ID,
	})
	require.NoError(t, err)
	require.NoError(t, env.Queries.UpdateDatabaseBackup(ctx, generated.UpdateDatabaseBackupParams{
		ID:        run.ID,
		Status:    "succeeded",
		RemoteKey: pgtype.Text{String: "backups/doomed.backup.gz", Valid: true},
	}))
	_, err = env.Queries.InsertBackupLocation(ctx, generated.InsertBackupLocationParams{
		DatabaseBackupID: run.ID,
		DestinationID:    dest.ID,
		RemoteKey:        pgtype.Text{String: "backups/doomed.backup.gz", Valid: true},
	})
	require.NoError(t, err)

	return uuidToStr(t, db.ID)
}

func uuidToStr(t *testing.T, u pgtype.UUID) string {
	t.Helper()
	v, err := u.Value()
	require.NoError(t, err)
	s, ok := v.(string)
	require.True(t, ok)
	return s
}

// TestGetDatabaseDeletionImpact_ReportsBackupsAndDestinations covers W2: the
// delete path must be able to state what it destroys, not just that the
// database goes away.
func TestGetDatabaseDeletionImpact_ReportsBackupsAndDestinations(t *testing.T) {
	resetDB(t)
	token := env.SetupAdmin(t, "admin@test.com", "password123")
	project := env.CreateProject(t, token, "Doomed", "doomed")
	projectID := project["id"].(string)
	var projUUID pgtype.UUID
	require.NoError(t, projUUID.Scan(projectID))

	dbID := seedDatabaseWithBackup(t, projUUID, "hetzner-s3")

	resp := env.DoRequest(t, "GET",
		"/api/projects/"+projectID+"/databases/"+dbID+"/deletion-impact",
		nil, testutil.AuthHeader(token))
	require.Equal(t, http.StatusOK, resp.StatusCode)

	result := testutil.ReadJSON(t, resp)
	assert.EqualValues(t, 1, result["backup_count"])
	assert.Equal(t, []any{"hetzner-s3"}, result["backup_destinations"])
}

// TestDeleteDatabase_CleansUpBeyondOnePage: cleanupBackups used to list a
// single capped page, while the confirmation dialog counts every backup. Past
// that cap the dialog promised destruction that never happened, leaving the
// surplus objects with no row pointing at them — unreachable and still billed.
// Uses a small page size via many rows would be slow, so this asserts the
// invariant that matters: every backup row is visited, not just the first page.
func TestDeleteDatabase_CleansUpBeyondOnePage(t *testing.T) {
	resetDB(t)
	ctx := context.Background()
	token := env.SetupAdmin(t, "admin@test.com", "password123")
	project := env.CreateProject(t, token, "Paged", "paged")
	projectID := project["id"].(string)
	var projUUID pgtype.UUID
	require.NoError(t, projUUID.Scan(projectID))

	db, err := env.Queries.CreateDatabase(ctx, generated.CreateDatabaseParams{
		ProjectID:            projUUID,
		Type:                 "postgres",
		Name:                 "paged",
		Slug:                 "paged-db",
		Version:              "16",
		Status:               "running",
		CredentialsEncrypted: []byte{0x00},
		BackupMode:           "none",
	})
	require.NoError(t, err)

	// Local-only backups: cleanup removes the file (already absent, which is
	// tolerated) and the row, with no S3 client needed.
	const total = 25
	for i := 0; i < total; i++ {
		run, rErr := env.Queries.InsertDatabaseBackup(ctx, generated.InsertDatabaseBackupParams{
			DatabaseID: db.ID,
		})
		require.NoError(t, rErr)
		require.NoError(t, env.Queries.UpdateDatabaseBackup(ctx, generated.UpdateDatabaseBackupParams{
			ID:        run.ID,
			Status:    "succeeded",
			LocalPath: pgtype.Text{String: t.TempDir() + "/b.gz", Valid: true},
		}))
	}

	count, err := env.Queries.CountDatabaseBackupsWithArtifacts(ctx, db.ID)
	require.NoError(t, err)
	require.EqualValues(t, total, count, "the dialog would report this many")

	resp := env.DoRequest(t, "DELETE",
		"/api/projects/"+projectID+"/databases/"+uuidToStr(t, db.ID),
		nil, testutil.AuthHeader(token))
	require.Equal(t, http.StatusOK, resp.StatusCode)

	remaining, err := env.Queries.ListDatabaseBackups(ctx, generated.ListDatabaseBackupsParams{
		DatabaseID: db.ID, Limit: 100,
	})
	require.NoError(t, err)
	assert.Empty(t, remaining, "every backup the dialog counted must actually be cleaned up")
}
