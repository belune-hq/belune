package handler_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/belune-hq/belune/internal/service"
	"github.com/belune-hq/belune/internal/testutil"
)

// TestRevealRoutes_RequireSession is the structural half of "read scope
// cannot exfiltrate secrets": a reveal endpoint decrypts a real secret
// (webhook secret, deploy-hook token, file-mount contents, env var value)
// and returns it in plaintext, but "read" scope is derived purely from the
// HTTP method (RequireScopeByMethod) — it says nothing about what a GET
// actually returns. Without this, the read-only PAT the create-token dialog
// sells as the safe option can decrypt every secret in the install. Sibling
// of TestDestroyRoutes_RequireSession: walk the REAL registered router (not
// a hand-maintained list) and assert every route ending in "/reveal"
// rejects a PAT specifically via middleware.RequireSession, checked by its
// exact error message so this fails loudly if some other check happened to
// reject the request for an unrelated reason.
func TestRevealRoutes_RequireSession(t *testing.T) {
	resetDB(t)
	adminToken := env.SetupAdmin(t, "admin@test.com", "password123")
	plain := mintScoped(t, adminToken, service.AllScopes)

	router, ok := env.Server.Config.Handler.(chi.Routes)
	require.True(t, ok, "test server handler must be walkable as chi.Routes")

	tested := 0
	err := chi.Walk(router, func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		if method != http.MethodGet || !strings.HasSuffix(route, "/reveal") {
			return nil
		}
		tested++

		path := route
		for _, seg := range strings.Split(route, "/") {
			if strings.HasPrefix(seg, "{") {
				path = strings.Replace(path, seg, "00000000-0000-0000-0000-000000000000", 1)
			}
		}

		resp := env.DoRequest(t, method, path, nil, testutil.AuthHeader(plain))
		body := testutil.ReadJSON(t, resp)
		assert.Equal(t, http.StatusForbidden, resp.StatusCode,
			"%s %s must reject a PAT", method, route)
		assert.Equal(t, "this action requires a session, not a personal access token", body["error"],
			"%s %s must be rejected by RequireSession specifically, not some other check", method, route)
		return nil
	})
	require.NoError(t, err)

	// Sanity floor: the six known reveal routes (webhook secret, deploy
	// hook, file mount, project env var, application env var, database
	// credentials). If this drops, a route's path changed shape or one was
	// removed — worth knowing either way, not silently passing on zero
	// routes checked. Bump this (and the count above) any time a new
	// reveal-shaped route is added, the same way destroy_boundary_test.go's
	// twelve are enumerated explicitly rather than left to drift.
	assert.GreaterOrEqual(t, tested, 6, "the walk should discover at least the six known reveal routes")
}
