package handler_test

import (
	"context"
	"fmt"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/weiliang79/belune/internal/service"
	"github.com/weiliang79/belune/internal/testutil"
)

// seedRequestLog inserts one row straight into request_logs. The production
// writer is the Caddy access-log tailer, which a handler test has no way to
// drive — and the path under test is the read side.
func seedRequestLog(t *testing.T, applicationID, path string, status int) {
	t.Helper()
	_, err := env.Pool.Exec(context.Background(), `
		INSERT INTO request_logs (application_id, method, path, status_code, latency_ms, hostname, client_ip)
		VALUES ($1, 'GET', $2, $3, 12, 'app.example.com', '198.51.100.7')`,
		applicationID, path, status)
	require.NoError(t, err)
}

// appWithRequests creates an application in the project and gives it one
// request log, returning the path so a test can assert on a value it chose.
func appWithRequests(t *testing.T, token, projectID, name, path string) string {
	t.Helper()
	app := env.CreateApplication(t, token, projectID, map[string]any{
		"name": name, "type": "image", "build_type": "image",
		"source_image": "nginx:1.27",
	})
	seedRequestLog(t, extractID(app["id"]), path, 200)
	return path
}

// requestPaths returns the path of every row GET /api/requests returns for the
// given credential.
func requestPaths(t *testing.T, token string) []string {
	t.Helper()
	resp := env.DoRequest(t, "GET", "/api/requests", nil, testutil.AuthHeader(token))
	require.Equal(t, http.StatusOK, resp.StatusCode, "GET /api/requests")
	defer resp.Body.Close()

	out := []string{}
	for _, row := range testutil.ReadJSONArray(t, resp) {
		out = append(out, fmt.Sprint(row.(map[string]any)["path"]))
	}
	return out
}

// requestTotal returns the row count GET /api/requests/summary reports, and
// the total across its per-minute series.
//
// ⚠️ Both, because they come from DIFFERENT queries — GetRequestSummary and
// GetRequestPerMinute — each carrying its own copy of the pin predicate.
// Asserting only the first leaves a mutation to the second alive.
func requestTotal(t *testing.T, token string) (total float64, perMinute float64) {
	t.Helper()
	resp := env.DoRequest(t, "GET", "/api/requests/summary", nil, testutil.AuthHeader(token))
	require.Equal(t, http.StatusOK, resp.StatusCode, "GET /api/requests/summary")
	defer resp.Body.Close()

	body := testutil.ReadJSON(t, resp)
	for _, point := range body["per_minute"].([]any) {
		perMinute += point.(map[string]any)["count"].(float64)
	}
	return body["total"].(float64), perMinute
}

