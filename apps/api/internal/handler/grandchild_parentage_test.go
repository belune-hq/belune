package handler_test

import (
	"context"
	"fmt"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestGrandchildrenAreCheckedAgainstTheirParent covers the layer below the one
// v0.1.19's nested-route fix closed, and the layer a gate-reaching tripwire
// cannot see.
//
// ⚠️ Those handlers DO call canAccessApplication/canAccessDomain — on the
// PARENT in the path. What was missing is that nothing tied the grandchild to
// that parent, so naming a parent you may reach and any grandchild id reached
// it. TestEveryNestedRouteReachesTheOwnershipGate passes on all three of these
// both before and after the fix, because it can only see that a gate was
// called, never which id it was called with. These are the behavioural half.
//
// Reproduced before the fix, with a token pinned to "Mine":
//
//	GET    .../applications/<mine>/deployments/<foreign>            -> 200
//	GET    .../applications/<mine>/deployments/<foreign>/build-logs -> 200 + logs
//	DELETE .../domains/<mine>/features/<foreign>                    -> 200, row gone
func TestGrandchildrenAreCheckedAgainstTheirParent(t *testing.T) {
	f := newNestedFixture(t)
	ctx := context.Background()

	deployIn := func(appID, sha string) string {
		var id string
		require.NoError(t, env.Pool.QueryRow(ctx, `
			INSERT INTO deployments (application_id, status, triggered_by, commit_sha, image_tag, build_logs)
			VALUES ($1,'success','manual',$2,'belune/x:1','SECRET_BUILD_ARG=hunter2')
			RETURNING id`, appID, sha).Scan(&id))
		return id
	}
	featureOn := func(domainID string) string {
		var id string
		require.NoError(t, env.Pool.QueryRow(ctx, `
			INSERT INTO domain_route_features (domain_id, feature_type, config)
			VALUES ($1,'ip_allowlist','{"cidrs":["10.0.0.0/8"]}') RETURNING id`, domainID).Scan(&id))
		return id
	}

	mineDeploy, foreignDeploy := deployIn(f.mineApp, "aaaaaaa"), deployIn(f.foreignApp, "bbbbbbb")
	mineFeature, foreignFeature := featureOn(f.mineDom), featureOn(f.foreignDom)

	// ⚠️ Controls first. Each grandchild must still be reachable under its OWN
	// parent, or every assertion below passes against a handler that refuses
	// everything — which is the failure mode a parentage check invites.
	t.Run("a grandchild under its own parent still works", func(t *testing.T) {
		assert.Equal(t, http.StatusOK, f.status(t, "GET",
			fmt.Sprintf("/api/projects/%s/applications/%s/deployments/%s", f.mineID, f.mineApp, mineDeploy), nil, f.pinned))
		// ⚠️ build-logs needs its own control, and had none. The parentage
		// check here can be mutated to `if true` — an unconditional 404 on
		// every build-log stream, i.e. the operator's "watch the build" view
		// broken outright — and the ENTIRE handler suite still passes: no
		// other test drives this route over HTTP. Found by mutation.
		//
		// Terminates rather than streaming forever because deployIn inserts
		// status 'success', so the handler sends the stored logs and a done
		// event and returns.
		assert.Equal(t, http.StatusOK, f.status(t, "GET",
			fmt.Sprintf("/api/projects/%s/applications/%s/deployments/%s/build-logs", f.mineID, f.mineApp, mineDeploy), nil, f.pinned))
		assert.Equal(t, http.StatusOK, f.status(t, "DELETE",
			fmt.Sprintf("/api/projects/%s/applications/%s/domains/%s/features/%s", f.mineID, f.mineApp, f.mineDom, mineFeature), nil, f.pinned))
	})

	t.Run("a foreign grandchild under a parent the caller may reach is refused", func(t *testing.T) {
		for _, tc := range []struct{ name, method, path string }{
			{"read the deployment", "GET",
				fmt.Sprintf("/api/projects/%s/applications/%s/deployments/%s", f.mineID, f.mineApp, foreignDeploy)},
			{"stream its build logs", "GET",
				fmt.Sprintf("/api/projects/%s/applications/%s/deployments/%s/build-logs", f.mineID, f.mineApp, foreignDeploy)},
			{"delete the route feature", "DELETE",
				fmt.Sprintf("/api/projects/%s/applications/%s/domains/%s/features/%s", f.mineID, f.mineApp, f.mineDom, foreignFeature)},
		} {
			t.Run(tc.name, func(t *testing.T) {
				assert.Equal(t, http.StatusNotFound, f.status(t, tc.method, tc.path, nil, f.pinned),
					"a grandchild of another project was reachable by naming a parent the caller may reach")
			})
		}
	})

	// A status code is not evidence the delete did not happen.
	t.Run("the refused delete did not land", func(t *testing.T) {
		var left int
		require.NoError(t, env.Pool.QueryRow(ctx,
			`SELECT count(*) FROM domain_route_features WHERE id = $1`, foreignFeature).Scan(&left))
		assert.Equal(t, 1, left,
			"the refused DELETE still removed another project's route feature")
	})

	// ⚠️ Not only a pin problem. The same substitution works for any Member
	// with an application of their own, so the fix must hold without a pin.
	t.Run("a member cannot reach another member's grandchild either", func(t *testing.T) {
		_, otherToken := createMember(t, f.adminToken, "other@test.com")
		otherProject := extractID(env.CreateProject(t, otherToken, "Other", "other")["id"])
		otherApp := extractID(env.CreateApplication(t, otherToken, otherProject, map[string]any{
			"name": "Other App", "type": "image", "build_type": "image", "source_image": "nginx:1.27"})["id"])

		assert.Equal(t, http.StatusNotFound, f.status(t, "GET",
			fmt.Sprintf("/api/projects/%s/applications/%s/deployments/%s", otherProject, otherApp, foreignDeploy), nil, otherToken))
	})
}
