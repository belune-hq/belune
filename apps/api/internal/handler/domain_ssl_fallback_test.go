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
