package handler_test

import (
	"context"
	"fmt"
	"net/http"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/belune-hq/belune/internal/store/generated"
	"github.com/belune-hq/belune/internal/testutil"
)

// markerState reads the two change markers straight from the row, so the test
// asserts on what was stored rather than on the derived field alone.
func markerState(t *testing.T, appID string) (config, source bool) {
	t.Helper()
	require.NoError(t, env.Pool.QueryRow(context.Background(),
		`SELECT config_changed_at IS NOT NULL, source_changed_at IS NOT NULL
		   FROM applications WHERE id = $1`, appID).Scan(&config, &source))
	return config, source
}

func fetchApp(t *testing.T, token, projectID, appID string) map[string]any {
	t.Helper()
	resp := env.DoRequest(t, "GET",
		fmt.Sprintf("/api/projects/%s/applications/%s", projectID, appID),
		nil, testutil.AuthHeader(token))
	require.Equal(t, http.StatusOK, resp.StatusCode)
	return testutil.ReadJSON(t, resp)
}

// markDeployed fakes "this app has deployed at least once", which is what
// un-suppresses the indicator.
func markDeployed(t *testing.T, appID string) {
	t.Helper()
	_, err := env.Pool.Exec(context.Background(),
		`UPDATE applications SET last_deployed_at = NOW() WHERE id = $1`, appID)
	require.NoError(t, err)
}

func TestChangeMarkers_StampedAndSuppressed(t *testing.T) {
	resetDB(t)
	token := env.SetupAdmin(t, "admin@test.com", "password123")
	project := env.CreateProject(t, token, "Test Project", "test-project")
	projectID := extractID(project["id"])
	app := env.CreateApplication(t, token, projectID, map[string]any{
		"name": "Marker App", "type": "git",
		"build_type": "dockerfile", "source_repo": "https://github.com/test/repo",
	})
	appID := extractID(app["id"])

	// A brand new application has nothing outstanding.
	assert.Equal(t, "", fetchApp(t, token, projectID, appID)["pending_change"])

	// Saving an env var is a config change.
	resp := env.DoRequest(t, "PUT",
		fmt.Sprintf("/api/projects/%s/applications/%s/env", projectID, appID),
		map[string]any{"vars": []map[string]any{{"key": "FOO", "value": "bar"}}},
		testutil.AuthHeader(token))
	require.Equal(t, http.StatusOK, resp.StatusCode)
	resp.Body.Close()

	config, source := markerState(t, appID)
	assert.True(t, config, "env var save must stamp the config marker")
	assert.False(t, source, "env var save must not stamp the source marker")

	// ...but it is not reported yet, because the app has never deployed. This
	// is the false positive that would otherwise show from birth.
	assert.Equal(t, "", fetchApp(t, token, projectID, appID)["pending_change"],
		"indicator must stay suppressed until the first successful deploy")

	markDeployed(t, appID)
	assert.Equal(t, "config", fetchApp(t, token, projectID, appID)["pending_change"])
}

func TestChangeMarkers_SourceOutranksConfig(t *testing.T) {
	resetDB(t)
	token := env.SetupAdmin(t, "admin@test.com", "password123")
	project := env.CreateProject(t, token, "Test Project", "test-project")
	projectID := extractID(project["id"])
	app := env.CreateApplication(t, token, projectID, map[string]any{
		"name": "Marker App", "type": "git",
		"build_type": "dockerfile", "source_repo": "https://github.com/test/repo",
	})
	appID := extractID(app["id"])
	markDeployed(t, appID)

	// Changing the branch is a source change: only a real build applies it.
	resp := env.DoRequest(t, "PUT",
		fmt.Sprintf("/api/projects/%s/applications/%s", projectID, appID),
		map[string]any{
			"name": "Marker App", "source_repo": "https://github.com/test/repo",
			"branch": "release/2.1",
		}, testutil.AuthHeader(token))
	require.Equal(t, http.StatusOK, resp.StatusCode)
	resp.Body.Close()

	_, source := markerState(t, appID)
	assert.True(t, source, "branch change must stamp the source marker")
	assert.Equal(t, "source", fetchApp(t, token, projectID, appID)["pending_change"])

	// A config change on top does not downgrade the reported severity — a
	// reload would not pick up the branch.
	resp = env.DoRequest(t, "PUT",
		fmt.Sprintf("/api/projects/%s/applications/%s/env", projectID, appID),
		map[string]any{"vars": []map[string]any{{"key": "FOO", "value": "bar"}}},
		testutil.AuthHeader(token))
	require.Equal(t, http.StatusOK, resp.StatusCode)
	resp.Body.Close()

	assert.Equal(t, "source", fetchApp(t, token, projectID, appID)["pending_change"])
}

