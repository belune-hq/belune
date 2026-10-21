package handler_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/bcrypt"

	"github.com/belune-hq/belune/internal/testutil"
)

// A recognisable password, so a leak anywhere (a row, a response, an audit entry)
// is a plain substring match.
const gatePassword = "hunter2-VERY-SECRET-9f8e"

type gateEnv struct {
	token       string
	featuresURL string // .../domains/{id}/features
	domainID    string
	hostname    string
}

func newGateEnv(t *testing.T) gateEnv {
	t.Helper()
	resetDB(t)
	token := env.SetupAdmin(t, "gate-admin@test.com", "password123")
	project := env.CreateProject(t, token, "Gate", "gate")
	projectID := extractID(project["id"])
	app := env.CreateApplication(t, token, projectID, map[string]any{
		"name": "gate-app", "type": "git", "build_type": "railpack",
		"source_repo": "https://github.com/test/repo", "branch": "main",
	})
	domains := fmt.Sprintf("/api/projects/%s/applications/%s/domains", projectID, extractID(app["id"]))
	const host = "gate.example.com"
	resp := env.DoRequest(t, "POST", domains, map[string]any{"hostname": host}, testutil.AuthHeader(token))
	require.Equal(t, http.StatusCreated, resp.StatusCode)
	domainID := extractID(testutil.ReadJSON(t, resp)["id"])
	return gateEnv{token: token, featuresURL: domains + "/" + domainID + "/features", domainID: domainID, hostname: host}
}

func (g gateEnv) put(t *testing.T, featureType string, config map[string]any, enabled bool) *http.Response {
	t.Helper()
	return env.DoRequest(t, "PUT", g.featuresURL, map[string]any{
		"feature_type": featureType, "config": config, "enabled": enabled,
	}, testutil.AuthHeader(g.token))
}

func readBody(t *testing.T, resp *http.Response) string {
	t.Helper()
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return string(b)
}

// row is what Postgres actually holds for the domain's basic_auth feature.
func (g gateEnv) row(t *testing.T) (config map[string]any, text string, enabled bool) {
	t.Helper()
	require.NoError(t, env.Pool.QueryRow(context.Background(),
		`SELECT config::text, enabled FROM domain_route_features WHERE domain_id = $1::uuid AND feature_type = 'basic_auth'`,
		g.domainID).Scan(&text, &enabled), "the feature must be stored")
	require.NoError(t, json.Unmarshal([]byte(text), &config))
	return config, text, enabled
}

// featureConfig returns a feature's config as an object and as text, whichever
// form an endpoint sent it in. The feature endpoints (PUT, and the features
// list) send config as a base64 string: the generated row holds it as []byte and
// the published spec says `format: byte`. The domain read embeds it as an object.
//
// It matters for what is asserted: a substring check on the raw body of the first
// kind proves nothing, because base64 hides every substring. Every "no plaintext"
// and "no password key" check below decodes first.
func featureConfig(t *testing.T, raw any) (map[string]any, string) {
	t.Helper()
	var text string
	switch v := raw.(type) {
	case string:
		b, err := base64.StdEncoding.DecodeString(v)
		require.NoError(t, err, "a string config must be base64")
		text = string(b)
	case map[string]any:
		b, err := json.Marshal(v)
		require.NoError(t, err)
		text = string(b)
	default:
		require.Failf(t, "unexpected config form", "%T", raw)
	}
	var m map[string]any
	require.NoError(t, json.Unmarshal([]byte(text), &m))
	return m, text
}

func configKeys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// ⛔ The regression this whole feature can suffer. The handler used to store the
// request's own bytes and call ParseFeatureConfig only to validate, so hashing
// anywhere else changes nothing: the plaintext would still be written to
// Postgres, returned by every later read, and handed to the route builder. This
// reads the row back from the DATABASE.
func TestRouteFeatures_BasicAuthPasswordIsHashedBeforeItIsStored(t *testing.T) {
	g := newGateEnv(t)

	resp := g.put(t, "basic_auth", map[string]any{"username": "alice", "password": gatePassword}, true)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var put map[string]any
	require.NoError(t, json.Unmarshal([]byte(readBody(t, resp)), &put))
	echoed, echoedText := featureConfig(t, put["config"])
	assert.NotContains(t, echoedText, gatePassword, "the response must not carry the password back")
	assert.ElementsMatch(t, []string{"username", "hashed_password"}, configKeys(echoed))

	config, text, enabled := g.row(t)
	assert.True(t, enabled)
	assert.ElementsMatch(t, []string{"username", "hashed_password"}, configKeys(config),
		"stored: a username and a hash, and no password key")
	assert.Equal(t, "alice", config["username"])
	hash, _ := config["hashed_password"].(string)
	require.NoError(t, bcrypt.CompareHashAndPassword([]byte(hash), []byte(gatePassword)),
		"the stored hash must verify against the password that was sent")
	assert.NotContains(t, text, gatePassword)

	// Not just that row: the plaintext is nowhere in the table.
	var leaked int
	require.NoError(t, env.Pool.QueryRow(context.Background(),
		`SELECT count(*) FROM domain_route_features WHERE config::text LIKE '%' || $1 || '%'`, gatePassword).Scan(&leaked))
	assert.Zero(t, leaked)

	// The route builder reads the stored row back: it must be handed the hash.
	require.NotEmpty(t, env.Proxy.AddedRoutes, "a feature change rebuilds the route")
	route := env.Proxy.AddedRoutes[len(env.Proxy.AddedRoutes)-1]
	require.Len(t, route.Features, 1)
	assert.NotContains(t, string(route.Features[0].Config), gatePassword)
	assert.Contains(t, string(route.Features[0].Config), hash)
}

