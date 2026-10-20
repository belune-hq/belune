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

// destroyResourceKinds are the literal path segments this PR's "tokens
// cannot destroy" boundary covers: the resources whose deletion or restore
// destroys real stored data, matching the same set PR1's isOwnerOnly split
// already established (project, application, database, volume, domain) plus
// the backup-artifact routes ("backups", "orphaned-backups" — NOT
// "backup-configs" or "backup-destinations", which are schedule/destination
// config and deliberately stay reachable, same as PR1's treatment of
// deploy-hooks/file-mounts/route-features/preview environments).
//
// "users" was added after a review found DeleteUser (cascades away every
// project the target owns — projects.user_id is ON DELETE CASCADE) was
// missing RequireSession AND missing from this set, so the structural test
// that exists specifically to catch such omissions had itself been blind to
// it.
//
// "certificates" was here too, for the same reason, then removed when
// DeleteCertificate was deliberately un-gated: domains.certificate_id is
// ON DELETE RESTRICT so only an unused cert is reachable, the route still
// requires admin + write, and a cert is re-uploadable from the PEM that
// created it — it isn't the "destroys unrecoverable stored data" category
// this set is for. See the comment on that route in routes.go.
var destroyResourceKinds = map[string]bool{
	"projects":         true,
	"applications":     true,
	"databases":        true,
	"volumes":          true,
	"domains":          true,
	"backups":          true,
	"orphaned-backups": true,
	"users":            true,
}

// destroyRouteResourceKind classifies a chi route pattern as a delete/restore
// action on one of destroyResourceKinds, or "" if it isn't one — by shape
// alone (DELETE .../<kind>/{id}, or POST .../<kind>/{id}/restore), the same
// shape every delete/restore route in this codebase already follows. It does
// not hand-list routes: TestDestroyRoutes_RequireSession discovers routes
// from the real router and classifies each one through this function, so a
// new destructive route added later is caught automatically rather than
// depending on someone remembering to add it to a list here.
func destroyRouteResourceKind(method, route string) string {
	path := route
	switch method {
	case http.MethodDelete:
	case http.MethodPost:
		if !strings.HasSuffix(path, "/restore") {
			return ""
		}
		path = strings.TrimSuffix(path, "/restore")
	default:
		return ""
	}

	segments := strings.Split(strings.Trim(path, "/"), "/")
	if len(segments) < 2 {
		return ""
	}
	last := segments[len(segments)-1]
	if !strings.HasPrefix(last, "{") {
		return "" // doesn't end in an id param — not this shape at all
	}
	kind := segments[len(segments)-2]
	if destroyResourceKinds[kind] {
		return kind
	}
	return ""
}

// TestDestroyRoutes_RequireSession is the structural half of "tokens cannot
// destroy": it walks the REAL registered router (not a hand-maintained list
// of route strings) and asserts that every delete/restore route on a
// destroyResourceKinds resource rejects a PAT specifically via
// middleware.RequireSession — checked by its exact error message, not just
// any 403, so this fails loudly if some other check happened to reject the
// request for an unrelated reason (e.g. a 404 on the dummy id would also
// prove nothing).
func TestDestroyRoutes_RequireSession(t *testing.T) {
	resetDB(t)
	adminToken := env.SetupAdmin(t, "admin@test.com", "password123")
	plain := mintScoped(t, adminToken, service.AllScopes)

	router, ok := env.Server.Config.Handler.(chi.Routes)
	require.True(t, ok, "test server handler must be walkable as chi.Routes")

	tested := 0
	err := chi.Walk(router, func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		kind := destroyRouteResourceKind(method, route)
		if kind == "" {
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
			"%s %s (resource kind %q) must reject a PAT", method, route, kind)
		assert.Equal(t, "this action requires a session, not a personal access token", body["error"],
			"%s %s must be rejected by RequireSession specifically, not some other check", method, route)
		return nil
	})
	require.NoError(t, err)

	// Sanity floor: the eleven routes RequireSession is known to be on
	// (DeleteProject, DeleteApplication, RemoveDomain, DeleteApplicationVolume,
	// RestoreVolumeBackup, DeleteDatabaseBackup, RestoreDatabase,
	// RestoreDatabaseFromTombstone, DeleteOrphanedDatabaseBackup, DeleteDatabase,
	// DeleteUser). DeleteCertificate was here until it was deliberately
	// un-gated (see destroyResourceKinds above). If this drops, either a
	// route's path changed shape or one was removed — worth knowing either
	// way, not silently passing on zero routes checked.
	assert.GreaterOrEqual(t, tested, 11, "the walk should discover at least the eleven known destroy/restore routes")
}

// TestDestroyRoutes_ClassifierExcludesOperationalConfig pins the negative
// side of the classifier itself: routes PR1 already established as
// "operational, not the resource" (a backup schedule, a destination, a route
// feature) must NOT be swept into the destroy set just because they also
// happen to be DELETE routes near a matching resource name.
func TestDestroyRoutes_ClassifierExcludesOperationalConfig(t *testing.T) {
	cases := []struct {
		method, route string
	}{
		{http.MethodDelete, "/api/projects/{projectId}/databases/{databaseId}/backup-configs/{configId}"},
		{http.MethodDelete, "/api/projects/{projectId}/applications/{applicationId}/volumes/{volumeId}/backup-configs/{configId}"},
		{http.MethodDelete, "/api/projects/{projectId}/backup-destinations/{destId}"},
		{http.MethodDelete, "/api/projects/{projectId}/applications/{applicationId}/domains/{domainId}/features/{featureId}"},
		{http.MethodDelete, "/api/projects/{projectId}/applications/{applicationId}/file-mounts/{fileMountId}"},
		{http.MethodDelete, "/api/tokens/{tokenId}"},
		{http.MethodGet, "/api/projects/{projectId}/databases/{databaseId}/restores"},
		// Revoking a pending invitation only reduces access — the "users"
		// addition to destroyResourceKinds must not accidentally sweep this
		// in just because it shares a path prefix with DeleteUser.
		{http.MethodDelete, "/api/users/invitations/{invitationId}"},
	}
	for _, tc := range cases {
		assert.Equal(t, "", destroyRouteResourceKind(tc.method, tc.route), "%s %s must not classify as a destroy route", tc.method, tc.route)
	}
}
