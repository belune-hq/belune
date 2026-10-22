package handler_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/belune-hq/belune/internal/proxy/capability"
	"github.com/belune-hq/belune/internal/testutil"
)

const testDNSToken = "cf-token-VERY-SECRET-0123456789abcdef0123"

func createDNSProvider(t *testing.T, token string) string {
	t.Helper()
	resp := env.DoRequest(t, "POST", "/api/dns-providers", map[string]any{
		"name": "cf", "provider": "cloudflare", "credentials": map[string]string{"api_token": testDNSToken},
	}, testutil.AuthHeader(token))
	require.Equal(t, http.StatusCreated, resp.StatusCode)
	return extractID(testutil.ReadJSON(t, resp)["id"])
}

// The stored value must be ciphertext, nothing may return it, and the audit log
// may name the fields but never carry the token.
func TestDNSProviders_CredentialsNeverLeave(t *testing.T) {
	resetDB(t)
	token := env.SetupAdmin(t, "dns-admin@test.com", "password123")
	// The shared harness has no audit service; this server shares the database.
	url := auditedServerURL(t)
	resp := postRaw(t, url+"/api/dns-providers", token, map[string]any{
		"name": "cf", "provider": "cloudflare", "credentials": map[string]string{"api_token": testDNSToken},
	})
	id := extractID(testutil.ReadJSON(t, resp)["id"])

	var enc []byte
	require.NoError(t, env.Pool.QueryRow(context.Background(),
		`SELECT credentials_encrypted FROM dns_providers WHERE id = $1`, id).Scan(&enc))
	assert.NotContains(t, string(enc), testDNSToken, "stored credentials must be encrypted")

	for _, path := range []string{"/api/dns-providers"} {
		resp := env.DoRequest(t, "GET", path, nil, testutil.AuthHeader(token))
		require.Equal(t, http.StatusOK, resp.StatusCode)
		rows := testutil.ReadJSONArray(t, resp)
		require.Len(t, rows, 1)
		assert.NotContains(t, fmt.Sprint(rows[0]), testDNSToken)
		assert.NotContains(t, fmt.Sprint(rows[0]), "credentials")
	}

	var details string
	require.Eventually(t, func() bool {
		return env.Pool.QueryRow(context.Background(),
			`SELECT details::text FROM audit_logs WHERE action = 'create_dns_provider'`).Scan(&details) == nil
	}, 3*time.Second, 25*time.Millisecond)
	assert.Contains(t, details, "api_token", "the audit records which fields were set")
	assert.NotContains(t, details, testDNSToken, "and never what they were set to")
}

