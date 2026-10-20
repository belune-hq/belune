package handler_test

import (
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/belune-hq/belune/internal/status"
	"github.com/belune-hq/belune/internal/store/generated"
	"github.com/belune-hq/belune/internal/testutil"
)

// TestContainerLogSessions verifies that container logs are grouped into
// per-container-generation sessions, labelled with the deployment that produced
// them, and filterable by one session or by the unassigned ("earlier") bucket.
func TestContainerLogSessions(t *testing.T) {
	resetDB(t)
	adminToken := env.SetupAdmin(t, "admin@test.com", "password123")

	project := env.CreateProject(t, adminToken, "Log Project", "log-project")
	projectID := extractID(project["id"])
	app := env.CreateApplication(t, adminToken, projectID, map[string]any{
		"name": "Log App", "type": "image", "build_type": "image",
		"source_image": "nginx:latest",
	})
	appID := extractID(app["id"])
	var appUUID pgtype.UUID
	require.NoError(t, appUUID.Scan(appID))

	ctx := context.Background()

	// Two deployments (two image generations) plus an unassigned line.
	dep1, err := env.Queries.CreateDeployment(ctx, generated.CreateDeploymentParams{
		ApplicationID: appUUID, Status: status.DeploymentSuccess, TriggeredBy: "manual",
	})
	require.NoError(t, err)
	dep2, err := env.Queries.CreateDeployment(ctx, generated.CreateDeploymentParams{
		ApplicationID: appUUID, Status: status.DeploymentSuccess, TriggeredBy: "rollback",
	})
	require.NoError(t, err)

	insert := func(msg string, dep pgtype.UUID, ctr pgtype.Text) {
		require.NoError(t, env.Queries.InsertContainerLog(ctx, generated.InsertContainerLogParams{
			SourceType:   "application",
			SourceID:     appUUID,
			Level:        "info",
			Stream:       "stdout",
			Message:      msg,
			DeploymentID: dep, ContainerID: ctr,
		}))
	}
	insert("line from dep1", dep1.ID, pgtype.Text{String: "ctr-1", Valid: true})
	insert("line from dep1 again", dep1.ID, pgtype.Text{String: "ctr-1", Valid: true})
	insert("line from dep2", dep2.ID, pgtype.Text{String: "ctr-2", Valid: true})
	insert("orphan line", pgtype.UUID{}, pgtype.Text{}) // no session → "earlier" bucket

	// Sessions endpoint: three groups (dep1, dep2, NULL), each enriched.
	resp := env.DoRequest(t, "GET", fmt.Sprintf("/api/projects/%s/applications/%s/logs/sessions", projectID, appID), nil, testutil.AuthHeader(adminToken))
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	sessions := testutil.ReadJSONArray(t, resp)
	require.Len(t, sessions, 3)

	// Locate dep1's session and check its line count + enrichment.
	var found bool
	for _, s := range sessions {
		row := s.(map[string]any)
		if row["container_id"] == "ctr-1" {
			found = true
			assert.EqualValues(t, 2, row["line_count"])
			assert.Equal(t, "manual", row["triggered_by"])
		}
	}
	assert.True(t, found, "the first container's session should be present with its deployment metadata")

	// Filter history to dep1: exactly its two lines.
	resp = env.DoRequest(t, "GET", fmt.Sprintf("/api/projects/%s/applications/%s/logs/history?session=ctr-1", projectID, appID), nil, testutil.AuthHeader(adminToken))
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Len(t, testutil.ReadJSONArray(t, resp), 2)

	// Filter to the unassigned bucket: only the orphan line.
	resp = env.DoRequest(t, "GET", fmt.Sprintf("/api/projects/%s/applications/%s/logs/history?session=none", projectID, appID), nil, testutil.AuthHeader(adminToken))
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	noneLogs := testutil.ReadJSONArray(t, resp)
	require.Len(t, noneLogs, 1)
	assert.Equal(t, "orphan line", noneLogs[0].(map[string]any)["message"])

	// No filter: all four lines.
	resp = env.DoRequest(t, "GET", fmt.Sprintf("/api/projects/%s/applications/%s/logs/history", projectID, appID), nil, testutil.AuthHeader(adminToken))
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Len(t, testutil.ReadJSONArray(t, resp), 4)

	// An unknown session returns nothing rather than silently falling back to
	// every session's logs, which would quietly show more than was asked for.
	// Container ids are opaque strings, so there is no "malformed" form to
	// reject — only one that matches nothing.
	resp = env.DoRequest(t, "GET", fmt.Sprintf("/api/projects/%s/applications/%s/logs/history?session=no-such-container", projectID, appID), nil, testutil.AuthHeader(adminToken))
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Empty(t, testutil.ReadJSONArray(t, resp),
		"an unknown session must not fall back to showing everything")
}

// An application gains a session per deploy for its whole life, and each one
// becomes an entry in the viewer's session picker, so the listing is capped.
func TestContainerLogSessions_IsBounded(t *testing.T) {
	resetDB(t)
	adminToken := env.SetupAdmin(t, "admin@test.com", "password123")

	project := env.CreateProject(t, adminToken, "Busy Project", "busy-project")
	projectID := extractID(project["id"])
	app := env.CreateApplication(t, adminToken, projectID, map[string]any{
		"name": "Busy App", "type": "image", "build_type": "image",
		"source_image": "nginx:latest",
	})
	appID := extractID(app["id"])
	var appUUID pgtype.UUID
	require.NoError(t, appUUID.Scan(appID))

	ctx := context.Background()
	const sessions = 60 // comfortably past the 50 cap

	for i := 0; i < sessions; i++ {
		d, err := env.Queries.CreateDeployment(ctx, generated.CreateDeploymentParams{
			ApplicationID: appUUID, Status: status.DeploymentSuccess, TriggeredBy: "manual",
		})
		require.NoError(t, err)
		require.NoError(t, env.Queries.InsertContainerLog(ctx, generated.InsertContainerLogParams{
			SourceType: "application", SourceID: appUUID,
			Level: "info", Stream: "stdout",
			Message:      fmt.Sprintf("line from session %d", i),
			DeploymentID: d.ID,
			// Each deploy gets its own container, which is what makes it a
			// distinct session.
			ContainerID: pgtype.Text{String: fmt.Sprintf("ctr-%02d", i), Valid: true},
			// Spread the timestamps so "most recent first" is well defined.
			RecordedAt: pgtype.Timestamptz{Time: time.Now().Add(time.Duration(i) * time.Minute), Valid: true},
		}))
	}

	resp := env.DoRequest(t, "GET", fmt.Sprintf("/api/projects/%s/applications/%s/logs/sessions", projectID, appID), nil, testutil.AuthHeader(adminToken))
	require.Equal(t, http.StatusOK, resp.StatusCode)
	rows := testutil.ReadJSONArray(t, resp)
	assert.Len(t, rows, 50, "the session listing must be capped")

	// The cap keeps the newest, which are the ones anyone reads.
	first := rows[0].(map[string]any)
	assert.EqualValues(t, 1, first["line_count"])

	// Every line is still reachable unfiltered — capping the picker must not
	// hide log content.
	resp = env.DoRequest(t, "GET", fmt.Sprintf("/api/projects/%s/applications/%s/logs/history?limit=1000", projectID, appID), nil, testutil.AuthHeader(adminToken))
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Len(t, testutil.ReadJSONArray(t, resp), sessions,
		"all lines remain readable in the unfiltered view")
}