// The contract: no read ever returns a password key.
func TestRouteFeatures_ReadsNeverReturnAPassword(t *testing.T) {
	g := newGateEnv(t)
	require.Equal(t, http.StatusOK, g.put(t, "basic_auth", map[string]any{"username": "alice", "password": gatePassword}, true).StatusCode)

	read := func(t *testing.T, url string) []byte {
		resp := env.DoRequest(t, "GET", url, nil, testutil.AuthHeader(g.token))
		require.Equal(t, http.StatusOK, resp.StatusCode)
		return []byte(readBody(t, resp))
	}
	check := func(t *testing.T, raw any) {
		config, text := featureConfig(t, raw)
		assert.ElementsMatch(t, []string{"username", "hashed_password"}, configKeys(config), "no password key, ever")
		assert.NotContains(t, text, gatePassword)
		assert.Contains(t, text, "hashed_password", "the hash is what a read shows")
	}

	t.Run("the features list", func(t *testing.T) {
		var features []map[string]any
		require.NoError(t, json.Unmarshal(read(t, g.featuresURL), &features))
		require.Len(t, features, 1)
		check(t, features[0]["config"])
	})
	t.Run("the domain read", func(t *testing.T) {
		var domain struct {
			RouteFeatures []map[string]any `json:"route_features"`
		}
		require.NoError(t, json.Unmarshal(read(t, strings.TrimSuffix(g.featuresURL, "/features")), &domain))
		require.Len(t, domain.RouteFeatures, 1)
		check(t, domain.RouteFeatures[0]["config"])
	})
}

// ⛔ The UI loads a stored config and re-saves it when anything else changes (the
// enable switch does). A supplied hash must come back exactly as it went in.
func TestRouteFeatures_ASuppliedHashIsStoredExactlyAndNeverRehashed(t *testing.T) {
	g := newGateEnv(t)
	hashBytes, err := bcrypt.GenerateFromPassword([]byte("whatever"), bcrypt.MinCost)
	require.NoError(t, err)
	hash := string(hashBytes)

	require.Equal(t, http.StatusOK, g.put(t, "basic_auth", map[string]any{"username": "alice", "hashed_password": hash}, true).StatusCode)
	config, _, _ := g.row(t)
	assert.Equal(t, hash, config["hashed_password"], "stored exactly as supplied")

	// The enable switch, as the dashboard does it: read the stored config from the
	// domain (that is where it gets route_features) and send it straight back.
	for _, enabled := range []bool{false, true, false} {
		resp := env.DoRequest(t, "GET", strings.TrimSuffix(g.featuresURL, "/features"), nil, testutil.AuthHeader(g.token))
		require.Equal(t, http.StatusOK, resp.StatusCode)
		var domain struct {
			RouteFeatures []map[string]any `json:"route_features"`
		}
		require.NoError(t, json.Unmarshal([]byte(readBody(t, resp)), &domain))
		require.Len(t, domain.RouteFeatures, 1)

		put := g.put(t, "basic_auth", domain.RouteFeatures[0]["config"].(map[string]any), enabled)
		require.Equal(t, http.StatusOK, put.StatusCode, readBody(t, put))

		config, _, gotEnabled := g.row(t)
		assert.Equal(t, hash, config["hashed_password"], "re-saving a stored config must not touch the hash")
		assert.Equal(t, enabled, gotEnabled)
		cost, err := bcrypt.Cost([]byte(config["hashed_password"].(string)))
		require.NoError(t, err)
		assert.Equal(t, bcrypt.MinCost, cost, "a re-hash would have come back at the work factor")
	}
}