func TestDNSProviders_ValidationAndRotation(t *testing.T) {
	resetDB(t)
	token := env.SetupAdmin(t, "dns-admin2@test.com", "password123")

	bad := env.DoRequest(t, "POST", "/api/dns-providers", map[string]any{
		"name": "x", "provider": "cloudflare", "credentials": map[string]string{"apitoken": "t"},
	}, testutil.AuthHeader(token))
	assert.Equal(t, http.StatusBadRequest, bad.StatusCode, "an unknown credential key must be refused, not stored")

	id := createDNSProvider(t, token)
	var before []byte
	require.NoError(t, env.Pool.QueryRow(context.Background(), `SELECT credentials_encrypted FROM dns_providers WHERE id=$1`, id).Scan(&before))

	// A rename with no credentials keeps the secret.
	resp := env.DoRequest(t, "PUT", "/api/dns-providers/"+id, map[string]any{"name": "renamed"}, testutil.AuthHeader(token))
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var after []byte
	require.NoError(t, env.Pool.QueryRow(context.Background(), `SELECT credentials_encrypted FROM dns_providers WHERE id=$1`, id).Scan(&after))
	assert.Equal(t, before, after)

	// Rotation replaces it.
	resp = env.DoRequest(t, "PUT", "/api/dns-providers/"+id, map[string]any{
		"name": "renamed", "credentials": map[string]string{"api_token": "rotated-token"},
	}, testutil.AuthHeader(token))
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.NoError(t, env.Pool.QueryRow(context.Background(), `SELECT credentials_encrypted FROM dns_providers WHERE id=$1`, id).Scan(&after))
	assert.NotEqual(t, before, after)

	// The type is fixed.
	resp = env.DoRequest(t, "PUT", "/api/dns-providers/"+id, map[string]any{"name": "n", "provider": "route53"}, testutil.AuthHeader(token))
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

func TestDNSProviders_MembersCannotTouchThem(t *testing.T) {
	resetDB(t)
	adminToken := env.SetupAdmin(t, "dns-admin3@test.com", "password123")
	_, memberToken := createMember(t, adminToken, "dns-member@test.com")
	id := createDNSProvider(t, adminToken)

	for _, c := range []struct{ method, path string }{
		{"GET", "/api/dns-providers"},
		{"POST", "/api/dns-providers"},
		{"PUT", "/api/dns-providers/" + id},
		{"DELETE", "/api/dns-providers/" + id},
	} {
		resp := env.DoRequest(t, c.method, c.path, map[string]any{"name": "x"}, testutil.AuthHeader(memberToken))
		assert.Equal(t, http.StatusForbidden, resp.StatusCode, "%s %s", c.method, c.path)
	}
}

// dnsDomainFixture: an admin's project, shared with a member, one application.
func dnsDomainFixture(t *testing.T) (adminToken, memberToken, projectID, appID, providerID string) {
	t.Helper()
	resetDB(t)
	adminToken = env.SetupAdmin(t, "dnsd-admin@test.com", "password123")
	_, memberToken = createMember(t, adminToken, "dnsd-member@test.com")
	projectID = extractID(env.CreateProject(t, adminToken, "P", "p")["id"])
	resp := env.DoRequest(t, "PUT", "/api/projects/"+projectID+"/sharing", map[string]any{"shared": true}, testutil.AuthHeader(adminToken))
	_ = resp.Body.Close()
	app := env.CreateApplication(t, adminToken, projectID, map[string]any{
		"name": "A", "type": "git", "build_type": "railpack", "source_repo": "https://github.com/test/repo", "branch": "main",
	})
	appID = extractID(app["id"])
	providerID = createDNSProvider(t, adminToken)
	env.Capabilities.Set(capability.NewSet("dns.providers.cloudflare", "http.handlers.reverse_proxy"))
	t.Cleanup(func() { env.Capabilities.Set(capability.Set{}) })
	return
}

func domainsPath(projectID, appID string) string {
	return fmt.Sprintf("/api/projects/%s/applications/%s/domains", projectID, appID)
}

func addDNSDomain(t *testing.T, token, projectID, appID, host, providerID string) *http.Response {
	t.Helper()
	return env.DoRequest(t, "POST", domainsPath(projectID, appID), map[string]any{
		"hostname": host, "ssl_enabled": true, "ssl_mode": "dns_challenge", "dns_provider_id": providerID,
	}, testutil.AuthHeader(token))
}

func TestDomains_DNSChallengeAdmission(t *testing.T) {
	adminToken, memberToken, projectID, appID, providerID := dnsDomainFixture(t)

	t.Run("admin with a capable proxy and a provider", func(t *testing.T) {
		resp := addDNSDomain(t, adminToken, projectID, appID, "*.example.com", providerID)
		require.Equal(t, http.StatusCreated, resp.StatusCode)
		resp.Body.Close()
	})

	t.Run("member is refused", func(t *testing.T) {
		resp := addDNSDomain(t, memberToken, projectID, appID, "m.example.com", providerID)
		assert.Equal(t, http.StatusForbidden, resp.StatusCode)
	})

	t.Run("provider is required", func(t *testing.T) {
		resp := addDNSDomain(t, adminToken, projectID, appID, "np.example.com", "")
		assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
	})

	t.Run("a wildcard outside dns_challenge is refused", func(t *testing.T) {
		resp := env.DoRequest(t, "POST", domainsPath(projectID, appID),
			map[string]any{"hostname": "*.other.com", "ssl_enabled": true}, testutil.AuthHeader(adminToken))
		assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
	})

	t.Run("a proxy without the module is refused and says which", func(t *testing.T) {
		env.Capabilities.Set(capability.NewSet("http.handlers.reverse_proxy"))
		resp := addDNSDomain(t, adminToken, projectID, appID, "nomod.example.com", providerID)
		require.Equal(t, http.StatusBadRequest, resp.StatusCode)
		assert.Contains(t, testutil.ReadJSON(t, resp)["error"], "dns.providers.cloudflare")
	})

	t.Run("a proxy we could not ask is not blamed", func(t *testing.T) {
		env.Capabilities.Set(capability.Set{})
		resp := addDNSDomain(t, adminToken, projectID, appID, "unk.example.com", providerID)
		require.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)
		assert.NotContains(t, testutil.ReadJSON(t, resp)["error"], "does not include")
	})
}

