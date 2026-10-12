package handler_test

import (
	"context"
	"fmt"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/weiliang79/belune/internal/testutil"
)

// TestUpdateDomain_OmittedSSLModeKeepsTheStoredOne is the regression test for a
// silent TLS downgrade.
//
// PUT .../domains/{id} used to default an omitted ssl_mode to "automatic"
// rather than falling back to the stored value, so a request that only meant
// to change the path took TLS with it:
//
//   - "off"    -> "automatic", starting an ACME request for a hostname that
//     cannot validate, because TLS is terminated in front of Belune
//   - "custom" -> "automatic" with certificate_id cleared, so the uploaded
//     certificate stopped being served AND an ACME request began for a
//     hostname that resolves to Cloudflare
//
// Both are documented configurations. The dashboard never hit this because its
// form always sends ssl_mode; a script using the published schema, where the
// field is optional, did.
func TestUpdateDomain_OmittedSSLModeKeepsTheStoredOne(t *testing.T) {
	resetDB(t)
	token := env.SetupAdmin(t, "domain-ssl@test.com", "password123")
	projectID := extractID(env.CreateProject(t, token, "P", "p")["id"])
	appID := extractID(minimalApp(t, token, projectID)["id"])

	read := func(domainID string) (sslMode string, hasCert bool) {
		t.Helper()
		var mode string
		var cert *string
		require.NoError(t, env.Pool.QueryRow(context.Background(),
			`SELECT ssl_mode, certificate_id::text FROM domains WHERE id = $1`, domainID).Scan(&mode, &cert))
		return mode, cert != nil
	}

	for _, mode := range []string{"off", "automatic"} {
		t.Run("stored "+mode+" survives a PUT that omits ssl_mode", func(t *testing.T) {
			resp := env.DoRequest(t, "POST",
				fmt.Sprintf("/api/projects/%s/applications/%s/domains", projectID, appID),
				map[string]any{"hostname": mode + ".example.com", "ssl_mode": mode}, testutil.AuthHeader(token))
			created := testutil.ReadJSON(t, resp)
			domainID := extractID(created["id"])
			before, _ := read(domainID)
			require.Equal(t, mode, before, "seeding failed")

			// A PUT that means to change only the path.
			upd := env.DoRequest(t, "PUT",
				fmt.Sprintf("/api/projects/%s/applications/%s/domains/%s", projectID, appID, domainID),
				map[string]any{"hostname": mode + ".example.com", "path": "/moved"}, testutil.AuthHeader(token))
			defer upd.Body.Close()
			require.Equal(t, http.StatusOK, upd.StatusCode)

			after, _ := read(domainID)
			assert.Equal(t, mode, after,
				"an omitted ssl_mode must keep the stored mode, not reset it to automatic")
		})
	}
}

// TestUpdateDomain_ExplicitSSLModeStillApplies is the control: the fallback
// must not make the field unwritable. Without this, a handler that ignored
// ssl_mode entirely would pass the test above.
func TestUpdateDomain_ExplicitSSLModeStillApplies(t *testing.T) {
	resetDB(t)
	token := env.SetupAdmin(t, "domain-ssl-explicit@test.com", "password123")
	projectID := extractID(env.CreateProject(t, token, "P", "p")["id"])
	appID := extractID(minimalApp(t, token, projectID)["id"])

	resp := env.DoRequest(t, "POST",
		fmt.Sprintf("/api/projects/%s/applications/%s/domains", projectID, appID),
		map[string]any{"hostname": "explicit.example.com", "ssl_mode": "automatic"}, testutil.AuthHeader(token))
	domainID := extractID(testutil.ReadJSON(t, resp)["id"])

	upd := env.DoRequest(t, "PUT",
		fmt.Sprintf("/api/projects/%s/applications/%s/domains/%s", projectID, appID, domainID),
		map[string]any{"hostname": "explicit.example.com", "ssl_mode": "off"}, testutil.AuthHeader(token))
	defer upd.Body.Close()
	require.Equal(t, http.StatusOK, upd.StatusCode)

	var mode string
	require.NoError(t, env.Pool.QueryRow(context.Background(),
		`SELECT ssl_mode FROM domains WHERE id = $1`, domainID).Scan(&mode))
	assert.Equal(t, "off", mode, "an explicit ssl_mode must still be written")
}

