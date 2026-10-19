package handler_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/weiliang79/belune/internal/proxy/capability"
	"github.com/weiliang79/belune/internal/testutil"
)

// A Caddy that was probed successfully but has no optional modules: stock.
var stockCaddy = capability.NewSet("http.handlers.reverse_proxy", "http.handlers.static_response")

var beluneCaddy = capability.NewSet("http.handlers.reverse_proxy", capability.ModuleRateLimit)

type featureFixture struct {
	token, base string
}

func newFeatureFixture(t *testing.T) featureFixture {
	t.Helper()
	resetDB(t)
	t.Cleanup(func() { env.Capabilities.Set(capability.Set{}) })

	token := env.SetupAdmin(t, "admin@test.com", "password123")
	project := env.CreateProject(t, token, "Test Project", "test-project")
	projectID := extractID(project["id"])
	app := env.CreateApplication(t, token, projectID, map[string]any{
		"name": "Domain App", "type": "git", "build_type": "dockerfile", "source_repo": "https://github.com/test/repo",
	})
	appID := extractID(app["id"])
	resp := env.DoRequest(t, "POST", fmt.Sprintf("/api/projects/%s/applications/%s/domains", projectID, appID),
		map[string]any{"hostname": "example.com", "ssl_enabled": true}, testutil.AuthHeader(token))
	require.Equal(t, http.StatusCreated, resp.StatusCode)
	domainID := extractID(testutil.ReadJSON(t, resp)["id"])

	return featureFixture{token: token, base: fmt.Sprintf("/api/projects/%s/applications/%s/domains/%s/features", projectID, appID, domainID)}
}

func (f featureFixture) put(t *testing.T, feature, config string, enabled bool) *http.Response {
	t.Helper()
	return env.DoRequest(t, "PUT", f.base, map[string]any{
		"feature_type": feature, "config": json.RawMessage(config), "enabled": enabled,
	}, testutil.AuthHeader(f.token))
}

const rateLimitConfig = `{"window":"1s","max_events":10}`

func TestCapabilities_ReportsThreeStates(t *testing.T) {
	f := newFeatureFixture(t)

	state := func() (bool, string) {
		resp := env.DoRequest(t, "GET", "/api/proxy/capabilities", nil, testutil.AuthHeader(f.token))
		require.Equal(t, http.StatusOK, resp.StatusCode)
		body := testutil.ReadJSON(t, resp)
		rl := body["features"].(map[string]any)["rate_limit"].(map[string]any)
		assert.Equal(t, capability.ModuleRateLimit, rl["module"])
		return body["known"].(bool), rl["state"].(string)
	}

	// No answer from the probe is NOT "none present": claiming the module is
	// missing while Caddy is restarting would send an operator to rebuild an
	// image that was fine.
	known, st := state()
	assert.False(t, known)
	assert.Equal(t, "unknown", st)

	env.Capabilities.Set(stockCaddy)
	known, st = state()
	assert.True(t, known)
	assert.Equal(t, "unavailable", st)

	env.Capabilities.Set(beluneCaddy)
	known, st = state()
	assert.True(t, known)
	assert.Equal(t, "available", st)
}

func TestCapabilities_RequiresAuth(t *testing.T) {
	resetDB(t)
	resp := env.DoRequest(t, "GET", "/api/proxy/capabilities", nil, nil)
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
}

func TestGate1_RefusesOnlyWhenTheModuleIsKnownAbsent(t *testing.T) {
	f := newFeatureFixture(t)

	t.Run("refuses when known absent", func(t *testing.T) {
		env.Capabilities.Set(stockCaddy)
		resp := f.put(t, "rate_limit", rateLimitConfig, true)
		assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
		assert.Contains(t, testutil.ReadJSON(t, resp)["error"], capability.ModuleRateLimit)
	})

	// The one that matters: a UX gate that refuses legitimate work because the
	// probe failed is worse than no gate. Gate 2 is the actual protection.
	t.Run("allows when the probe is unknown", func(t *testing.T) {
		env.Capabilities.Set(capability.Set{})
		resp := f.put(t, "rate_limit", rateLimitConfig, true)
		assert.Equal(t, http.StatusOK, resp.StatusCode)
	})

	t.Run("allows when present", func(t *testing.T) {
		env.Capabilities.Set(beluneCaddy)
		resp := f.put(t, "rate_limit", rateLimitConfig, true)
		assert.Equal(t, http.StatusOK, resp.StatusCode)
	})

	t.Run("never gates a feature stock Caddy serves", func(t *testing.T) {
		env.Capabilities.Set(stockCaddy)
		resp := f.put(t, "ip_allowlist", `{"ranges":["10.0.0.0/8"]}`, true)
		assert.Equal(t, http.StatusOK, resp.StatusCode)
	})

	t.Run("disabling is never refused", func(t *testing.T) {
		env.Capabilities.Set(stockCaddy)
		resp := f.put(t, "rate_limit", rateLimitConfig, false)
		assert.Equal(t, http.StatusOK, resp.StatusCode)
	})

	t.Run("still validates the config first", func(t *testing.T) {
		env.Capabilities.Set(beluneCaddy)
		resp := f.put(t, "rate_limit", `{"window":"1s"}`, true)
		assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
	})
}

// An operator who moves to a Caddy without the module must still be able to see
// and remove the rows that are now inert.
func TestGate1_ListAndDeleteWorkForAnInertFeature(t *testing.T) {
	f := newFeatureFixture(t)

	env.Capabilities.Set(beluneCaddy)
	resp := f.put(t, "rate_limit", rateLimitConfig, true)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	featureID := extractID(testutil.ReadJSON(t, resp)["id"])

	env.Capabilities.Set(stockCaddy)

	list := env.DoRequest(t, "GET", f.base, nil, testutil.AuthHeader(f.token))
	require.Equal(t, http.StatusOK, list.StatusCode)
	assert.Contains(t, fmt.Sprint(testutil.ReadJSONArray(t, list)), "rate_limit")

	del := env.DoRequest(t, "DELETE", f.base+"/"+featureID, nil, testutil.AuthHeader(f.token))
	assert.Equal(t, http.StatusOK, del.StatusCode)
}