// The escalation a peer review found: renaming an admin's dns_challenge domain to
// a wildcard would issue the whole zone with the admin's credential. Asserts who
// is let THROUGH, not just that the admin can.
func TestUpdateDomain_MemberCannotRenameDNSChallengeDomain(t *testing.T) {
	adminToken, memberToken, projectID, appID, providerID := dnsDomainFixture(t)
	resp := addDNSDomain(t, adminToken, projectID, appID, "app.example.com", providerID)
	require.Equal(t, http.StatusCreated, resp.StatusCode)
	domainID := extractID(testutil.ReadJSON(t, resp)["id"])
	path := domainsPath(projectID, appID) + "/" + domainID

	resp = env.DoRequest(t, "PUT", path, map[string]any{"hostname": "*.example.com"}, testutil.AuthHeader(memberToken))
	assert.Equal(t, http.StatusForbidden, resp.StatusCode, "a member must not widen the names a DNS credential issues")
	var host string
	require.NoError(t, env.Pool.QueryRow(context.Background(), `SELECT hostname FROM domains WHERE id=$1`, domainID).Scan(&host))
	assert.Equal(t, "app.example.com", host)

	// A benign re-save by the member (same hostname, e.g. a port edit) still works,
	// even with the proxy unprobeable: only a CHANGE is gated.
	env.Capabilities.Set(capability.Set{})
	resp = env.DoRequest(t, "PUT", path, map[string]any{"hostname": "app.example.com", "container_port": 3000}, testutil.AuthHeader(memberToken))
	assert.Equal(t, http.StatusOK, resp.StatusCode)

	// The admin can rename, once the proxy can be asked.
	env.Capabilities.Set(capability.NewSet("dns.providers.cloudflare"))
	resp = env.DoRequest(t, "PUT", path, map[string]any{"hostname": "*.example.com"}, testutil.AuthHeader(adminToken))
	assert.Equal(t, http.StatusOK, resp.StatusCode)
}

