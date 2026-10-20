package handler_test

import (
	"fmt"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/belune-hq/belune/internal/testutil"
)

func TestSetResources(t *testing.T) {
	resetDB(t)
	token := env.SetupAdmin(t, "admin@test.com", "password123")
	project := env.CreateProject(t, token, "Test Project", "test-project")
	projectID := extractID(project["id"])
	app := env.CreateApplication(t, token, projectID, map[string]any{
		"name": "Res App", "type": "image", "build_type": "image",
		"source_image": "nginx:1.25",
	})
	appID := extractID(app["id"])
	url := fmt.Sprintf("/api/projects/%s/applications/%s/resources", projectID, appID)

	resp := env.DoRequest(t, "PUT", url, map[string]any{
		"cpu_limit": 0.5, "memory_limit": 268435456,
	}, testutil.AuthHeader(token))
	require.Equal(t, http.StatusOK, resp.StatusCode)
	result := testutil.ReadJSON(t, resp)
	assert.Equal(t, 0.5, result["cpu_limit"])
	assert.Equal(t, float64(268435456), result["memory_limit"])

	// Negative limits are rejected.
	resp = env.DoRequest(t, "PUT", url, map[string]any{
		"cpu_limit": -1, "memory_limit": 0,
	}, testutil.AuthHeader(token))
	defer resp.Body.Close()
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
}
