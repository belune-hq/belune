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

// seedBackupInDestination inserts a database, a succeeded backup, and a
// location row placing that backup in destID — the state that locks a
// destination's identity.
func seedBackupInDestination(t *testing.T, projectID, destID pgtype.UUID) {
	t.Helper()
	ctx := context.Background()

	db, err := env.Queries.CreateDatabase(ctx, generated.CreateDatabaseParams{
		ProjectID:            projectID,
		Type:                 "postgres",
		Name:                 "locked-db",
		Slug:                 "locked-db",
		Version:              "16",
		Status:               "running",
		CredentialsEncrypted: []byte{0x00},
		BackupMode:           "none",
	})
	require.NoError(t, err)

	run, err := env.Queries.InsertDatabaseBackup(ctx, generated.InsertDatabaseBackupParams{
		DatabaseID: db.ID,
	})
	require.NoError(t, err)

	_, err = env.Queries.InsertBackupLocation(ctx, generated.InsertBackupLocationParams{
		DatabaseBackupID: run.ID,
		DestinationID:    destID,
		RemoteKey:        pgtype.Text{String: "backups/locked-db.backup.gz", Valid: true},
	})
	require.NoError(t, err)
}

// TestUpdateBackupDestination_IdentityLockedWhileHoldingBackups covers the third
// W1 failure mode: a destination repointed in place would send every backup
// recorded there at storage its data was never written to.
func TestUpdateBackupDestination_IdentityLockedWhileHoldingBackups(t *testing.T) {
	resetDB(t)
	token := env.SetupAdmin(t, "admin@test.com", "password123")
	project := env.CreateProject(t, token, "Backups", "backups")
	projectID := project["id"].(string)

	create := map[string]any{
		"name":       "hetzner-s3",
		"provider":   "s3",
		"endpoint":   "s3.example.com",
		"region":     "us-east-1",
		"bucket":     "bucket-a",
		"access_key": "ak",
		"secret_key": "sk",
	}
	resp := env.DoRequest(t, "POST", "/api/projects/"+projectID+"/backup-destinations", create, testutil.AuthHeader(token))
	require.Equal(t, http.StatusCreated, resp.StatusCode)
	destID := testutil.ReadJSON(t, resp)["id"].(string)

	// Nothing recorded yet: the bucket is still free to change.
	free := map[string]any{
		"name": "hetzner-s3", "provider": "s3", "endpoint": "s3.example.com",
		"region": "us-east-1", "bucket": "bucket-early",
	}
	resp = env.DoRequest(t, "PUT", "/api/projects/"+projectID+"/backup-destinations/"+destID, free, testutil.AuthHeader(token))
	require.Equal(t, http.StatusOK, resp.StatusCode, "an empty destination should still be editable")

	var destUUID pgtype.UUID
	require.NoError(t, destUUID.Scan(destID))
	var projUUID pgtype.UUID
	require.NoError(t, projUUID.Scan(projectID))
	seedBackupInDestination(t, projUUID, destUUID)

	// Bucket is identity — refused, and the message names the count.
	moved := map[string]any{
		"name": "hetzner-s3", "provider": "s3", "endpoint": "s3.example.com",
		"region": "us-east-1", "bucket": "bucket-b",
	}
	resp = env.DoRequest(t, "PUT", "/api/projects/"+projectID+"/backup-destinations/"+destID, moved, testutil.AuthHeader(token))
	require.Equal(t, http.StatusConflict, resp.StatusCode)
	assert.Contains(t, testutil.ReadJSON(t, resp)["error"], "1 backup(s)")

	// Endpoint is identity too.
	rehosted := map[string]any{
		"name": "hetzner-s3", "provider": "s3", "endpoint": "s3.elsewhere.com",
		"region": "us-east-1", "bucket": "bucket-early",
	}
	resp = env.DoRequest(t, "PUT", "/api/projects/"+projectID+"/backup-destinations/"+destID, rehosted, testutil.AuthHeader(token))
	assert.Equal(t, http.StatusConflict, resp.StatusCode)

	// Credentials and region are not: rotation must stay possible.
	rotated := map[string]any{
		"name": "hetzner-s3", "provider": "s3", "endpoint": "s3.example.com",
		"region": "eu-central-1", "bucket": "bucket-early",
		"access_key": "ak2", "secret_key": "sk2",
	}
	resp = env.DoRequest(t, "PUT", "/api/projects/"+projectID+"/backup-destinations/"+destID, rotated, testutil.AuthHeader(token))
	require.Equal(t, http.StatusOK, resp.StatusCode, "credential and region changes must stay allowed")
	assert.Equal(t, "eu-central-1", testutil.ReadJSON(t, resp)["region"])
}