// UpdateDomain writes every column; an omitting PUT must carry all of them.
func TestUpdateDomain_OmittingPutKeepsEveryColumn(t *testing.T) {
	adminToken, _, projectID, appID, providerID := dnsDomainFixture(t)
	resp := addDNSDomain(t, adminToken, projectID, appID, "keep.example.com", providerID)
	require.Equal(t, http.StatusCreated, resp.StatusCode)
	domainID := extractID(testutil.ReadJSON(t, resp)["id"])
	_, err := env.Pool.Exec(context.Background(), `
		UPDATE domains SET ssl_provider='cloudflare', ssl_credentials_encrypted='\xdeadbeef',
		       advanced_config='{"a":1}', ssl_enabled=true, force_https=true WHERE id=$1`, domainID)
	require.NoError(t, err)

	read := func() map[string]any {
		row := map[string]any{}
		var provider, sslMode string
		var creds, adv []byte
		var enabled, force bool
		var dns pgtype.UUID
		require.NoError(t, env.Pool.QueryRow(context.Background(), `
			SELECT ssl_mode, COALESCE(ssl_provider,''), ssl_credentials_encrypted, advanced_config, ssl_enabled, force_https, dns_provider_id
			  FROM domains WHERE id=$1`, domainID).Scan(&sslMode, &provider, &creds, &adv, &enabled, &force, &dns))
		row["mode"], row["provider"], row["creds"], row["adv"] = sslMode, provider, string(creds), strings.ReplaceAll(string(adv), " ", "")
		row["enabled"], row["force"], row["dns"] = enabled, force, dns
		return row
	}
	before := read()

	resp = env.DoRequest(t, "PUT", domainsPath(projectID, appID)+"/"+domainID,
		map[string]any{"hostname": "keep.example.com"}, testutil.AuthHeader(adminToken))
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, before, read())
}

func TestDNSProviders_DeleteRefusedWhileInUse(t *testing.T) {
	adminToken, _, projectID, appID, providerID := dnsDomainFixture(t)
	resp := addDNSDomain(t, adminToken, projectID, appID, "use.example.com", providerID)
	require.Equal(t, http.StatusCreated, resp.StatusCode)

	resp = env.DoRequest(t, "DELETE", "/api/dns-providers/"+providerID, nil, testutil.AuthHeader(adminToken))
	require.Equal(t, http.StatusConflict, resp.StatusCode)
	assert.Contains(t, testutil.ReadJSON(t, resp)["error"], "1 domain")
}

func postRaw(t *testing.T, url, token string, body any) *http.Response {
	t.Helper()
	raw, err := json.Marshal(body)
	require.NoError(t, err)
	req, err := http.NewRequest("POST", url, bytes.NewReader(raw))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	for k, v := range testutil.AuthHeader(token) {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	require.Equal(t, http.StatusCreated, resp.StatusCode)
	return resp
}

// A TLS-off domain has no HTTPS listener to redirect to, so what is stored must
// say so — whether the client sent true, or omitted it and the stale value would
// have been carried forward.
func TestDomains_ForceHTTPSIsFalseWhenTLSIsOff(t *testing.T) {
	adminToken, _, projectID, appID, _ := dnsDomainFixture(t)
	read := func(id string) bool {
		var f bool
		require.NoError(t, env.Pool.QueryRow(context.Background(), `SELECT force_https FROM domains WHERE id=$1`, id).Scan(&f))
		return f
	}

	resp := env.DoRequest(t, "POST", domainsPath(projectID, appID), map[string]any{
		"hostname": "off.example.com", "ssl_mode": "off", "force_https": true,
	}, testutil.AuthHeader(adminToken))
	require.Equal(t, http.StatusCreated, resp.StatusCode)
	id := extractID(testutil.ReadJSON(t, resp)["id"])
	assert.False(t, read(id), "create must not persist force_https for an off domain")

	// A legacy row holding the stale true is corrected by an omitting PUT.
	_, err := env.Pool.Exec(context.Background(), `UPDATE domains SET force_https=true WHERE id=$1`, id)
	require.NoError(t, err)
	resp = env.DoRequest(t, "PUT", domainsPath(projectID, appID)+"/"+id,
		map[string]any{"hostname": "off.example.com"}, testutil.AuthHeader(adminToken))
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.False(t, read(id))

	// Turning TLS on and asking for the redirect is honoured.
	resp = env.DoRequest(t, "PUT", domainsPath(projectID, appID)+"/"+id,
		map[string]any{"hostname": "off.example.com", "ssl_mode": "automatic", "force_https": true}, testutil.AuthHeader(adminToken))
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.True(t, read(id))
}
