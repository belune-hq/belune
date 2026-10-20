package handler_test

import (
	"context"
	"fmt"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/belune-hq/belune/internal/testutil"
)

func TestCreateApplication_RejectsIncoherentSource(t *testing.T) {
	resetDB(t)
	token := env.SetupAdmin(t, "admin@test.com", "password123")
	project := env.CreateProject(t, token, "Test Project", "test-project")
	projectID := extractID(project["id"])

	cases := map[string]map[string]any{
		"image app with no image": {
			"name": "A", "type": "image", "build_type": "image",
		},
		"image app carrying a repo": {
			"name": "B", "type": "image", "build_type": "image",
			"source_image": "nginx:1.25", "source_repo": "https://github.com/a/b",
		},
		"git app with no repo": {
			"name": "C", "type": "git", "build_type": "dockerfile",
		},
		"git app built as image": {
			"name": "D", "type": "git", "build_type": "image",
			"source_repo": "https://github.com/a/b",
		},
		// Previously reached the database and came back as a 500.
		"unknown type": {
			"name": "E", "type": "compose", "build_type": "image",
			"source_image": "nginx:1.25",
		},
	}

	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			resp := env.DoRequest(t, "POST",
				fmt.Sprintf("/api/projects/%s/applications", projectID),
				body, testutil.AuthHeader(token))
			defer resp.Body.Close()
			assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
		})
	}

	// Nothing was stored by any of the rejected requests.
	var count int
	require.NoError(t, env.Pool.QueryRow(context.Background(),
		`SELECT count(*) FROM applications WHERE project_id = $1`, projectID).Scan(&count))
	assert.Equal(t, 0, count)
}

// Clearing the image on an image application used to be accepted, stored as
// NULL, and only surface much later as a failed pull. The clear is requested
// explicitly: this test originally sent only a name and relied on an omitted
// key reading as "clear", which was the partial-PUT bug itself.
func TestUpdateApplication_RejectsClearingSourceImage(t *testing.T) {
	resetDB(t)
	token := env.SetupAdmin(t, "admin@test.com", "password123")
	project := env.CreateProject(t, token, "Test Project", "test-project")
	projectID := extractID(project["id"])
	app := env.CreateApplication(t, token, projectID, map[string]any{
		"name": "Image App", "type": "image", "build_type": "image",
		"source_image": "nginx:1.25",
	})
	appID := extractID(app["id"])

	resp := env.DoRequest(t, "PUT",
		fmt.Sprintf("/api/projects/%s/applications/%s", projectID, appID),
		map[string]any{"name": "Image App", "source_image": ""}, testutil.AuthHeader(token))
	defer resp.Body.Close()
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)

	// The stored image is untouched, so the next deploy still works.
	var image string
	require.NoError(t, env.Pool.QueryRow(context.Background(),
		`SELECT source_image FROM applications WHERE id = $1`, appID).Scan(&image))
	assert.Equal(t, "nginx:1.25", image)
}

// Setting a repo on an image application was accepted and then ignored: a push
// webhook could match the app by repository URL and "succeed" by re-pulling the
// same image, never touching the repo.
func TestUpdateApplication_RejectsRepoOnImageApp(t *testing.T) {
	resetDB(t)
	token := env.SetupAdmin(t, "admin@test.com", "password123")
	project := env.CreateProject(t, token, "Test Project", "test-project")
	projectID := extractID(project["id"])
	app := env.CreateApplication(t, token, projectID, map[string]any{
		"name": "Image App", "type": "image", "build_type": "image",
		"source_image": "nginx:1.25",
	})
	appID := extractID(app["id"])

	resp := env.DoRequest(t, "PUT",
		fmt.Sprintf("/api/projects/%s/applications/%s", projectID, appID),
		map[string]any{
			"name": "Image App", "source_image": "nginx:1.25",
			"source_repo": "https://github.com/a/b",
		}, testutil.AuthHeader(token))
	defer resp.Body.Close()
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)

	var repo *string
	require.NoError(t, env.Pool.QueryRow(context.Background(),
		`SELECT source_repo FROM applications WHERE id = $1`, appID).Scan(&repo))
	assert.Nil(t, repo, "no repo should have been stored on an image application")
}

// A coherent update still works — the guard must not be so strict that normal
// edits fail.
func TestUpdateApplication_AcceptsCoherentSource(t *testing.T) {
	resetDB(t)
	token := env.SetupAdmin(t, "admin@test.com", "password123")
	project := env.CreateProject(t, token, "Test Project", "test-project")
	projectID := extractID(project["id"])
	app := env.CreateApplication(t, token, projectID, map[string]any{
		"name": "Image App", "type": "image", "build_type": "image",
		"source_image": "nginx:1.25",
	})
	appID := extractID(app["id"])

	resp := env.DoRequest(t, "PUT",
		fmt.Sprintf("/api/projects/%s/applications/%s", projectID, appID),
		map[string]any{"name": "Renamed", "source_image": "nginx:1.27"},
		testutil.AuthHeader(token))
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	var image string
	require.NoError(t, env.Pool.QueryRow(context.Background(),
		`SELECT source_image FROM applications WHERE id = $1`, appID).Scan(&image))
	assert.Equal(t, "nginx:1.27", image)
}
