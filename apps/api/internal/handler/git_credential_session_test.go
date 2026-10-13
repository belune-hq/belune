package handler_test

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/weiliang79/belune/internal/testutil"
)

const sessionOnlyMessage = "this action requires a session, not a personal access token"

// The four git credential routes are browser flows / credential operations and
// require a signed-in session. Both directions are asserted for each: a gate
// that only ever has its refusal tested can ship inert, or refuse everyone.
// A PAT refusal is matched on RequireSession's own message, because several
// other layers also answer 403 and a bare status would pass for the wrong reason.
func TestGitCredentialRoutes_RequireASession(t *testing.T) {
	resetDB(t)
	adminToken := env.SetupAdmin(t, "git-session@test.com", "password123")
	// Admin-role owner, so the PAT is refused by RequireSession itself rather
	// than by the role or scope layers that sit in front of it.
	writeToken := mintScoped(t, adminToken, []string{"write"})

	do := func(token, method, path string, body any) (int, string) {
		resp := env.DoRequest(t, method, path, body, testutil.AuthHeader(token))
		defer resp.Body.Close()
		b, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		return resp.StatusCode, string(b)
	}
	const unknownID = "00000000-0000-4000-8000-000000000000"
	save := map[string]any{"provider": "github", "client_id": "cid", "client_secret": "secret"}

	// Real session behaviour, end to end: save → connect → delete.
	status, body := do(adminToken, "PUT", "/api/git/providers", save)
	require.Equal(t, http.StatusOK, status, body)
	var saved map[string]any
	require.NoError(t, json.Unmarshal([]byte(body), &saved))
	configID := extractID(saved["id"])

	status, body = do(adminToken, "GET", "/api/git/integrations/connect?provider=github", nil)
	// The harness has no public base URL, so the handler itself answers 400
	// after the gate; what matters here is that the session got through it.
	assert.NotContains(t, body, sessionOnlyMessage, "a session must pass the gate on connect (status %d)", status)
	assert.NotEqual(t, http.StatusForbidden, status)

	// A token is refused on all four, and writes nothing.
	for _, tc := range []struct {
		method, path string
		body         any
	}{
		{"GET", "/api/git/integrations/connect?provider=github", nil},
		{"DELETE", "/api/git/integrations/" + unknownID, nil},
		{"PUT", "/api/git/providers", save},
		{"DELETE", "/api/git/providers/" + configID, nil},
	} {
		t.Run("token refused "+tc.method+" "+strings.SplitN(tc.path, "?", 2)[0], func(t *testing.T) {
			status, body := do(writeToken, tc.method, tc.path, tc.body)
			assert.Equal(t, http.StatusForbidden, status)
			assert.Contains(t, body, sessionOnlyMessage)
		})
	}

	// The refused DELETE must have left the config in place...
	status, body = do(adminToken, "GET", "/api/git/providers", nil)
	require.Equal(t, http.StatusOK, status)
	assert.Contains(t, body, configID, "a token's refused DELETE removed the provider config")

	// ...and the session may still do it (positive control for the DELETEs).
	status, body = do(adminToken, "DELETE", "/api/git/providers/"+configID, nil)
	assert.Equal(t, http.StatusOK, status, body)
	status, body = do(adminToken, "DELETE", "/api/git/integrations/"+unknownID, nil)
	assert.NotContains(t, body, sessionOnlyMessage, "a session must pass the gate on DELETE integration (status %d)", status)

	// Reads stay open to a token.
	for _, path := range []string{"/api/git/integrations", "/api/git/integrations/available", "/api/git/providers"} {
		status, _ := do(writeToken, "GET", path, nil)
		assert.Equal(t, http.StatusOK, status, "GET %s must stay reachable by a token", path)
	}
}

// The two provider redirect targets carry no Authorization header by design and
// are guarded by a one-time state nonce. Pulling them behind the session gate
// would break the OAuth flow outright, so pin that they answer anonymously —
// with their own validation error, not a 401/403.
func TestGitCallbacks_StayPublic(t *testing.T) {
	resetDB(t)
	for _, path := range []string{
		"/api/git/integrations/callback",
		"/api/git/providers/github/manifest/callback",
	} {
		resp := env.DoRequest(t, "GET", path, nil, nil)
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		assert.Equal(t, http.StatusBadRequest, resp.StatusCode, "%s must be reachable without credentials: %s", path, b)
	}
}