// TestAllRequestLogs_RespectsTokenPin is the REST half of a hole the WebSocket
// closed in v0.1.12 and REST did not.
//
// /api/requests and /api/requests/summary are admin-gated but have no
// {projectId}, so middleware.RequireProjectAccess never fires on them — and
// canAccessApplication is no help either, since it returns true for any admin
// and deliberately ignores the pin ("a separate axis, composed by each
// caller"). Nothing structural narrowed these, so a project-pinned admin token
// read every project's traffic.
func TestAllRequestLogs_RespectsTokenPin(t *testing.T) {
	resetDB(t)
	adminToken := env.SetupAdmin(t, "admin@test.com", "password123")
	adminUserID := extractID(mustAuthMe(t, adminToken)["id"])

	first := env.CreateProject(t, adminToken, "First", "first")
	firstID := extractID(first["id"])
	appWithRequests(t, adminToken, firstID, "First App", "/first//healthz")

	second := env.CreateProject(t, adminToken, "Second", "second")
	appWithRequests(t, adminToken, extractID(second["id"]), "Second App", "/second/secret")

	// ⚠️ Unpinned first, so the narrowing below is demonstrably the PIN doing
	// the work rather than an access check that would have blocked it anyway.
	// Without this the pinned assertion passes against a handler that denies
	// everyone.
	unpinned := mintScoped(t, adminToken, service.AllScopes)
	assert.ElementsMatch(t, []string{"/first//healthz", "/second/secret"},
		requestPaths(t, unpinned), "an unpinned admin token sees every project's requests")
	unpinnedTotal, unpinnedPerMinute := requestTotal(t, unpinned)
	assert.Equal(t, float64(2), unpinnedTotal)
	assert.Equal(t, float64(2), unpinnedPerMinute)

	pinned := createPinnedAPIToken(t, adminUserID, firstID, service.AllScopes)

	assert.Equal(t, []string{"/first//healthz"}, requestPaths(t, pinned),
		"a pinned token is narrowed to its project")
	assert.NotContains(t, requestPaths(t, pinned), "/second/secret",
		"a pinned token must never read another project's request paths")

	// The aggregates need the pin as much as the list does: counting another
	// project's traffic, timing it, or watching it by the minute is the same
	// disclosure in a smaller shape.
	pinnedTotal, pinnedPerMinute := requestTotal(t, pinned)
	assert.Equal(t, float64(1), pinnedTotal,
		"a pinned token must not count requests it cannot read")
	assert.Equal(t, float64(1), pinnedPerMinute,
		"nor watch them by the minute — the series is its own query, with its own copy of the pin")
}

// TestAllRequestLogs_PinSurvivesAnExplicitApplicationFilter covers the way a
// caller would try to reach past the pin on purpose. The two predicates AND
// together in SQL, so an application outside the pin yields nothing rather
// than 403 — deliberately, and noted here so the empty body is read as the
// pin holding rather than as the filter being ignored.
func TestAllRequestLogs_PinSurvivesAnExplicitApplicationFilter(t *testing.T) {
	resetDB(t)
	adminToken := env.SetupAdmin(t, "admin@test.com", "password123")
	adminUserID := extractID(mustAuthMe(t, adminToken)["id"])

	first := env.CreateProject(t, adminToken, "First", "first")
	firstID := extractID(first["id"])

	second := env.CreateProject(t, adminToken, "Second", "second")
	foreign := env.CreateApplication(t, adminToken, extractID(second["id"]), map[string]any{
		"name": "Foreign", "type": "image", "build_type": "image",
		"source_image": "nginx:1.27",
	})
	foreignID := extractID(foreign["id"])
	seedRequestLog(t, foreignID, "/second/secret", 200)

	pinned := createPinnedAPIToken(t, adminUserID, firstID, service.AllScopes)
	resp := env.DoRequest(t, "GET",
		"/api/requests?application_id="+foreignID, nil, testutil.AuthHeader(pinned))
	require.Equal(t, http.StatusOK, resp.StatusCode)
	defer resp.Body.Close()

	assert.Empty(t, testutil.ReadJSONArray(t, resp),
		"naming an application outside the pin must not reach past it")
}

// TestStreamAllRequestLogs_DeniedForPinnedToken mirrors wsAdminChannelAllowed,
// which has refused a pinned token the WebSocket twin of this firehose since
// v0.1.12. Until this change REST streamed a pinned token the very data the
// socket refused it.
func TestStreamAllRequestLogs_DeniedForPinnedToken(t *testing.T) {
	resetDB(t)
	adminToken := env.SetupAdmin(t, "admin@test.com", "password123")
	adminUserID := extractID(mustAuthMe(t, adminToken)["id"])
	project := env.CreateProject(t, adminToken, "First", "first")

	pinned := createPinnedAPIToken(t, adminUserID, extractID(project["id"]), service.AllScopes)
	resp := env.DoRequest(t, "GET", "/api/requests/stream", nil, testutil.AuthHeader(pinned))
	defer resp.Body.Close()
	assert.Equal(t, http.StatusForbidden, resp.StatusCode,
		"a pinned token must not stream the cross-project firehose")
}
