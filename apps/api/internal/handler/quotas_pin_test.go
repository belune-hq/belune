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

// setQuota caps a scope, as the operator's session would.
func setQuota(t *testing.T, token, scope, scopeID string, maxApps int) {
	t.Helper()
	resp := env.DoRequest(t, "PUT", fmt.Sprintf("/api/quotas/%s/%s", scope, scopeID),
		map[string]any{"max_applications": maxApps}, testutil.AuthHeader(token))
	require.Equal(t, http.StatusOK, resp.StatusCode, "PUT /api/quotas/%s/%s", scope, scopeID)
	resp.Body.Close()
}

// quotaScopeIDs returns the scope_id of every row GET /api/quotas returns.
func quotaScopeIDs(t *testing.T, token string) []string {
	t.Helper()
	resp := env.DoRequest(t, "GET", "/api/quotas", nil, testutil.AuthHeader(token))
	require.Equal(t, http.StatusOK, resp.StatusCode, "GET /api/quotas")
	defer resp.Body.Close()
	out := []string{}
	for _, row := range testutil.ReadJSONArray(t, resp) {
		out = append(out, fmt.Sprint(row.(map[string]any)["scope_id"]))
	}
	return out
}

// TestQuotas_RespectPinOnReads covers the disclosure half of a hole that also
// allowed a cross-project WRITE. These routes are admin-gated, but the project
// arrives as {scopeId}, not {projectId}, so middleware.RequirePinnedProjectMatch
// never fires on them and nothing else narrowed them.
func TestQuotas_RespectPinOnReads(t *testing.T) {
	resetDB(t)
	adminToken := env.SetupAdmin(t, "admin@test.com", "password123")
	adminUserID := extractID(mustAuthMe(t, adminToken)["id"])

	mine := env.CreateProject(t, adminToken, "Mine", "mine")
	mineID := extractID(mine["id"])
	foreign := env.CreateProject(t, adminToken, "Foreign", "foreign")
	foreignID := extractID(foreign["id"])

	setQuota(t, adminToken, "project", mineID, 5)
	setQuota(t, adminToken, "project", foreignID, 5)
	setQuota(t, adminToken, "user", adminUserID, 9)

	// ⚠️ Unpinned control first, so the narrowing below is attributable to the
	// pin rather than to an access check that would have refused anyone.
	unpinned := mintScoped(t, adminToken, service.AllScopes)
	assert.ElementsMatch(t, []string{mineID, foreignID, adminUserID}, quotaScopeIDs(t, unpinned),
		"an unpinned token sees every quota, including the user-scoped one")

	pinned := createPinnedAPIToken(t, adminUserID, mineID, service.AllScopes)

	assert.Equal(t, []string{mineID}, quotaScopeIDs(t, pinned),
		"a pinned token sees only its own project's quota")

	resp := env.DoRequest(t, "GET", "/api/quotas/project/"+foreignID, nil, testutil.AuthHeader(pinned))
	resp.Body.Close()
	assert.Equal(t, http.StatusForbidden, resp.StatusCode,
		"naming another project's quota must be refused, not narrowed away")

	own := env.DoRequest(t, "GET", "/api/quotas/project/"+mineID, nil, testutil.AuthHeader(pinned))
	own.Body.Close()
	assert.Equal(t, http.StatusOK, own.StatusCode,
		"its own project's quota is still readable — otherwise the refusal above proves nothing")
}

// TestQuotas_PinnedTokenCannotAddressUserScope covers the decision that a pin,
// being expressed in projects, has nothing to say about a user-scoped quota —
// and that a user quota governs every project that user owns, so honouring one
// for a pinned token would reach past the pin by another route.
func TestQuotas_PinnedTokenCannotAddressUserScope(t *testing.T) {
	resetDB(t)
	adminToken := env.SetupAdmin(t, "admin@test.com", "password123")
	adminUserID := extractID(mustAuthMe(t, adminToken)["id"])
	mine := env.CreateProject(t, adminToken, "Mine", "mine")
	setQuota(t, adminToken, "user", adminUserID, 9)

	pinned := createPinnedAPIToken(t, adminUserID, extractID(mine["id"]), service.AllScopes)

	// Its OWN owner's user quota.
	//
	// ⚠️ Asserted on the MESSAGE, not just the status, and that is the whole
	// point of this test. Deleting the user-scope refusal still yields 403,
	// because the code then falls through to PinAllows against a USER id and a
	// pin set only ever holds PROJECT ids (api_token_projects.project_id has an
	// FK to projects, so the two namespaces cannot overlap). Status alone
	// cannot tell "refused for being user-scoped" from "refused for not being
	// in the pin", so the decision would be undefended — verified: removing the
	// branch passed every test here until this assertion existed.
	resp := env.DoRequest(t, "GET", "/api/quotas/user/"+adminUserID, nil, testutil.AuthHeader(pinned))
	defer resp.Body.Close()
	require.Equal(t, http.StatusForbidden, resp.StatusCode,
		"a pinned token cannot address a user-scoped quota, not even its own owner's")
	assert.Contains(t, fmt.Sprint(testutil.ReadJSON(t, resp)["error"]), "user-scoped",
		"refused for being user-scoped, not incidentally for missing the pin")

	assert.NotContains(t, quotaScopeIDs(t, pinned), adminUserID,
		"nor see it in the list")
}

// TestQuotas_WritesRequireASession is the breaking half of this change: a quota
// decides whether another project can deploy at all, so capping one is an
// operator decision in the same family as issuing credentials or opening the
// host shell. It TIGHTENS what a PAT may reach.
func TestQuotas_WritesRequireASession(t *testing.T) {
	resetDB(t)
	adminToken := env.SetupAdmin(t, "admin@test.com", "password123")
	adminUserID := extractID(mustAuthMe(t, adminToken)["id"])
	mine := env.CreateProject(t, adminToken, "Mine", "mine")
	mineID := extractID(mine["id"])

	// ⚠️ An UNPINNED, fully-scoped admin token — the most capable token that
	// exists. If even this cannot write, the refusal is about being a token,
	// which is the decision, rather than about the pin.
	unpinned := mintScoped(t, adminToken, service.AllScopes)

	read := env.DoRequest(t, "GET", "/api/quotas", nil, testutil.AuthHeader(unpinned))
	read.Body.Close()
	assert.Equal(t, http.StatusOK, read.StatusCode,
		"reads stay token-reachable — checking your own limits from a script is legitimate")

	for _, tc := range []struct{ method, body string }{{"PUT", "put"}, {"DELETE", "delete"}} {
		var body any
		if tc.method == "PUT" {
			body = map[string]any{"max_applications": 1}
		}
		resp := env.DoRequest(t, tc.method, "/api/quotas/project/"+mineID, body, testutil.AuthHeader(unpinned))
		resp.Body.Close()
		assert.Equal(t, http.StatusForbidden, resp.StatusCode,
			"%s /api/quotas must refuse a token, however scoped", tc.method)
	}

	// The session still can, or the endpoint is simply broken.
	setQuota(t, adminToken, "project", mineID, 3)
	_ = adminUserID
}
