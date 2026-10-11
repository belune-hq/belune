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

// nestedFixture is two projects, each with a child of every kind that hangs off
// {projectId}, plus a token pinned to the first.
type nestedFixture struct {
	adminToken          string
	mineID, foreignID   string
	mineApp, foreignApp string
	mineDB, foreignDB   string
	mineDom, foreignDom string
	pinned              string
}

func newNestedFixture(t *testing.T) nestedFixture {
	t.Helper()
	resetDB(t)
	adminToken := env.SetupAdmin(t, "admin@test.com", "password123")
	adminUserID := extractID(mustAuthMe(t, adminToken)["id"])

	app := func(projectID, name string) string {
		return extractID(env.CreateApplication(t, adminToken, projectID, map[string]any{
			"name": name, "type": "image", "build_type": "image",
			"source_image": "nginx:1.27",
		})["id"])
	}
	db := func(projectID, name string) string {
		resp := env.DoRequest(t, "POST", fmt.Sprintf("/api/projects/%s/databases", projectID),
			map[string]any{"name": name, "type": "postgres"}, testutil.AuthHeader(adminToken))
		require.Equal(t, http.StatusAccepted, resp.StatusCode)
		defer resp.Body.Close()
		return extractID(testutil.ReadJSON(t, resp)["id"])
	}

	dom := func(projectID, appID, hostname string) string {
		resp := env.DoRequest(t, "POST",
			fmt.Sprintf("/api/projects/%s/applications/%s/domains", projectID, appID),
			map[string]any{"hostname": hostname, "ssl_enabled": false}, testutil.AuthHeader(adminToken))
		require.Equal(t, http.StatusCreated, resp.StatusCode)
		defer resp.Body.Close()
		return extractID(testutil.ReadJSON(t, resp)["id"])
	}

	mine := extractID(env.CreateProject(t, adminToken, "Mine", "mine")["id"])
	foreign := extractID(env.CreateProject(t, adminToken, "Foreign", "foreign")["id"])
	mineApp, foreignApp := app(mine, "Mine App"), app(foreign, "Foreign App")

	return nestedFixture{
		adminToken: adminToken,
		mineID:     mine, foreignID: foreign,
		mineApp: mineApp, foreignApp: foreignApp,
		mineDB: db(mine, "minedb"), foreignDB: db(foreign, "foreigndb"),
		// ⚠️ Domains were named in this file's own commit message and were not
		// actually here. domainOwner is the third lookup that gained
		// project_id, and a mutation dropping it passed the entire suite.
		mineDom:    dom(mine, mineApp, "mine.example.com"),
		foreignDom: dom(foreign, foreignApp, "foreign.example.com"),
		pinned:     createPinnedAPIToken(t, adminUserID, mine, service.AllScopes),
	}
}

func (f nestedFixture) status(t *testing.T, method, path string, body any, token string) int {
	t.Helper()
	resp := env.DoRequest(t, method, path, body, testutil.AuthHeader(token))
	defer resp.Body.Close()
	return resp.StatusCode
}