// TestUpdateDomain_OmittedSSLModeOnAMultiPathHostname is the case the tests
// above could not reach: every hostname in them has exactly one row, so
// checkHostTLSAgreement's sibling loop never ran.
//
// ⚠️ The ssl_mode fallback fixed the WRITE but left the agreement check reading
// the raw request field, which is now "" when omitted. The check compares that
// against each sibling's real mode, so every sibling mismatched and the request
// was refused for disagreeing with the mode it was about to keep. On main the
// same line read the same field, but the field had already been defaulted to
// "automatic" — removing that default is what exposed this.
func TestUpdateDomain_OmittedSSLModeOnAMultiPathHostname(t *testing.T) {
	resetDB(t)
	token := env.SetupAdmin(t, "domain-multipath@test.com", "password123")
	projectID := extractID(env.CreateProject(t, token, "P", "p")["id"])
	appID := extractID(minimalApp(t, token, projectID)["id"])

	add := func(path string) string {
		t.Helper()
		resp := env.DoRequest(t, "POST",
			fmt.Sprintf("/api/projects/%s/applications/%s/domains", projectID, appID),
			map[string]any{"hostname": "shop.example.com", "path": path, "ssl_mode": "automatic"},
			testutil.AuthHeader(token))
		require.Equal(t, http.StatusCreated, resp.StatusCode, "seeding %s", path)
		return extractID(testutil.ReadJSON(t, resp)["id"])
	}
	rootID := add("/")
	_ = add("/api")

	// A PUT that means to change only the path, on a hostname that has a
	// sibling row. Both are "automatic", so there is nothing to disagree with.
	resp := env.DoRequest(t, "PUT",
		fmt.Sprintf("/api/projects/%s/applications/%s/domains/%s", projectID, appID, rootID),
		map[string]any{"hostname": "shop.example.com", "path": "/store"},
		testutil.AuthHeader(token))
	defer resp.Body.Close()
	body := testutil.ReadJSON(t, resp)
	require.Equal(t, http.StatusOK, resp.StatusCode,
		"an omitted ssl_mode must not read as disagreeing with its own siblings: %v", body["error"])

	var mode string
	require.NoError(t, env.Pool.QueryRow(context.Background(),
		`SELECT ssl_mode FROM domains WHERE id = $1`, rootID).Scan(&mode))
	assert.Equal(t, "automatic", mode, "the stored mode must survive")
}

// TestAddDomain_ConflictBodyHasNoSentinelPrefix: the 409 body is published, and
// extracting createDomainRecord briefly changed it to
// "domain conflict: shop.com is already configured" because the sentinel was
// wrapped with %w and the message written with err.Error(). A typed error
// keeps errors.Is working and the text clean.
func TestAddDomain_ConflictBodyHasNoSentinelPrefix(t *testing.T) {
	resetDB(t)
	token := env.SetupAdmin(t, "domain-conflict-body@test.com", "password123")
	projectID := extractID(env.CreateProject(t, token, "P", "p")["id"])
	appID := extractID(minimalApp(t, token, projectID)["id"])

	add := func() *http.Response {
		return env.DoRequest(t, "POST",
			fmt.Sprintf("/api/projects/%s/applications/%s/domains", projectID, appID),
			map[string]any{"hostname": "dupe.example.com", "ssl_mode": "off"}, testutil.AuthHeader(token))
	}
	first := add()
	require.Equal(t, http.StatusCreated, first.StatusCode)
	first.Body.Close()

	second := add()
	defer second.Body.Close()
	require.Equal(t, http.StatusConflict, second.StatusCode)
	msg := fmt.Sprint(testutil.ReadJSON(t, second)["error"])
	assert.Equal(t, "dupe.example.com is already configured", msg,
		"the published 409 body must not carry the sentinel's text")
}

// TestSetHealthCheck_NegativeTuningBecomesNull: moving the validation into the
// service briefly changed `v <= 0` to `v != 0`, so a negative timeout
// persisted where it had become NULL. Nothing downstream breaks — the deploy
// path guards > 0 — but the API response and the dashboard form then showed
// the negative back, which is behaviour drift inside a pure move.
func TestSetHealthCheck_NegativeTuningBecomesNull(t *testing.T) {
	resetDB(t)
	token := env.SetupAdmin(t, "hc-negative@test.com", "password123")
	projectID := extractID(env.CreateProject(t, token, "P", "p")["id"])
	appID := extractID(minimalApp(t, token, projectID)["id"])

	resp := env.DoRequest(t, "PUT",
		fmt.Sprintf("/api/projects/%s/applications/%s/health-check", projectID, appID),
		map[string]any{"type": "http", "path": "/healthz", "timeout_seconds": -5},
		testutil.AuthHeader(token))
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	var timeout *int32
	require.NoError(t, env.Pool.QueryRow(context.Background(),
		`SELECT health_check_timeout_seconds FROM applications WHERE id = $1`, appID).Scan(&timeout))
	assert.Nil(t, timeout, "a non-positive tuning value must be stored as NULL, not echoed back")
}