// Changing root_directory needs a real build from the new subdirectory, so it
// must stamp the source marker like dockerfile_path and branch do — not the
// weaker config marker, which only triggers a reload of the existing image.
func TestChangeMarkers_RootDirectoryChangeStampsSource(t *testing.T) {
	resetDB(t)
	token := env.SetupAdmin(t, "admin@test.com", "password123")
	project := env.CreateProject(t, token, "Test Project", "test-project")
	projectID := extractID(project["id"])
	app := env.CreateApplication(t, token, projectID, map[string]any{
		"name": "Marker App", "type": "git",
		"build_type": "dockerfile", "source_repo": "https://github.com/test/repo",
	})
	appID := extractID(app["id"])
	markDeployed(t, appID)

	resp := env.DoRequest(t, "PUT",
		fmt.Sprintf("/api/projects/%s/applications/%s", projectID, appID),
		map[string]any{
			"name": "Marker App", "source_repo": "https://github.com/test/repo",
			"root_directory": "apps/web",
		}, testutil.AuthHeader(token))
	require.Equal(t, http.StatusOK, resp.StatusCode)
	resp.Body.Close()

	_, source := markerState(t, appID)
	assert.True(t, source, "root directory change must stamp the source marker")
	assert.Equal(t, "source", fetchApp(t, token, projectID, appID)["pending_change"])
}

// A save that changes nothing must stamp nothing, or the indicator becomes
// noise the user cannot clear by doing what it asks.
func TestChangeMarkers_NoOpSaveStampsNothing(t *testing.T) {
	resetDB(t)
	token := env.SetupAdmin(t, "admin@test.com", "password123")
	project := env.CreateProject(t, token, "Test Project", "test-project")
	projectID := extractID(project["id"])
	app := env.CreateApplication(t, token, projectID, map[string]any{
		"name": "Marker App", "type": "git",
		"build_type": "dockerfile", "source_repo": "https://github.com/test/repo",
	})
	appID := extractID(app["id"])
	markDeployed(t, appID)

	resp := env.DoRequest(t, "PUT",
		fmt.Sprintf("/api/projects/%s/applications/%s", projectID, appID),
		map[string]any{"name": "Marker App", "source_repo": "https://github.com/test/repo"},
		testutil.AuthHeader(token))
	require.Equal(t, http.StatusOK, resp.StatusCode)
	resp.Body.Close()

	config, source := markerState(t, appID)
	assert.False(t, config)
	assert.False(t, source)
}

// The asymmetry is the whole reason there are two columns: a reload applies
// config but produces no new image, so it must not clear the source marker.
func TestChangeMarkers_ClearingIsAsymmetric(t *testing.T) {
	resetDB(t)
	token := env.SetupAdmin(t, "admin@test.com", "password123")
	project := env.CreateProject(t, token, "Test Project", "test-project")
	projectID := extractID(project["id"])
	app := env.CreateApplication(t, token, projectID, map[string]any{
		"name": "Marker App", "type": "image", "build_type": "image",
		"source_image": "nginx:1.25",
	})
	appID := extractID(app["id"])
	ctx := context.Background()

	var appUUID pgtype.UUID
	require.NoError(t, appUUID.Scan(appID))

	require.NoError(t, env.Queries.TouchApplicationConfigChanged(ctx, appUUID))
	require.NoError(t, env.Queries.TouchApplicationSourceChanged(ctx, appUUID))

	// A deployment started after both markers.
	dep, err := env.Queries.CreateDeployment(ctx, generated.CreateDeploymentParams{
		ApplicationID: appUUID, Status: "pending", TriggeredBy: "manual",
	})
	require.NoError(t, err)

	// The skip-build path (reload / rollback).
	require.NoError(t, env.Queries.ClearApplicationConfigChanged(ctx,
		generated.ClearApplicationConfigChangedParams{ID: appUUID, ID_2: dep.ID}))

	config, source := markerState(t, appID)
	assert.False(t, config, "reload applies config")
	assert.True(t, source, "reload produces no new image, so the source marker must survive")

	// The build/pull path clears the rest.
	require.NoError(t, env.Queries.ClearApplicationSourceChanged(ctx,
		generated.ClearApplicationSourceChangedParams{ID: appUUID, ID_2: dep.ID}))

	_, source = markerState(t, appID)
	assert.False(t, source)
}

// A change saved while a deploy is already running is not applied by that
// deploy, so its marker must outlive it. Losing it would be the dangerous
// failure: silently telling the user their edit is live when it is not.
func TestChangeMarkers_EditDuringDeploySurvives(t *testing.T) {
	resetDB(t)
	token := env.SetupAdmin(t, "admin@test.com", "password123")
	project := env.CreateProject(t, token, "Test Project", "test-project")
	projectID := extractID(project["id"])
	app := env.CreateApplication(t, token, projectID, map[string]any{
		"name": "Marker App", "type": "image", "build_type": "image",
		"source_image": "nginx:1.25",
	})
	appID := extractID(app["id"])
	ctx := context.Background()

	var appUUID pgtype.UUID
	require.NoError(t, appUUID.Scan(appID))

	// Deployment starts first...
	dep, err := env.Queries.CreateDeployment(ctx, generated.CreateDeploymentParams{
		ApplicationID: appUUID, Status: "pending", TriggeredBy: "manual",
	})
	require.NoError(t, err)

	// ...then the edit lands, mid-flight.
	require.NoError(t, env.Queries.TouchApplicationConfigChanged(ctx, appUUID))

	require.NoError(t, env.Queries.ClearApplicationConfigChanged(ctx,
		generated.ClearApplicationConfigChangedParams{ID: appUUID, ID_2: dep.ID}))

	config, _ := markerState(t, appID)
	assert.True(t, config, "an edit made after the deploy started must keep its marker")
}
