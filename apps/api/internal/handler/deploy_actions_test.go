package handler_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/belune-hq/belune/internal/status"
	"github.com/belune-hq/belune/internal/store/generated"
	"github.com/belune-hq/belune/internal/testutil"
)

func idStr(u pgtype.UUID) string { return uuid.UUID(u.Bytes).String() }

func appActionPath(projectID, appID, action string) string {
	return fmt.Sprintf("/api/projects/%s/applications/%s/%s", projectID, appID, action)
}

// seedApp creates a project + application of the given type, returning the
// project id string and the application row.
func seedApp(t *testing.T, adminToken, slug, appType, buildType string) (string, generated.Application) {
	t.Helper()
	proj := env.CreateProject(t, adminToken, "Proj "+slug, "proj-"+slug)
	var projID pgtype.UUID
	require.NoError(t, projID.Scan(proj["id"].(string)))

	params := generated.CreateApplicationParams{
		ProjectID: projID, Name: "App " + slug, Slug: "app-" + slug,
		Type: appType, BuildType: buildType,
	}
	if appType == "image" {
		params.SourceImage = pgtype.Text{String: "nginx:1.27", Valid: true}
	} else {
		params.SourceRepo = pgtype.Text{String: "https://github.com/example/repo", Valid: true}
	}
	app, err := env.Queries.CreateApplication(context.Background(), params)
	require.NoError(t, err)
	return proj["id"].(string), app
}

// seedSuccessDeployment inserts a successful deployment carrying image_tag and
// (optionally) commit_sha, satisfying GetLatestSuccessfulDeployment.
func seedSuccessDeployment(t *testing.T, appID pgtype.UUID, imageTag, commit string) {
	t.Helper()
	ctx := context.Background()
	dep, err := env.Queries.CreateDeployment(ctx, generated.CreateDeploymentParams{
		ApplicationID: appID, Status: status.DeploymentPending, TriggeredBy: "manual",
	})
	require.NoError(t, err)
	require.NoError(t, env.Queries.UpdateDeploymentImageTag(ctx, generated.UpdateDeploymentImageTagParams{
		ID: dep.ID, ImageTag: pgtype.Text{String: imageTag, Valid: true},
	}))
	if commit != "" {
		require.NoError(t, env.Queries.UpdateDeploymentCommitSha(ctx, generated.UpdateDeploymentCommitShaParams{
			ID: dep.ID, CommitSha: pgtype.Text{String: commit, Valid: true},
		}))
	}
	_, err = env.Queries.UpdateDeploymentStatus(ctx, generated.UpdateDeploymentStatusParams{
		ID: dep.ID, Status: status.DeploymentSuccess,
	})
	require.NoError(t, err)
}

func decodePayload(t *testing.T, payload []byte) map[string]any {
	t.Helper()
	var m map[string]any
	require.NoError(t, json.Unmarshal(payload, &m))
	return m
}

func TestReloadApplication(t *testing.T) {
	resetDB(t)
	adminToken := env.SetupAdmin(t, "admin@test.com", "password123")
	projID, app := seedApp(t, adminToken, "reload", "git", "dockerfile")
	appID := idStr(app.ID)

	// No successful deployment yet → 409.
	resp := env.DoRequest(t, "POST", appActionPath(projID, appID, "reload"), nil, testutil.AuthHeader(adminToken))
	assert.Equal(t, http.StatusConflict, resp.StatusCode)
	resp.Body.Close()

	seedSuccessDeployment(t, app.ID, "app-reload:abc12345", "")

	env.Asynq.Tasks = nil
	resp = env.DoRequest(t, "POST", appActionPath(projID, appID, "reload"), nil, testutil.AuthHeader(adminToken))
	require.Equal(t, http.StatusAccepted, resp.StatusCode)
	resp.Body.Close()

	// Enqueued a deploy task that reuses the current image (skip build) and does
	// not pin a commit.
	require.Len(t, env.Asynq.Tasks, 1)
	pl := decodePayload(t, env.Asynq.Tasks[0].Payload)
	assert.Equal(t, "app-reload:abc12345", pl["rollback_image_tag"])
	assert.Nil(t, pl["commit_sha"])
}

func TestRebuildApplication_GitPinsCommit(t *testing.T) {
	resetDB(t)
	adminToken := env.SetupAdmin(t, "admin@test.com", "password123")
	projID, app := seedApp(t, adminToken, "rebuild", "git", "dockerfile")
	appID := idStr(app.ID)

	// Deployment without a commit → 409.
	seedSuccessDeployment(t, app.ID, "app-rebuild:dep00001", "")
	resp := env.DoRequest(t, "POST", appActionPath(projID, appID, "rebuild"), nil, testutil.AuthHeader(adminToken))
	assert.Equal(t, http.StatusConflict, resp.StatusCode)
	resp.Body.Close()

	// A later deployment with a commit → 202, payload pins the commit.
	seedSuccessDeployment(t, app.ID, "app-rebuild:dep00002", "abcdef1234567890abcdef1234567890abcdef12")

	env.Asynq.Tasks = nil
	resp = env.DoRequest(t, "POST", appActionPath(projID, appID, "rebuild"), nil, testutil.AuthHeader(adminToken))
	require.Equal(t, http.StatusAccepted, resp.StatusCode)
	resp.Body.Close()

	require.Len(t, env.Asynq.Tasks, 1)
	pl := decodePayload(t, env.Asynq.Tasks[0].Payload)
	assert.Equal(t, "abcdef1234567890abcdef1234567890abcdef12", pl["commit_sha"])
	assert.Nil(t, pl["rollback_image_tag"])
}

func TestRebuildApplication_ImageTypeRejected(t *testing.T) {
	resetDB(t)
	adminToken := env.SetupAdmin(t, "admin@test.com", "password123")
	projID, app := seedApp(t, adminToken, "img", "image", "image")
	seedSuccessDeployment(t, app.ID, "nginx@sha256:deadbeef", "")

	resp := env.DoRequest(t, "POST", appActionPath(projID, idStr(app.ID), "rebuild"), nil, testutil.AuthHeader(adminToken))
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
	resp.Body.Close()
}

// TestUpgradeDatabase_SameVersionAllowed verifies the relaxed guard: targeting
// the current version is accepted as a "refresh to latest patch" (previously 400).
func TestUpgradeDatabase_SameVersionAllowed(t *testing.T) {
	resetDB(t)
	adminToken := env.SetupAdmin(t, "admin@test.com", "password123")
	proj := env.CreateProject(t, adminToken, "Proj db", "proj-db")
	var projID pgtype.UUID
	require.NoError(t, projID.Scan(proj["id"].(string)))

	db, err := env.Queries.CreateDatabase(context.Background(), generated.CreateDatabaseParams{
		ProjectID: projID, Type: "postgres", Name: "PG", Slug: "pg", Version: "18",
		Status: status.DatabaseRunning, BackupMode: "none",
	})
	require.NoError(t, err)

	body := map[string]string{"target_version": "18"} // same as current
	resp := env.DoRequest(t, "POST",
		fmt.Sprintf("/api/projects/%s/databases/%s/upgrade", proj["id"].(string), idStr(db.ID)),
		body, testutil.AuthHeader(adminToken))
	assert.Equal(t, http.StatusAccepted, resp.StatusCode)
	resp.Body.Close()
}