// TestDeleteUser_CascadesThroughRecordedBackups guards the FK shape on
// backup_locations.destination_id. ON DELETE RESTRICT looked right but broke
// this cascade: Postgres fires a destination's referencing-key triggers as it
// deletes each row, so it tripped on location rows that the parallel
// databases → database_backups → backup_locations cascade was about to remove
// anyway, and deleting a user who owned any recorded backup failed outright.
func TestDeleteUser_CascadesThroughRecordedBackups(t *testing.T) {
	resetDB(t)
	ctx := context.Background()
	adminToken := env.SetupAdmin(t, "admin@test.com", "password123")

	resp := env.DoRequest(t, "POST", "/api/users", map[string]string{
		"email":    "owner@test.com",
		"password": "password123",
		"role":     "member",
	}, testutil.AuthHeader(adminToken))
	require.Equal(t, http.StatusCreated, resp.StatusCode)
	ownerID := testutil.ReadJSON(t, resp)["id"].(string)
	var ownerUUID pgtype.UUID
	require.NoError(t, ownerUUID.Scan(ownerID))

	project, err := env.Queries.CreateProject(ctx, generated.CreateProjectParams{
		Name:     "Owned",
		Slug:     "owned",
		UserID:   ownerUUID,
		ServerID: testutil.LocalServerID(t, ctx, env.Queries),
	})
	require.NoError(t, err)

	dest, err := env.Queries.CreateBackupDestination(ctx, generated.CreateBackupDestinationParams{
		ProjectID:            project.ID,
		Name:                 "hetzner-s3",
		Provider:             "s3",
		Endpoint:             "s3.example.com",
		Region:               "us-east-1",
		Bucket:               "b",
		CredentialsEncrypted: []byte{0x00},
	})
	require.NoError(t, err)
	seedBackupInDestination(t, project.ID, dest.ID)

	resp = env.DoRequest(t, "DELETE", "/api/users/"+ownerID, nil, testutil.AuthHeader(adminToken))
	require.Equal(t, http.StatusOK, resp.StatusCode,
		"deleting a user who owns a recorded backup must cascade, not violate the destination FK")

	count, err := env.Queries.CountLocationsByDestination(ctx, dest.ID)
	require.NoError(t, err)
	assert.Zero(t, count, "location rows should have cascaded away with the project")
}

// TestUpdateBackupDestination_RegionIsIdentityForDerivedEndpoint: an AWS
// destination stores an empty endpoint, and the client derives
// s3.<region>.amazonaws.com from the region — so there the region is the
// address, and changing it repoints recorded backups at a regional endpoint
// their bucket does not live behind.
func TestUpdateBackupDestination_RegionIsIdentityForDerivedEndpoint(t *testing.T) {
	resetDB(t)
	token := env.SetupAdmin(t, "admin@test.com", "password123")
	project := env.CreateProject(t, token, "AWS", "aws")
	projectID := project["id"].(string)
	var projUUID pgtype.UUID
	require.NoError(t, projUUID.Scan(projectID))

	// provider s3 with no endpoint: the AWS shape the edit form produces.
	create := map[string]any{
		"name": "aws-backups", "provider": "s3", "endpoint": "",
		"region": "us-east-1", "bucket": "bucket-a",
		"access_key": "ak", "secret_key": "sk",
	}
	resp := env.DoRequest(t, "POST", "/api/projects/"+projectID+"/backup-destinations", create, testutil.AuthHeader(token))
	require.Equal(t, http.StatusCreated, resp.StatusCode)
	destID := testutil.ReadJSON(t, resp)["id"].(string)
	var destUUID pgtype.UUID
	require.NoError(t, destUUID.Scan(destID))

	seedBackupInDestination(t, projUUID, destUUID)

	moved := map[string]any{
		"name": "aws-backups", "provider": "s3", "endpoint": "",
		"region": "eu-central-1", "bucket": "bucket-a",
	}
	resp = env.DoRequest(t, "PUT", "/api/projects/"+projectID+"/backup-destinations/"+destID, moved, testutil.AuthHeader(token))
	assert.Equal(t, http.StatusConflict, resp.StatusCode,
		"region change on a derived-endpoint destination moves where objects are read from")

	// Credential rotation must still work — region unchanged.
	rotated := map[string]any{
		"name": "aws-backups", "provider": "s3", "endpoint": "",
		"region": "us-east-1", "bucket": "bucket-a",
		"access_key": "ak2", "secret_key": "sk2",
	}
	resp = env.DoRequest(t, "PUT", "/api/projects/"+projectID+"/backup-destinations/"+destID, rotated, testutil.AuthHeader(token))
	assert.Equal(t, http.StatusOK, resp.StatusCode, "credential rotation must stay possible")
}
