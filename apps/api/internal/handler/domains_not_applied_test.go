package handler_test

import (
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/belune-hq/belune/internal/proxy"
	"github.com/belune-hq/belune/internal/testutil"
)

// not_applied is what the reconciler OBSERVED it stripped, shown only once it has
// outlasted two reconcile intervals. The window is the whole difference from
// warning on an unknown capability state, which fires on every update.
func TestDomains_NotAppliedHonoursGraceAndScope(t *testing.T) {
	resetDB(t)
	adminToken := env.SetupAdmin(t, "admin@test.com", "password123")
	_, ownerToken := createMember(t, adminToken, "owner@test.com")
	_, otherToken := createMember(t, adminToken, "other@test.com")

	mkApp := func(token, slug, host string) (base string) {
		project := env.CreateProject(t, token, slug, slug)
		projectID := extractID(project["id"])
		app := env.CreateApplication(t, token, projectID, map[string]any{
			"name": slug, "type": "git", "build_type": "railpack",
			"source_repo": "https://github.com/test/repo", "branch": "main",
		})
		base = fmt.Sprintf("/api/projects/%s/applications/%s/domains", projectID, extractID(app["id"]))
		resp := env.DoRequest(t, "POST", base, map[string]any{"hostname": host}, testutil.AuthHeader(token))
		require.Equal(t, http.StatusCreated, resp.StatusCode)
		resp.Body.Close()
		return base
	}
	ownerBase := mkApp(ownerToken, "owner-project", "mine.example.com")
	mkApp(otherToken, "other-project", "theirs.example.com")

	fetch := func() []map[string]any {
		resp := env.DoRequest(t, "GET", ownerBase, nil, testutil.AuthHeader(ownerToken))
		require.Equal(t, http.StatusOK, resp.StatusCode)
		var rows []map[string]any
		for _, v := range testutil.ReadJSONArray(t, resp) {
			rows = append(rows, v.(map[string]any))
		}
		return rows
	}
	degrade := func(age time.Duration) {
		since := time.Now().Add(-age)
		env.Reconciler.Status_ = proxy.ReconcilerStatus{
			IntervalSeconds: 30,
			Degraded: []proxy.Degradation{
				{Hostname: "mine.example.com", Path: "/", Feature: proxy.FeatureRateLimit,
					Module: "http.handlers.rate_limit", Reason: proxy.ReasonModulesUnknown, Since: since},
				{Hostname: "theirs.example.com", Path: "/", Feature: proxy.FeatureRateLimit,
					Module: "http.handlers.rate_limit", Reason: proxy.ReasonModulesUnknown, Since: since},
			},
		}
	}
	t.Cleanup(func() { env.Reconciler.Status_ = proxy.ReconcilerStatus{} })

	// Inside the window (2 x 30s): nothing, however real the degradation is.
	degrade(59 * time.Second)
	assert.NotContains(t, fetch()[0], "not_applied", "a degradation inside the grace window must not be reported")

	// Past it: reported, with the reason and since the UI needs.
	degrade(61 * time.Second)
	rows := fetch()
	require.Len(t, rows, 1)
	na, ok := rows[0]["not_applied"].([]any)
	require.True(t, ok, "a degradation past the grace window must be reported")
	require.Len(t, na, 1, "another project's hostname must never appear")
	entry := na[0].(map[string]any)
	assert.Equal(t, "rate_limit", entry["feature_type"])
	assert.Equal(t, "modules_unknown", entry["reason"])
	assert.NotEmpty(t, entry["since"])

	// The single-domain read is the same shape.
	domainID := extractID(rows[0]["id"])
	resp := env.DoRequest(t, "GET", ownerBase+"/"+domainID, nil, testutil.AuthHeader(ownerToken))
	require.Equal(t, http.StatusOK, resp.StatusCode)
	one := testutil.ReadJSON(t, resp)
	assert.Contains(t, one, "not_applied")
	assert.Contains(t, one, "route_features", "embedding must keep the row's own fields inline")

	// The admin debugging view stays unfiltered.
	degrade(time.Second)
	resp = env.DoRequest(t, "GET", "/api/maintenance/proxy", nil, testutil.AuthHeader(adminToken))
	require.Equal(t, http.StatusOK, resp.StatusCode)
	raw := testutil.ReadJSON(t, resp)
	assert.Len(t, raw["degraded"], 2, "the maintenance view must keep every raw degradation")
}
