package handler_test

import (
	"fmt"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/belune-hq/belune/internal/testutil"
)

// The bug: the list endpoint masks a secret's value as "••••••••". The old
// editor sent every row back verbatim on save, so re-saving after editing a
// DIFFERENT variable silently overwrote the secret with the literal mask. The
// "unchanged" marker is how the rework avoids that.
func TestUpdateEnvVars_UnchangedSecretPreservesValue(t *testing.T) {
	resetDB(t)
	adminToken := env.SetupAdmin(t, "admin@test.com", "password123")

	project := env.CreateProject(t, adminToken, "Env Project", "env-project")
	projectID := extractID(project["id"])
	app := env.CreateApplication(t, adminToken, projectID, map[string]any{
		"name": "Env App", "type": "image", "build_type": "image",
		"source_image": "nginx:latest",
	})
	appID := extractID(app["id"])
	path := fmt.Sprintf("/api/projects/%s/applications/%s/env", projectID, appID)

	resp := env.DoRequest(t, "PUT", path, map[string]any{"vars": []map[string]any{
		{"key": "API_KEY", "value": "real-secret-value", "is_secret": true},
	}}, testutil.AuthHeader(adminToken))
	require.Equal(t, http.StatusOK, resp.StatusCode)
	resp.Body.Close()

	resp = env.DoRequest(t, "GET", path, nil, testutil.AuthHeader(adminToken))
	require.Equal(t, http.StatusOK, resp.StatusCode)
	rows := testutil.ReadJSONArray(t, resp)
	require.Len(t, rows, 1)
	row := rows[0].(map[string]any)
	assert.Nil(t, row["value"], "list must never send a secret's value")
	envVarID := row["id"].(string)

	// Re-save as the editor would: the secret row is untouched (its value
	// field was never populated) and marked unchanged, while a new variable
	// is added alongside it.
	resp = env.DoRequest(t, "PUT", path, map[string]any{"vars": []map[string]any{
		{"key": "API_KEY", "value": "", "is_secret": true, "unchanged": true},
		{"key": "OTHER", "value": "x", "is_secret": false},
	}}, testutil.AuthHeader(adminToken))
	require.Equal(t, http.StatusOK, resp.StatusCode)
	resp.Body.Close()

	revealPath := fmt.Sprintf("%s/%s/reveal", path, envVarID)
	resp = env.DoRequest(t, "GET", revealPath, nil, testutil.AuthHeader(adminToken))
	require.Equal(t, http.StatusOK, resp.StatusCode)
	revealed := testutil.ReadJSON(t, resp)
	assert.Equal(t, "real-secret-value", revealed["value"],
		"an unchanged secret must survive a save untouched, not be overwritten with the mask")
}

func TestRevealEnvVar(t *testing.T) {
	resetDB(t)
	adminToken := env.SetupAdmin(t, "admin@test.com", "password123")

	project := env.CreateProject(t, adminToken, "Env Project", "env-project")
	projectID := extractID(project["id"])
	app := env.CreateApplication(t, adminToken, projectID, map[string]any{
		"name": "Env App", "type": "image", "build_type": "image",
		"source_image": "nginx:latest",
	})
	appID := extractID(app["id"])
	path := fmt.Sprintf("/api/projects/%s/applications/%s/env", projectID, appID)

	resp := env.DoRequest(t, "PUT", path, map[string]any{"vars": []map[string]any{
		{"key": "TOKEN", "value": "shh-its-a-secret", "is_secret": true},
	}}, testutil.AuthHeader(adminToken))
	require.Equal(t, http.StatusOK, resp.StatusCode)
	resp.Body.Close()

	resp = env.DoRequest(t, "GET", path, nil, testutil.AuthHeader(adminToken))
	rows := testutil.ReadJSONArray(t, resp)
	require.Len(t, rows, 1)
	envVarID := rows[0].(map[string]any)["id"].(string)

	resp = env.DoRequest(t, "GET", fmt.Sprintf("%s/%s/reveal", path, envVarID), nil, testutil.AuthHeader(adminToken))
	require.Equal(t, http.StatusOK, resp.StatusCode)
	body := testutil.ReadJSON(t, resp)
	assert.Equal(t, "shh-its-a-secret", body["value"])

	// A reveal for an env var that belongs to a different application must
	// 404, not leak across applications.
	otherApp := env.CreateApplication(t, adminToken, projectID, map[string]any{
		"name": "Other App", "type": "image", "build_type": "image",
		"source_image": "nginx:latest",
	})
	otherAppID := extractID(otherApp["id"])
	resp = env.DoRequest(t, "GET",
		fmt.Sprintf("/api/projects/%s/applications/%s/env/%s/reveal", projectID, otherAppID, envVarID),
		nil, testutil.AuthHeader(adminToken))
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
	resp.Body.Close()
}

func TestRevealProjectEnvVar(t *testing.T) {
	resetDB(t)
	adminToken := env.SetupAdmin(t, "admin@test.com", "password123")

	project := env.CreateProject(t, adminToken, "Env Project", "env-project")
	projectID := extractID(project["id"])
	path := fmt.Sprintf("/api/projects/%s/env", projectID)

	resp := env.DoRequest(t, "PUT", path, map[string]any{"vars": []map[string]any{
		{"key": "SHARED_SECRET", "value": "project-level-secret", "is_secret": true},
	}}, testutil.AuthHeader(adminToken))
	require.Equal(t, http.StatusOK, resp.StatusCode)
	resp.Body.Close()

	resp = env.DoRequest(t, "GET", path, nil, testutil.AuthHeader(adminToken))
	rows := testutil.ReadJSONArray(t, resp)
	require.Len(t, rows, 1)
	envVarID := rows[0].(map[string]any)["id"].(string)

	resp = env.DoRequest(t, "GET", fmt.Sprintf("%s/%s/reveal", path, envVarID), nil, testutil.AuthHeader(adminToken))
	require.Equal(t, http.StatusOK, resp.StatusCode)
	body := testutil.ReadJSON(t, resp)
	assert.Equal(t, "project-level-secret", body["value"])
}
