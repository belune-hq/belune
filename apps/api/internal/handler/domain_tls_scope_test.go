package handler_test

import (
	"fmt"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/weiliang79/belune/internal/service"
	"github.com/weiliang79/belune/internal/testutil"
)

// addDomainFor creates an application with one domain and returns the hostname,
// so a test can assert on rows by a value it chose rather than by index.
func addDomainFor(t *testing.T, token, projectID, appName, hostname string) {
	t.Helper()
	app := env.CreateApplication(t, token, projectID, map[string]any{
		"name": appName, "type": "git", "build_type": "railpack",
		"source_repo": "https://github.com/test/repo", "branch": "main",
	})
	resp := env.DoRequest(t, "POST",
		fmt.Sprintf("/api/projects/%s/applications/%s/domains", projectID, extractID(app["id"])),
		map[string]any{"hostname": hostname, "ssl_enabled": true},
		testutil.AuthHeader(token))
	require.Equal(t, http.StatusCreated, resp.StatusCode)
	resp.Body.Close()
}

// tlsHostnames returns the hostname of every row GET /api/domains/tls returns
// for the given credential.
func tlsHostnames(t *testing.T, token string) []string {
	t.Helper()
	resp := env.DoRequest(t, "GET", "/api/domains/tls", nil, testutil.AuthHeader(token))
	require.Equal(t, http.StatusOK, resp.StatusCode, "GET /api/domains/tls")
	defer resp.Body.Close()

	out := []string{}
	for _, row := range testutil.ReadJSONArray(t, resp) {
		out = append(out, fmt.Sprint(row.(map[string]any)["hostname"]))
	}
	return out
}

// TestDomainTLSStatus_ScopedByRole is the behavioural half of moving
// GET /api/domains/tls out of the admin group. It was admin-only, which left a
// member unable to discover which certificate their OWN domain serves:
// ListDomainsByApplication is SELECT * FROM domains, so it hands back a bare
// certificate_id and nothing resolves it to a name. This query already joins
// certificates — it just never filtered by project, so it could not be shown to
// anyone but an admin.
//
// Asserts the three audiences separately, because the failure modes differ: an
// admin under-reporting is a regression, a member over-reporting is a leak.
func TestDomainTLSStatus_ScopedByRole(t *testing.T) {
	resetDB(t)
	adminToken := env.SetupAdmin(t, "admin@test.com", "password123")
	_, ownerToken := createMember(t, adminToken, "owner@test.com")
	_, otherToken := createMember(t, adminToken, "other@test.com")

	ownerProject := env.CreateProject(t, ownerToken, "Owner Project", "owner-project")
	ownerProjectID := extractID(ownerProject["id"])
	addDomainFor(t, ownerToken, ownerProjectID, "Owned", "owned.example.com")

	otherProject := env.CreateProject(t, otherToken, "Other Project", "other-project")
	addDomainFor(t, otherToken, extractID(otherProject["id"]), "Foreign", "foreign.example.com")

	// Admin: every domain on the install, both projects.
	assert.ElementsMatch(t, []string{"owned.example.com", "foreign.example.com"},
		tlsHostnames(t, adminToken), "an admin sees every domain")

	// Member: their own only. The other member's domain is the leak this guards.
	assert.Equal(t, []string{"owned.example.com"}, tlsHostnames(t, ownerToken),
		"a member sees only their own project's domains")
	assert.NotContains(t, tlsHostnames(t, ownerToken), "foreign.example.com",
		"a member must never see another member's domain")

	// Sharing widens it the same way it widens every other member-scoped list —
	// OR p.shared in the query, not a special case here.
	resp := env.DoRequest(t, "PUT", fmt.Sprintf("/api/projects/%s/sharing", ownerProjectID),
		map[string]any{"shared": true}, testutil.AuthHeader(adminToken))
	require.Equal(t, http.StatusOK, resp.StatusCode)
	resp.Body.Close()

	assert.Contains(t, tlsHostnames(t, otherToken), "owned.example.com",
		"a shared project's domains are visible to every member")
}

// TestDomainTLSStatus_RespectsTokenPin covers the hole this route's placement
// opens. It lives in the RequirePinnedProjectMatch group, but that middleware only
// ever compares a {projectId} URL param and this route has none — so the pin is
// enforced in the handler, and nothing structural would catch it regressing.
// Without it a project-pinned token reads every domain its owner can reach,
// which is exactly what the pin exists to prevent.
func TestDomainTLSStatus_RespectsTokenPin(t *testing.T) {
	resetDB(t)
	adminToken := env.SetupAdmin(t, "admin@test.com", "password123")
	ownerID, ownerToken := createMember(t, adminToken, "owner@test.com")

	first := env.CreateProject(t, ownerToken, "First", "first")
	firstID := extractID(first["id"])
	addDomainFor(t, ownerToken, firstID, "First App", "first.example.com")

	second := env.CreateProject(t, ownerToken, "Second", "second")
	addDomainFor(t, ownerToken, extractID(second["id"]), "Second App", "second.example.com")

	// Unpinned, the same owner sees both — so the narrowing below is the pin
	// doing the work, not an access check that would have blocked it anyway.
	unpinned := mintScoped(t, ownerToken, service.AllScopes)
	assert.ElementsMatch(t, []string{"first.example.com", "second.example.com"},
		tlsHostnames(t, unpinned), "an unpinned token sees every project it can reach")

	pinned := createPinnedAPIToken(t, ownerID, firstID, service.AllScopes)
	assert.Equal(t, []string{"first.example.com"}, tlsHostnames(t, pinned),
		"a pinned token is narrowed to its project")
}
