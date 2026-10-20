package handler_test

import (
	"fmt"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/belune-hq/belune/internal/testutil"
)

// TestGetDomain_ReturnsOneAndHonoursAccess covers the endpoint added for the
// polling case a list serves badly: tls_status moves on its own, so a script
// that adds a domain and waits has to re-read that one row rather than fetching
// every domain on the application each time.
//
// The access assertion is the half worth having. canAccessDomain resolves the
// DOMAIN's own owner rather than trusting the {applicationId} in the path, so a
// member cannot read another member's domain by pointing the path at their own
// application — which is exactly the mistake a nested read invites.
func TestGetDomain_ReturnsOneAndHonoursAccess(t *testing.T) {
	resetDB(t)
	adminToken := env.SetupAdmin(t, "admin@test.com", "password123")
	_, ownerToken := createMember(t, adminToken, "owner@test.com")
	_, otherToken := createMember(t, adminToken, "other@test.com")

	project := env.CreateProject(t, ownerToken, "Owner Project", "owner-project")
	projectID := extractID(project["id"])
	app := env.CreateApplication(t, ownerToken, projectID, map[string]any{
		"name": "Shop", "type": "git", "build_type": "railpack",
		"source_repo": "https://github.com/test/repo", "branch": "main",
	})
	appID := extractID(app["id"])

	resp := env.DoRequest(t, "POST",
		fmt.Sprintf("/api/projects/%s/applications/%s/domains", projectID, appID),
		map[string]any{"hostname": "shop.example.com", "ssl_enabled": true},
		testutil.AuthHeader(ownerToken))
	require.Equal(t, http.StatusCreated, resp.StatusCode)
	domainID := extractID(testutil.ReadJSON(t, resp)["id"])

	path := fmt.Sprintf("/api/projects/%s/applications/%s/domains/%s", projectID, appID, domainID)

	// The owner reads it back, in the shape the list returns — route_features
	// included, so a caller does not get a subtly different row from the one
	// they already know.
	resp = env.DoRequest(t, "GET", path, nil, testutil.AuthHeader(ownerToken))
	require.Equal(t, http.StatusOK, resp.StatusCode)
	body := testutil.ReadJSON(t, resp)
	assert.Equal(t, "shop.example.com", body["hostname"])
	assert.Contains(t, body, "tls_status", "the field this endpoint exists to poll")
	assert.Contains(t, body, "route_features", "must match the list's shape")

	// Another member cannot read it, even though the path is well-formed.
	resp = env.DoRequest(t, "GET", path, nil, testutil.AuthHeader(otherToken))
	assert.Equal(t, http.StatusForbidden, resp.StatusCode,
		"a member must not read another member's domain")
	resp.Body.Close()

	// ⚠️ The path-swap attempt: another member points the {applicationId} at an
	// application they DO own while naming a domain they do not. Access is
	// resolved from the domain, so this must still be refused — if it ever
	// returns 200, canAccessDomain has been swapped for a check on the path.
	otherProject := env.CreateProject(t, otherToken, "Other Project", "other-project")
	otherApp := env.CreateApplication(t, otherToken, extractID(otherProject["id"]), map[string]any{
		"name": "Theirs", "type": "git", "build_type": "railpack",
		"source_repo": "https://github.com/test/repo", "branch": "main",
	})
	resp = env.DoRequest(t, "GET", fmt.Sprintf("/api/projects/%s/applications/%s/domains/%s",
		extractID(otherProject["id"]), extractID(otherApp["id"]), domainID),
		nil, testutil.AuthHeader(otherToken))
	assert.Equal(t, http.StatusForbidden, resp.StatusCode,
		"access must resolve from the domain, never from the {applicationId} in the path")
	resp.Body.Close()

	// An admin reads any domain.
	resp = env.DoRequest(t, "GET", path, nil, testutil.AuthHeader(adminToken))
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	resp.Body.Close()
}

// TestGetPreview_RejectsANonPreview guards the check that makes this route mean
// what its path says. A preview IS an application row, so without the parent
// check this path would happily return any application the caller can reach —
// turning /previews/{id} into a second, undocumented GetApplication.
func TestGetPreview_RejectsANonPreview(t *testing.T) {
	resetDB(t)
	adminToken := env.SetupAdmin(t, "admin@test.com", "password123")

	project := env.CreateProject(t, adminToken, "Preview Project", "preview-project")
	projectID := extractID(project["id"])
	app := env.CreateApplication(t, adminToken, projectID, map[string]any{
		"name": "Parent", "type": "git", "build_type": "railpack",
		"source_repo": "https://github.com/test/repo", "branch": "main",
	})
	appID := extractID(app["id"])

	// The parent application is a real, readable application — but it is not a
	// preview, so this route must refuse it rather than serve it.
	resp := env.DoRequest(t, "GET",
		fmt.Sprintf("/api/projects/%s/applications/%s/previews/%s", projectID, appID, appID),
		nil, testutil.AuthHeader(adminToken))
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode,
		"a non-preview application must not be readable through the previews path")
	resp.Body.Close()

	// An id that is no application at all is a 404, not a 500. Reached as an
	// ADMIN on purpose: canAccessApplication passes admins unconditionally, so
	// this is the one caller for whom an unknown id gets past the access check
	// and exercises the not-found path rather than being masked by a 403.
	resp = env.DoRequest(t, "GET",
		fmt.Sprintf("/api/projects/%s/applications/%s/previews/00000000-0000-0000-0000-000000000000", projectID, appID),
		nil, testutil.AuthHeader(adminToken))
	assert.Equal(t, http.StatusNotFound, resp.StatusCode,
		"an unknown id must 404 rather than 500")
	resp.Body.Close()
}