// ⛔ A row saved before this change may hold something that is not bcrypt-shaped
// (the old docs said the password "is stored hashed", which invited a plaintext).
// The shape check is for what comes IN: re-saving the row as it is, which is all
// the enable switch does, must keep working, or the operator could not even turn
// a broken gate off. The API would now refuse to create such a row, so it is
// seeded directly.
func TestRouteFeatures_AnExistingRowIsNeverRejudgedWhenResavedUnchanged(t *testing.T) {
	g := newGateEnv(t)
	_, err := env.Pool.Exec(context.Background(),
		`INSERT INTO domain_route_features (domain_id, feature_type, config, enabled) VALUES ($1::uuid, 'basic_auth', '{"username":"alice","hashed_password":"x"}', true)`, g.domainID)
	require.NoError(t, err)

	// Unchanged: accepted, both ways round.
	for _, enabled := range []bool{false, true} {
		resp := g.put(t, "basic_auth", map[string]any{"username": "alice", "hashed_password": "x"}, enabled)
		body := readBody(t, resp)
		require.Equal(t, http.StatusOK, resp.StatusCode, body)
		config, _, gotEnabled := g.row(t)
		assert.Equal(t, "x", config["hashed_password"])
		assert.Equal(t, enabled, gotEnabled)
	}

	// A NEW value of the same kind is judged: that is the mistake being caught.
	resp := g.put(t, "basic_auth", map[string]any{"username": "alice", "hashed_password": "y"}, true)
	body := readBody(t, resp)
	require.Equal(t, http.StatusBadRequest, resp.StatusCode, body)
	assert.Contains(t, body, "bcrypt hash")
	config, _, _ := g.row(t)
	assert.Equal(t, "x", config["hashed_password"], "a refused write leaves the stored row alone")
}

// Both present: the password wins. The edit flow always shows the stored hash, so
// changing a password means sending both, and refusing that would break the
// feature exactly when it is needed.
func TestRouteFeatures_PasswordWinsWhenBothAreSent(t *testing.T) {
	g := newGateEnv(t)
	oldHash, err := bcrypt.GenerateFromPassword([]byte("the old password"), bcrypt.MinCost)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, g.put(t, "basic_auth", map[string]any{"username": "alice", "hashed_password": string(oldHash)}, true).StatusCode)

	resp := g.put(t, "basic_auth", map[string]any{"username": "alice", "hashed_password": string(oldHash), "password": gatePassword}, true)
	require.Equal(t, http.StatusOK, resp.StatusCode, readBody(t, resp))

	config, _, _ := g.row(t)
	hash := config["hashed_password"].(string)
	assert.NotEqual(t, string(oldHash), hash)
	assert.NoError(t, bcrypt.CompareHashAndPassword([]byte(hash), []byte(gatePassword)))
	assert.Error(t, bcrypt.CompareHashAndPassword([]byte(hash), []byte("the old password")))
	assert.ElementsMatch(t, []string{"username", "hashed_password"}, configKeys(config))
}

func TestRouteFeatures_BasicAuthRefusesWhatCannotBeAGate(t *testing.T) {
	g := newGateEnv(t)

	for name, tc := range map[string]struct {
		config map[string]any
		want   string
	}{
		"neither password nor hash":         {map[string]any{"username": "alice"}, "username and password are required"},
		"the dashboard's untouched example": {map[string]any{"username": "", "password": ""}, "username and password are required"},
		"no username":                       {map[string]any{"password": gatePassword}, "username and password are required"},
		"a plaintext pasted as the hash":    {map[string]any{"username": "alice", "hashed_password": gatePassword}, "bcrypt hash"},
		"a truncated hash":                  {map[string]any{"username": "alice", "hashed_password": "$2a$12$abcdefghijklmnopqrstuv"}, "bcrypt hash"},
		"a password over 72 bytes":          {map[string]any{"username": "alice", "password": strings.Repeat("a", 73)}, "at most 72 bytes"},
		"an unknown key":                    {map[string]any{"username": "alice", "passwd": gatePassword}, "unknown field"},
	} {
		t.Run(name, func(t *testing.T) {
			resp := g.put(t, "basic_auth", tc.config, true)
			body := readBody(t, resp)
			require.Equal(t, http.StatusBadRequest, resp.StatusCode, body)
			assert.Contains(t, body, tc.want)
			// A mis-pasted password is exactly what could be in hashed_password.
			assert.NotContains(t, body, gatePassword, "an error must never repeat what was sent")

			var stored int
			require.NoError(t, env.Pool.QueryRow(context.Background(),
				`SELECT count(*) FROM domain_route_features WHERE domain_id = $1::uuid`, g.domainID).Scan(&stored))
			assert.Zero(t, stored, "a refused write must store nothing")
		})
	}
}

// Only basic_auth is rewritten. Every other type is stored exactly as before: a
// redirect with no status_code must not suddenly gain a 301.
func TestRouteFeatures_OtherTypesAreStoredAsSent(t *testing.T) {
	g := newGateEnv(t)

	resp := g.put(t, "redirect", map[string]any{"from": "/old", "to": "/new"}, true)
	require.Equal(t, http.StatusOK, resp.StatusCode, readBody(t, resp))

	var config string
	require.NoError(t, env.Pool.QueryRow(context.Background(),
		`SELECT config::text FROM domain_route_features WHERE domain_id = $1::uuid AND feature_type = 'redirect'`, g.domainID).Scan(&config))
	var m map[string]any
	require.NoError(t, json.Unmarshal([]byte(config), &m))
	assert.ElementsMatch(t, []string{"from", "to"}, configKeys(m), "no defaulted field written back")
}
