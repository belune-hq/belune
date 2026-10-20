package handler_test

import (
	"context"
	"fmt"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/belune-hq/belune/internal/service"
	"github.com/belune-hq/belune/internal/testutil"
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
// {projectId}, so middleware.RequirePinnedProjectMatch never fires on them —
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

// TestAllRequestLogs_EmptyPinSetReadsNothing defends an invariant this change
// newly wrote down and four endpoints now depend on: a token that IS pinned but
// has no reachable projects left must match NOTHING, not everything.
//
// ⚠️ Reachable in production, not a contrived state. api_token_projects.project_id
// is ON DELETE CASCADE, so deleting the pinned project empties the set while the
// token stays pinned. GetAPITokenByHash says the rule outright — project_ids is
// '{}' for an unpinned token "exactly as often as for a pinned token with no
// reachable projects left ... the caller distinguishes those by pinned, never by
// array length" — and pinnedProjectUUIDs is shared, so collapsing the two would
// silently widen /api/deployments and /api/domains/tls along with these.
//
// Found by review as a surviving mutation: changing `ids == nil` to
// `len(ids) == 0` passed the entire handler suite.
func TestAllRequestLogs_EmptyPinSetReadsNothing(t *testing.T) {
	resetDB(t)
	adminToken := env.SetupAdmin(t, "admin@test.com", "password123")
	adminUserID := extractID(mustAuthMe(t, adminToken)["id"])

	doomed := env.CreateProject(t, adminToken, "Doomed", "doomed")
	doomedID := extractID(doomed["id"])

	other := env.CreateProject(t, adminToken, "Other", "other")
	appWithRequests(t, adminToken, extractID(other["id"]), "Other App", "/other/secret")

	pinned := createPinnedAPIToken(t, adminUserID, doomedID, service.AllScopes)

	// Delete the only project the token is pinned to. The pin row cascades, so
	// the token is now pinned to nothing while still being pinned.
	resp := env.DoRequest(t, "DELETE", "/api/projects/"+doomedID, nil, testutil.AuthHeader(adminToken))
	require.Equal(t, http.StatusOK, resp.StatusCode, "DELETE /api/projects/{id}")
	resp.Body.Close()

	assert.Empty(t, requestPaths(t, pinned),
		"a token pinned to nothing must read nothing — not everything")
	total, perMinute := requestTotal(t, pinned)
	assert.Equal(t, float64(0), total, "nor count it")
	assert.Equal(t, float64(0), perMinute, "nor watch it by the minute")
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

	// ⚠️ The unpinned control, without which an unconditional deny ships green.
	// Found by review: inverting the guard to `if true || ...` passed the whole
	// handler suite, because this was the only test touching the route. It is
	// also the one route here whose production behaviour is a flat refusal, so
	// "it refuses" on its own says nothing.
	unpinned := mintScoped(t, adminToken, service.AllScopes)
	open := env.DoRequest(t, "GET", "/api/requests/stream", nil, testutil.AuthHeader(unpinned))
	open.Body.Close()
	require.NotEqual(t, http.StatusForbidden, open.StatusCode,
		"an unpinned token must still reach the stream — otherwise the assertion below is vacuous")

	pinned := createPinnedAPIToken(t, adminUserID, extractID(project["id"]), service.AllScopes)
	resp := env.DoRequest(t, "GET", "/api/requests/stream", nil, testutil.AuthHeader(pinned))
	defer resp.Body.Close()
	require.Equal(t, http.StatusForbidden, resp.StatusCode,
		"a pinned token must not stream the cross-project firehose")
	// ⚠️ The MESSAGE, not just the code. The role and scope gates on this
	// route answer 403 too, so a mutation that broke one of those for a
	// pinned token would read as the pin deny working. A quota mutation
	// survived exactly this way earlier in v0.1.19.
	assert.Contains(t, fmt.Sprint(testutil.ReadJSON(t, resp)["error"]),
		"cannot stream requests across every project",
		"the 403 must come from the pin deny, not from the role or scope gate")
}

// TestAuditLogs_DeniedForPinnedToken covers the other install-wide surface a
// pin cannot be expressed against.
//
// audit_logs carries a polymorphic (resource_type, resource_id) pair and no
// project_id, so there is no predicate to narrow it with — and its rows name
// foreign projects, applications, hostnames, token names and env-var KEYS in
// `details`. Confirmed before the fix: a token pinned to one project read
// another project's audit rows, and the CSV export returned them too.
func TestAuditLogs_DeniedForPinnedToken(t *testing.T) {
	resetDB(t)
	adminToken := env.SetupAdmin(t, "admin@test.com", "password123")
	adminUserID := extractID(mustAuthMe(t, adminToken)["id"])
	project := env.CreateProject(t, adminToken, "Mine", "mine")

	// ⚠️ Unpinned control, or an unconditional deny would ship green — the
	// same gap review found in the request-stream test.
	unpinned := mintScoped(t, adminToken, service.AllScopes)
	pinned := createPinnedAPIToken(t, adminUserID, extractID(project["id"]), service.AllScopes)

	for _, path := range []string{"/api/audit-logs?limit=50", "/api/audit-logs/actions", "/api/audit-logs/export"} {
		open := env.DoRequest(t, "GET", path, nil, testutil.AuthHeader(unpinned))
		open.Body.Close()
		require.NotEqual(t, http.StatusForbidden, open.StatusCode,
			"%s: an unpinned token must still reach it, or the refusal below proves nothing", path)

		denied := env.DoRequest(t, "GET", path, nil, testutil.AuthHeader(pinned))
		require.Equal(t, http.StatusForbidden, denied.StatusCode,
			"%s: a pinned token must not read the install-wide audit log", path)
		// ⚠️ The MESSAGE, not just the code — these routes are also role- and
		// scope-gated, and both answer 403, so the bare code cannot tell the
		// pin deny from a mutation that broke one of those instead.
		assert.Contains(t, fmt.Sprint(testutil.ReadJSON(t, denied)["error"]),
			"the audit log is install-wide",
			"%s: the 403 must come from DenyPinnedToken, not the role or scope gate", path)
		denied.Body.Close()
	}
}
