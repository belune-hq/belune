package handler_test

import (
	"crypto/rand"
	"crypto/rsa"
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

// uploadCertificate mints a self-signed certificate covering the given SANs and
// stores it through the real admin endpoint, returning its id.
func uploadCertificate(t *testing.T, adminToken, name string, sans ...string) string {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)

	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: sans[0]},
		DNSNames:     sans,
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	require.NoError(t, err)

	resp := env.DoRequest(t, "POST", "/api/certificates", map[string]any{
		"name":     name,
		"cert_pem": string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})),
		"key_pem":  string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})),
	}, testutil.AuthHeader(adminToken))
	require.Equal(t, http.StatusCreated, resp.StatusCode)
	return extractID(testutil.ReadJSON(t, resp)["id"])
}

func usableCertNames(t *testing.T, token, hostname string) []string {
	t.Helper()
	resp := env.DoRequest(t, "GET", "/api/certificates/usable?hostname="+hostname, nil, testutil.AuthHeader(token))
	require.Equal(t, http.StatusOK, resp.StatusCode, "GET /api/certificates/usable?hostname=%s", hostname)
	defer resp.Body.Close()

	out := []string{}
	for _, row := range testutil.ReadJSONArray(t, resp) {
		out = append(out, fmt.Sprint(row.(map[string]any)["name"]))
	}
	return out
}

// TestUsableCertificates_MemberCanFindWithoutListing is the point of the route.
// A member may attach a certificate (domain routes are project-scoped) and
// ssl_mode=custom REQUIRES a certificate_id, but GET /api/certificates is
// admin-only — so before this they could configure custom TLS only if an admin
// read them a uuid, and the add-domain form rendered an empty picker that
// refused to submit.
func TestUsableCertificates_MemberCanFindWithoutListing(t *testing.T) {
	resetDB(t)
	adminToken := env.SetupAdmin(t, "admin@test.com", "password123")
	_, memberToken := createMember(t, adminToken, "member@test.com")

	uploadCertificate(t, adminToken, "Shop Exact", "shop.example.com")
	uploadCertificate(t, adminToken, "Example Wildcard", "*.example.com")
	uploadCertificate(t, adminToken, "Unrelated", "other.test")

	// The listing stays shut. If this ever returns 200 the route below has
	// stopped being the only way a member reaches certificate metadata.
	resp := env.DoRequest(t, "GET", "/api/certificates", nil, testutil.AuthHeader(memberToken))
	assert.Equal(t, http.StatusForbidden, resp.StatusCode, "the full listing must stay admin-only")
	resp.Body.Close()

	// Exact and wildcard both answer for a host one label deep.
	assert.ElementsMatch(t, []string{"Shop Exact", "Example Wildcard"},
		usableCertNames(t, memberToken, "shop.example.com"))

	// Only the wildcard answers for a sibling host.
	assert.Equal(t, []string{"Example Wildcard"},
		usableCertNames(t, memberToken, "blog.example.com"))

	// ⚠️ A wildcard covers ONE label. Nothing should answer for a host two deep,
	// or the picker would offer a certificate that cannot actually serve it.
	assert.Empty(t, usableCertNames(t, memberToken, "a.b.example.com"),
		"*.example.com must not be offered for a two-label host")

	// Nor for the apex, which a wildcard also never covers.
	assert.Equal(t, []string{}, usableCertNames(t, memberToken, "example.com"),
		"*.example.com must not be offered for the apex")

	assert.Empty(t, usableCertNames(t, memberToken, "nothing.invalid"))
}

// TestUsableCertificates_DoesNotLeakOtherSubjects guards the reason the full
// listing is admin-only in the first place. A certificate carries every SAN it
// was issued for, so echoing the subjects array would tell a member the
// hostnames of every other application the certificate also covers — turning
// the fix for one gap into the leak the gap existed to prevent.
func TestUsableCertificates_DoesNotLeakOtherSubjects(t *testing.T) {
	resetDB(t)
	adminToken := env.SetupAdmin(t, "admin@test.com", "password123")
	_, memberToken := createMember(t, adminToken, "member@test.com")

	uploadCertificate(t, adminToken, "Shared SAN Cert",
		"mine.example.com", "someone-elses-secret-project.example.com", "finance-internal.example.com")

	resp := env.DoRequest(t, "GET", "/api/certificates/usable?hostname=mine.example.com",
		nil, testutil.AuthHeader(memberToken))
	require.Equal(t, http.StatusOK, resp.StatusCode)
	defer resp.Body.Close()

	rows := testutil.ReadJSONArray(t, resp)
	require.Len(t, rows, 1)
	row := rows[0].(map[string]any)

	// What they need to choose and to judge currency.
	assert.Equal(t, "Shared SAN Cert", row["name"])
	assert.Contains(t, row, "not_after")

	// What they must never receive. Asserting on the KEY, not on a substring of
	// the body: a future field that happens to embed a SAN would slip past a
	// text search, and "subjects" is the field whose absence is the contract.
	assert.NotContains(t, row, "subjects",
		"the subjects array names every host the certificate covers — it must not reach a non-admin")
	assert.NotContains(t, row, "domain_count",
		"how widely a certificate is used across the install is not a member's business")
	assert.NotContains(t, row, "cert_pem_encrypted")
	assert.NotContains(t, row, "key_pem_encrypted")
}

// TestUsableCertificates_RequiresHostname keeps the route from degrading into
// the admin listing with the SANs filed off: with no hostname there is no
// "already supplied" for the answer to be bounded by.
func TestUsableCertificates_RequiresHostname(t *testing.T) {
	resetDB(t)
	adminToken := env.SetupAdmin(t, "admin@test.com", "password123")
	_, memberToken := createMember(t, adminToken, "member@test.com")
	uploadCertificate(t, adminToken, "Something", "any.example.com")

	for _, path := range []string{"/api/certificates/usable", "/api/certificates/usable?hostname=", "/api/certificates/usable?hostname=%20"} {
		resp := env.DoRequest(t, "GET", path, nil, testutil.AuthHeader(memberToken))
		assert.Equal(t, http.StatusBadRequest, resp.StatusCode, "%s must be rejected, not answered", path)
		resp.Body.Close()
	}
}
