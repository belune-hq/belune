package handler_test

import (
	"fmt"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/belune-hq/belune/internal/testutil"
)

// The update endpoint replaces the whole set, so a variable the caller omits
// has been deleted in the editor. Upserting alone left it in the database and
// the deploy kept injecting it — removing a variable appeared to work but did
// nothing.
func TestUpdateEnvVars_RemovesOmittedKeys(t *testing.T) {
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

	put := func(vars []map[string]any) *http.Response {
		return env.DoRequest(t, "PUT", path, map[string]any{"vars": vars}, testutil.AuthHeader(adminToken))
	}
	keys := func() []string {
		resp := env.DoRequest(t, "GET", path, nil, testutil.AuthHeader(adminToken))
		require.Equal(t, http.StatusOK, resp.StatusCode)
		var out []string
		for _, row := range testutil.ReadJSONArray(t, resp) {
			out = append(out, row.(map[string]any)["key"].(string))
		}
		return out
	}

	resp := put([]map[string]any{
		{"key": "KEEP", "value": "1", "is_secret": false},
		{"key": "DROP", "value": "2", "is_secret": false},
	})
	require.Equal(t, http.StatusOK, resp.StatusCode)
	resp.Body.Close()
	assert.ElementsMatch(t, []string{"KEEP", "DROP"}, keys())

	// Resubmit without DROP: it must be gone, and KEEP must survive.
	resp = put([]map[string]any{{"key": "KEEP", "value": "1", "is_secret": false}})
	require.Equal(t, http.StatusOK, resp.StatusCode)
	resp.Body.Close()
	assert.Equal(t, []string{"KEEP"}, keys(), "an omitted variable must be deleted")

	// An empty set clears everything — "this app now has no variables".
	resp = put([]map[string]any{})
	require.Equal(t, http.StatusOK, resp.StatusCode)
	resp.Body.Close()
	assert.Empty(t, keys(), "submitting no variables clears them all")
}

// Validation runs before any write, so a rejected request must leave the
// existing variables untouched rather than partially rewritten.
func TestUpdateEnvVars_InvalidKeyLeavesExistingIntact(t *testing.T) {
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
		{"key": "ORIGINAL", "value": "keep me", "is_secret": false},
	}}, testutil.AuthHeader(adminToken))
	require.Equal(t, http.StatusOK, resp.StatusCode)
	resp.Body.Close()

	// A key the regex rejects, submitted alongside a valid one.
	resp = env.DoRequest(t, "PUT", path, map[string]any{"vars": []map[string]any{
		{"key": "FINE", "value": "x", "is_secret": false},
		{"key": "BAD-KEY", "value": "y", "is_secret": false},
	}}, testutil.AuthHeader(adminToken))
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
	resp.Body.Close()

	resp = env.DoRequest(t, "GET", path, nil, testutil.AuthHeader(adminToken))
	require.Equal(t, http.StatusOK, resp.StatusCode)
	rows := testutil.ReadJSONArray(t, resp)
	require.Len(t, rows, 1, "a rejected request must not change the stored set")
	assert.Equal(t, "ORIGINAL", rows[0].(map[string]any)["key"])
}

// The project endpoint used to delete every variable before validating, so one
// bad key wiped the whole set with no transaction to undo it.
func TestUpdateProjectEnvVars_InvalidKeyDoesNotWipe(t *testing.T) {
	resetDB(t)
	adminToken := env.SetupAdmin(t, "admin@test.com", "password123")

	project := env.CreateProject(t, adminToken, "Env Project", "env-project")
	projectID := extractID(project["id"])
	path := fmt.Sprintf("/api/projects/%s/env", projectID)

	resp := env.DoRequest(t, "PUT", path, map[string]any{"vars": []map[string]any{
		{"key": "SHARED", "value": "keep me", "is_secret": false},
	}}, testutil.AuthHeader(adminToken))
	require.Equal(t, http.StatusOK, resp.StatusCode)
	resp.Body.Close()

	resp = env.DoRequest(t, "PUT", path, map[string]any{"vars": []map[string]any{
		{"key": "BAD-KEY", "value": "y", "is_secret": false},
	}}, testutil.AuthHeader(adminToken))
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
	resp.Body.Close()

	resp = env.DoRequest(t, "GET", path, nil, testutil.AuthHeader(adminToken))
	require.Equal(t, http.StatusOK, resp.StatusCode)
	rows := testutil.ReadJSONArray(t, resp)
	require.Len(t, rows, 1, "a rejected request must not have deleted anything")
	assert.Equal(t, "SHARED", rows[0].(map[string]any)["key"])

	// Removal still works on the project endpoint.
	resp = env.DoRequest(t, "PUT", path, map[string]any{"vars": []map[string]any{}}, testutil.AuthHeader(adminToken))
	require.Equal(t, http.StatusOK, resp.StatusCode)
	resp.Body.Close()
	resp = env.DoRequest(t, "GET", path, nil, testutil.AuthHeader(adminToken))
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Empty(t, testutil.ReadJSONArray(t, resp))
}
