package handler_test

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/belune-hq/belune/internal/testutil"
)

// testCertPair mints a self-signed PEM pair for the given hostname.
func testCertPair(t *testing.T, hostname string) (certPEM, keyPEM string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)

	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: hostname},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		DNSNames:     []string{hostname},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	require.NoError(t, err)
	keyDER, err := x509.MarshalECPrivateKey(key)
	require.NoError(t, err)

	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})),
		string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}))
}

func TestUploadCertificate(t *testing.T) {
	resetDB(t)
	token := env.SetupAdmin(t, "admin@test.com", "password123")
	certPEM, keyPEM := testCertPair(t, "app.example.com")

	resp := env.DoRequest(t, "POST", "/api/certificates", map[string]any{
		"name": "example-wildcard", "cert_pem": certPEM, "key_pem": keyPEM,
	}, testutil.AuthHeader(token))
	require.Equal(t, http.StatusCreated, resp.StatusCode)

	result := testutil.ReadJSON(t, resp)
	assert.Equal(t, "example-wildcard", result["name"])
	assert.Contains(t, result["subjects"], "app.example.com")
	assert.NotEmpty(t, result["not_after"])

	// The private key must never come back out of the API.
	assert.NotContains(t, fmt.Sprint(result), "PRIVATE KEY")

	list := env.DoRequest(t, "GET", "/api/certificates", nil, testutil.AuthHeader(token))
	require.Equal(t, http.StatusOK, list.StatusCode)
	certs := testutil.ReadJSONArray(t, list)
	require.Len(t, certs, 1)
	assert.EqualValues(t, 0, certs[0].(map[string]any)["domain_count"])
}

func TestUploadCertificate_RejectsMismatchedKey(t *testing.T) {
	resetDB(t)
	token := env.SetupAdmin(t, "admin@test.com", "password123")
	certPEM, _ := testCertPair(t, "app.example.com")
	_, otherKey := testCertPair(t, "other.example.com")

	resp := env.DoRequest(t, "POST", "/api/certificates", map[string]any{
		"name": "mismatched", "cert_pem": certPEM, "key_pem": otherKey,
	}, testutil.AuthHeader(token))
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

func TestUploadCertificate_RejectsNonPEM(t *testing.T) {
	resetDB(t)
	token := env.SetupAdmin(t, "admin@test.com", "password123")

	// The mistake the old cert_path field invited: pasting a path, not a PEM.
	resp := env.DoRequest(t, "POST", "/api/certificates", map[string]any{
		"name": "path-not-pem", "cert_pem": "/etc/ssl/app.crt", "key_pem": "/etc/ssl/app.key",
	}, testutil.AuthHeader(token))
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

func TestDeleteCertificate_ConflictWhileInUse(t *testing.T) {
	resetDB(t)
	token := env.SetupAdmin(t, "admin@test.com", "password123")
	certPEM, keyPEM := testCertPair(t, "custom.example.com")

	created := testutil.ReadJSON(t, env.DoRequest(t, "POST", "/api/certificates", map[string]any{
		"name": "in-use", "cert_pem": certPEM, "key_pem": keyPEM,
	}, testutil.AuthHeader(token)))
	certID := extractID(created["id"])

	project := env.CreateProject(t, token, "Cert Project", "cert-project")
	projectID := extractID(project["id"])
	app := env.CreateApplication(t, token, projectID, map[string]any{
		"name": "Cert App", "type": "git", "build_type": "dockerfile", "source_repo": "https://github.com/test/repo",
	})
	appID := extractID(app["id"])

	domain := env.DoRequest(t, "POST", fmt.Sprintf("/api/projects/%s/applications/%s/domains", projectID, appID), map[string]any{
		"hostname": "custom.example.com", "ssl_enabled": true,
		"ssl_mode": "custom", "certificate_id": certID,
	}, testutil.AuthHeader(token))
	require.Equal(t, http.StatusCreated, domain.StatusCode)

	// The certificate is serving a live domain, so deleting it would silently
	// break TLS for that hostname.
	del := env.DoRequest(t, "DELETE", "/api/certificates/"+certID, nil, testutil.AuthHeader(token))
	assert.Equal(t, http.StatusConflict, del.StatusCode)

	// Once the domain is gone, the delete goes through.
	domainID := extractID(testutil.ReadJSON(t, domain)["id"])
	require.Equal(t, http.StatusOK,
		env.DoRequest(t, "DELETE", fmt.Sprintf("/api/projects/%s/applications/%s/domains/%s", projectID, appID, domainID), nil, testutil.AuthHeader(token)).StatusCode)

	del = env.DoRequest(t, "DELETE", "/api/certificates/"+certID, nil, testutil.AuthHeader(token))
	assert.Equal(t, http.StatusNoContent, del.StatusCode)
}

func TestAddDomain_CustomModeRequiresCertificate(t *testing.T) {
	resetDB(t)
	token := env.SetupAdmin(t, "admin@test.com", "password123")
	project := env.CreateProject(t, token, "Cert Project", "cert-project")
	projectID := extractID(project["id"])
	app := env.CreateApplication(t, token, projectID, map[string]any{
		"name": "Cert App", "type": "git", "build_type": "dockerfile", "source_repo": "https://github.com/test/repo",
	})
	appID := extractID(app["id"])

	resp := env.DoRequest(t, "POST", fmt.Sprintf("/api/projects/%s/applications/%s/domains", projectID, appID), map[string]any{
		"hostname": "nocert.example.com", "ssl_enabled": true, "ssl_mode": "custom",
	}, testutil.AuthHeader(token))
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

func TestCertificates_RequireAdmin(t *testing.T) {
	resetDB(t)
	adminToken := env.SetupAdmin(t, "admin@test.com", "password123")

	env.DoRequest(t, "POST", "/api/users", map[string]any{
		"email": "member@test.com", "password": "password123", "role": "member",
	}, testutil.AuthHeader(adminToken)).Body.Close()
	memberToken := env.LoginAs(t, "member@test.com", "password123")

	resp := env.DoRequest(t, "GET", "/api/certificates", nil, testutil.AuthHeader(memberToken))
	assert.Equal(t, http.StatusForbidden, resp.StatusCode)
}