// TestNestedRoutes_PinSurvivesASubstitutedProjectID is the defect this change
// exists for.
//
// RequirePinnedProjectMatch compares the pin to the {projectId} in the URL and
// nothing verified that the child further along the path belonged to it — and
// canAccessOwned short-circuited on admin before the resource was even looked
// up. So a project-pinned admin token could pass its OWN project id with ANY
// child id and reach the resource. Verified by running it before the fix:
// requests, env and a PUT of env all returned 200 on a foreign application.
func TestNestedRoutes_PinSurvivesASubstitutedProjectID(t *testing.T) {
	f := newNestedFixture(t)

	// ⚠️ Controls first, both directions. Without the "own child" cases the
	// assertions below pass against a gate that refuses everything, and without
	// the "honest foreign path" case they pass against a gate that only reads
	// the URL — which is the bug.
	t.Run("its own children stay reachable", func(t *testing.T) {
		assert.Equal(t, http.StatusOK,
			f.status(t, "GET", fmt.Sprintf("/api/projects/%s/applications/%s/requests", f.mineID, f.mineApp), nil, f.pinned))
		assert.Equal(t, http.StatusOK,
			f.status(t, "GET", fmt.Sprintf("/api/projects/%s/applications/%s/env", f.mineID, f.mineApp), nil, f.pinned))
		assert.Equal(t, http.StatusOK,
			f.status(t, "GET", fmt.Sprintf("/api/projects/%s/databases/%s", f.mineID, f.mineDB), nil, f.pinned))
		assert.Equal(t, http.StatusOK,
			f.status(t, "GET", fmt.Sprintf("/api/projects/%s/applications/%s/domains/%s/features", f.mineID, f.mineApp, f.mineDom), nil, f.pinned))
	})

	t.Run("the honest foreign path is refused by the middleware", func(t *testing.T) {
		assert.Equal(t, http.StatusForbidden,
			f.status(t, "GET", fmt.Sprintf("/api/projects/%s/applications/%s/env", f.foreignID, f.foreignApp), nil, f.pinned))
	})

	t.Run("substituting its own project id no longer reaches a foreign child", func(t *testing.T) {
		for _, tc := range []struct {
			name, method, path string
			body               any
		}{
			{"read requests", "GET",
				fmt.Sprintf("/api/projects/%s/applications/%s/requests", f.mineID, f.foreignApp), nil},
			{"read env", "GET",
				fmt.Sprintf("/api/projects/%s/applications/%s/env", f.mineID, f.foreignApp), nil},
			{"WRITE env", "PUT",
				fmt.Sprintf("/api/projects/%s/applications/%s/env", f.mineID, f.foreignApp),
				map[string]any{"env_vars": []map[string]any{{"key": "INJECTED", "value": "x"}}}},
			{"read the application", "GET",
				fmt.Sprintf("/api/projects/%s/applications/%s", f.mineID, f.foreignApp), nil},
			{"read a database", "GET",
				fmt.Sprintf("/api/projects/%s/databases/%s", f.mineID, f.foreignDB), nil},
			{"read a domain's features", "GET",
				fmt.Sprintf("/api/projects/%s/applications/%s/domains/%s/features", f.mineID, f.mineApp, f.foreignDom), nil},
		} {
			t.Run(tc.name, func(t *testing.T) {
				assert.Equal(t, http.StatusForbidden, f.status(t, tc.method, tc.path, tc.body, f.pinned),
					"a pinned token reached a foreign child by naming its own project")
			})
		}
	})

	t.Run("the write really did not land", func(t *testing.T) {
		// Status codes can lie about effects. Read the foreign application's
		// env back through the admin session and look for the injected key.
		resp := env.DoRequest(t, "GET",
			fmt.Sprintf("/api/projects/%s/applications/%s/env", f.foreignID, f.foreignApp),
			nil, testutil.AuthHeader(f.adminToken))
		defer resp.Body.Close()
		require.Equal(t, http.StatusOK, resp.StatusCode)
		for _, row := range testutil.ReadJSONArray(t, resp) {
			assert.NotEqual(t, "INJECTED", fmt.Sprint(row.(map[string]any)["key"]),
				"the refused PUT still wrote an env var into another project's application")
		}
	})
}

// TestNestedRoutes_UnpinnedCallersAreUnaffected is the other half of the
// bargain: this change alters the gate every project-scoped route in the API
// goes through, so the common paths must be provably untouched.
func TestNestedRoutes_UnpinnedCallersAreUnaffected(t *testing.T) {
	f := newNestedFixture(t)
	unpinned := mintScoped(t, f.adminToken, service.AllScopes)

	for _, token := range []struct{ name, tok string }{
		{"an admin session", f.adminToken},
		{"an unpinned admin token", unpinned},
	} {
		t.Run(token.name+" reaches both projects", func(t *testing.T) {
			for _, p := range []string{
				fmt.Sprintf("/api/projects/%s/applications/%s/env", f.mineID, f.mineApp),
				fmt.Sprintf("/api/projects/%s/applications/%s/env", f.foreignID, f.foreignApp),
				fmt.Sprintf("/api/projects/%s/databases/%s", f.foreignID, f.foreignDB),
			} {
				assert.Equal(t, http.StatusOK, f.status(t, "GET", p, nil, token.tok), p)
			}
		})
	}
}
