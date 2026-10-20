package handler_test

import (
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/belune-hq/belune/internal/testutil"
)

// TestUnmatchedAPIPath_Returns404JSON covers the catch-all's blind spot.
//
// The SPA handler answers any path that does not resolve to a built asset with
// index.html, which is what makes client-side routing work — and it also
// swallowed every unmatched /api/ path, so a caller of a typo'd or RENAMED
// endpoint got 200 text/html. A success status and an HTML page is worse than a
// 404: most clients fail to parse it in some confusing way rather than
// reporting "not found", and it made the reference unfalsifiable — the spec
// said which paths existed, and probing a wrong one did not disagree.
//
// Found while verifying the /api/metrics -> /api/summary move, where the old
// path kept answering 200 after it had stopped being routed.
func TestUnmatchedAPIPath_Returns404JSON(t *testing.T) {
	resetDB(t)
	adminToken := env.SetupAdmin(t, "admin@test.com", "password123")

	for _, path := range []string{
		"/api/definitely-not-a-route",
		"/api/metrics",         // the real rename, still the motivating case
		"/api/proxy/reconcile", // moved under /api/maintenance
		"/api",                 // bare, no trailing slash
	} {
		resp := env.DoRequest(t, "GET", path, nil, testutil.AuthHeader(adminToken))
		assert.Equal(t, http.StatusNotFound, resp.StatusCode, "%s must 404", path)
		assert.Equal(t, "application/json", resp.Header.Get("Content-Type"),
			"%s must answer as the API, not as a page", path)

		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		require.NoError(t, err)
		assert.Contains(t, string(body), `"error"`,
			"%s must use the same {\"error\": ...} shape as every other API failure", path)
		assert.NotContains(t, strings.ToLower(string(body)), "<!doctype",
			"%s must not return the SPA", path)
	}
}

// TestUnmatchedAppPath_StillServesTheSPA is the other half, and the reason the
// fix wraps the catch-all instead of replacing it. Client-side routing depends
// on an unknown path returning index.html: /projects/{id} is a real page the
// server knows nothing about, and a 404 there would break every deep link and
// every refresh.
//
// ⚠️ Asserting this is the point. A fix that returned 404 for everything
// unmatched would pass the test above and break the entire dashboard.
func TestUnmatchedAppPath_StillServesTheSPA(t *testing.T) {
	resetDB(t)

	// No auth: the SPA is served to anyone, and handles its own redirect to
	// /login once it boots.
	for _, path := range []string{"/projects", "/projects/some-uuid/applications", "/nonexistent-page"} {
		resp := env.DoRequest(t, "GET", path, nil, nil)
		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		require.NoError(t, err)

		// In a test binary the SPA may not be built (web/dist holds only
		// .gitkeep), in which case web.Handler() is nil and the catch-all
		// answers app paths with a plain 404 rather than index.html. Either
		// outcome is fine here. What must never happen is this path being
		// caught by the API branch and answered with the API's JSON error
		// shape.
		assert.NotContains(t, string(body), `"error":"no such endpoint"`,
			"%s is an app route — it must never get the API's 404", path)
	}
}
