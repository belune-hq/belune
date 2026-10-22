package handler_test

import (
	"errors"
	"io"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/belune-hq/belune/internal/proxy/capability"
	"github.com/belune-hq/belune/internal/testutil"
)

const customCertConfig = `{"apps":{"tls":{"certificates":{"load_pem":[{
  "certificate":"-----BEGIN CERTIFICATE-----\nAAA\n-----END CERTIFICATE-----\n",
  "key":"-----BEGIN PRIVATE KEY-----\nSECRETKEYMATERIAL\n-----END PRIVATE KEY-----\n",
  "tags":["example.com"]}]}}}}`

func proxyViewFixture(t *testing.T) string {
	t.Helper()
	resetDB(t)
	token := env.SetupAdmin(t, "admin@test.com", "password123")
	t.Cleanup(func() {
		*env.ProxyAdmin = testutil.MockProxyAdmin{}
		env.Capabilities.Set(capability.Set{})
	})
	return token
}

// The endpoint, not just the function: a key that never leaves the server cannot
// reach a browser tab, a screenshot or a screen share.
func TestProxyConfig_NeverReturnsPrivateKeys(t *testing.T) {
	token := proxyViewFixture(t)
	env.ProxyAdmin.ConfigOut = customCertConfig

	resp := env.DoRequest(t, "GET", "/api/maintenance/proxy/config", nil, testutil.AuthHeader(token))
	require.Equal(t, http.StatusOK, resp.StatusCode)
	raw, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	body := string(raw)

	assert.NotContains(t, body, "SECRETKEYMATERIAL")
	assert.NotContains(t, body, "PRIVATE KEY")
	assert.Contains(t, body, "BEGIN CERTIFICATE", "the public certificate is still shown")
	assert.Contains(t, body, `"redacted_fields":1`)
}

func TestProxyConfig_FailsClosed(t *testing.T) {
	token := proxyViewFixture(t)

	t.Run("invalid JSON is never passed through", func(t *testing.T) {
		env.ProxyAdmin.ConfigOut = `{"key":"-----BEGIN PRIVATE KEY----- SECRETKEYMATERIAL`
		resp := env.DoRequest(t, "GET", "/api/maintenance/proxy/config", nil, testutil.AuthHeader(token))
		raw, _ := io.ReadAll(resp.Body)
		assert.Equal(t, http.StatusInternalServerError, resp.StatusCode)
		assert.NotContains(t, string(raw), "SECRETKEYMATERIAL")
	})
	t.Run("an unreachable admin API is a gateway error, not an empty config", func(t *testing.T) {
		env.ProxyAdmin.ConfigOut = ""
		env.ProxyAdmin.ConfigErr = errors.New("connection refused")
		resp := env.DoRequest(t, "GET", "/api/maintenance/proxy/config", nil, testutil.AuthHeader(token))
		resp.Body.Close()
		assert.Equal(t, http.StatusBadGateway, resp.StatusCode)
	})
}

func TestProxyMetrics_StatesAreDistinct(t *testing.T) {
	token := proxyViewFixture(t)
	state := func() string {
		resp := env.DoRequest(t, "GET", "/api/maintenance/proxy/metrics", nil, testutil.AuthHeader(token))
		require.Equal(t, http.StatusOK, resp.StatusCode)
		return testutil.ReadJSON(t, resp)["state"].(string)
	}

	env.ProxyAdmin.MetricsErr = errors.New("refused")
	assert.Equal(t, "unreachable", state())

	env.ProxyAdmin.MetricsErr = nil
	env.ProxyAdmin.Enabled = false
	assert.Equal(t, "disabled", state())

	// Same empty exposition, but the config says metrics are on: idle, not broken.
	env.ProxyAdmin.Enabled = true
	assert.Equal(t, "no_data", state())

	env.ProxyAdmin.MetricsOut = `# TYPE caddy_http_requests_in_flight gauge
caddy_http_requests_in_flight{handler="reverse_proxy",server="srv0"} 2
`
	assert.Equal(t, "collecting", state())
}

// unknown must never read as unavailable: a Caddy that is merely restarting is
// not one that lacks the module.
func TestProxyPanel_OptionalModulesKeepThreeStates(t *testing.T) {
	token := caddyPanelFixture(t)
	get := func(name string) string {
		c := getCaddyPanel(t, token)
		return c["optional_modules"].(map[string]any)[name].(map[string]any)["state"].(string)
	}

	assert.Equal(t, "unknown", get("crowdsec"))

	env.Capabilities.Set(stockCaddy)
	assert.Equal(t, "unavailable", get("crowdsec"))
	assert.Equal(t, "unavailable", get("waf"))

	env.Capabilities.Set(capability.NewSet("http", capability.ModuleCrowdSec))
	assert.Equal(t, "available", get("crowdsec"))
	assert.Equal(t, "unavailable", get("waf"))
}
