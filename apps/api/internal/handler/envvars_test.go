package handler_test

import (
	"fmt"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/belune-hq/belune/internal/testutil"
)

func TestUpdateAndListEnvVars(t *testing.T) {
	resetDB(t)
	token := env.SetupAdmin(t, "admin@test.com", "password123")
	project := env.CreateProject(t, token, "Test Project", "test-project")
	projectID := extractID(project["id"])
	app := env.CreateApplication(t, token, projectID, map[string]any{
		"name": "Env App", "type": "git", "build_type": "dockerfile", "source_repo": "https://github.com/test/repo",
	})
	appID := extractID(app["id"])

	// Set env vars
	resp := env.DoRequest(t, "PUT", fmt.Sprintf("/api/projects/%s/applications/%s/env", projectID, appID), map[string]any{
		"vars": []map[string]any{
			{"key": "DATABASE_URL", "value": "postgres://localhost/db", "is_secret": false},
			{"key": "API_KEY", "value": "secret-key-123", "is_secret": true},
		},
	}, testutil.AuthHeader(token))
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	resp.Body.Close()

	// List env vars
	resp = env.DoRequest(t, "GET", fmt.Sprintf("/api/projects/%s/applications/%s/env", projectID, appID), nil, testutil.AuthHeader(token))
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	vars := testutil.ReadJSONArray(t, resp)
	require.Len(t, vars, 2)

	// Check that non-secret values are decrypted and secrets never sent
	for _, v := range vars {
		ev := v.(map[string]any)
		key := ev["key"].(string)

		if key == "DATABASE_URL" {
			assert.Equal(t, "postgres://localhost/db", ev["value"])
			assert.Equal(t, false, ev["is_secret"])
		} else if key == "API_KEY" {
			assert.Nil(t, ev["value"], "a secret's value must never be sent by the list endpoint")
			assert.Equal(t, true, ev["is_secret"])
		}
	}
}

func TestUpdateEnvVars_InvalidKey(t *testing.T) {
	resetDB(t)
	token := env.SetupAdmin(t, "admin@test.com", "password123")
	project := env.CreateProject(t, token, "Test Project", "test-project")
	projectID := extractID(project["id"])
	app := env.CreateApplication(t, token, projectID, map[string]any{
		"name": "Env App", "type": "git", "build_type": "dockerfile", "source_repo": "https://github.com/test/repo",
	})
	appID := extractID(app["id"])

	// Invalid key: starts with a digit
	resp := env.DoRequest(t, "PUT", fmt.Sprintf("/api/projects/%s/applications/%s/env", projectID, appID), map[string]any{
		"vars": []map[string]any{
			{"key": "1INVALID", "value": "test", "is_secret": false},
		},
	}, testutil.AuthHeader(token))
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
	resp.Body.Close()

	// Invalid key: contains special chars
	resp = env.DoRequest(t, "PUT", fmt.Sprintf("/api/projects/%s/applications/%s/env", projectID, appID), map[string]any{
		"vars": []map[string]any{
			{"key": "MY-VAR", "value": "test", "is_secret": false},
		},
	}, testutil.AuthHeader(token))
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
	resp.Body.Close()

	// Valid key: normal env var names
	resp = env.DoRequest(t, "PUT", fmt.Sprintf("/api/projects/%s/applications/%s/env", projectID, appID), map[string]any{
		"vars": []map[string]any{
			{"key": "_VALID_KEY_123", "value": "test", "is_secret": false},
		},
	}, testutil.AuthHeader(token))
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	resp.Body.Close()
}
