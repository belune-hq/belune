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

// isGrantRoute identifies the two routes that hand out project-owner-
// equivalent access rather than operate a workload: /transfer moves
// ownership to a named user, /sharing extends it to every Member on the
// install (canAccessOwned's `if shared { return true }` has no membership
// check — see the comment on that route pair in routes.go).
//
// UNLIKE destroy_boundary_test.go and reveal_boundary_test.go, this has no
// uniform SHAPE to discover by: every destroy/restore route ends in
// "/{id}" or "/{id}/restore", every reveal route ends in "/reveal" — a
// grant route is just these two specific path suffixes, named here because
// nothing else identifies them structurally. That makes this predicate
// thinner than its siblings: it pins the two routes fixed today, but a
// FUTURE grant-shaped route under a different name (an "invite a co-owner"
// endpoint, say) would NOT be caught by this walk and would need its own
// line added here. Prefer a real structural predicate if one turns up;
// none was found for this class.
func isGrantRoute(method, route string) bool {
	if method != http.MethodPut {
		return false
	}
	return strings.HasSuffix(route, "/transfer") || strings.HasSuffix(route, "/sharing")
}

// TestGrantRoutes_RequireSession is the structural half of "a token cannot
// change who can reach a project": walk the REAL registered router (not a
// hand-maintained list of route strings, beyond isGrantRoute's own two
// suffixes — see its caveat) and assert every grant route rejects a
// full-scope PAT specifically via middleware.RequireSession, checked by its
// exact error message so this fails loudly if some other check happened to
// reject the request for an unrelated reason — a 404 on the dummy id, or
// TransferProject's own RequireRole("admin"), would also prove nothing
// here (this token is minted for an admin, so it clears that gate and
// RequireSession has to be what actually stops it). Sibling of
// TestDestroyRoutes_RequireSession and TestRevealRoutes_RequireSession.
func TestGrantRoutes_RequireSession(t *testing.T) {
	resetDB(t)
	adminToken := env.SetupAdmin(t, "admin@test.com", "password123")
	plain := mintScoped(t, adminToken, service.AllScopes)

	router, ok := env.Server.Config.Handler.(chi.Routes)
	require.True(t, ok, "test server handler must be walkable as chi.Routes")

	tested := 0
	err := chi.Walk(router, func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		if !isGrantRoute(method, route) {
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

	// Sanity floor: the two known grant routes (transfer, sharing). If this
	// drops, a route's path changed shape or one was removed — worth an
	// explicit look, the same reasoning destroy_boundary_test.go and
	// reveal_boundary_test.go's own sanity floors give for their sets.
	assert.GreaterOrEqual(t, tested, 2, "the walk should discover at least the two known grant routes")
}
