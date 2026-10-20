package worker_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/belune-hq/belune/internal/config"
	"github.com/belune-hq/belune/internal/pkg/crypto"
	"github.com/belune-hq/belune/internal/proxy"
	"github.com/belune-hq/belune/internal/proxy/caddy"
	"github.com/belune-hq/belune/internal/store/generated"
	"github.com/belune-hq/belune/internal/testutil"
	"github.com/belune-hq/belune/internal/tlsstatus"
	"github.com/belune-hq/belune/internal/worker"
)

// The full Phase 3 loop against a real Caddy and a real database: a certificate
// is served, the probe observes it on the wire, and the domain's badge reflects
// what a browser would actually be handed.
//
//	BELUNE_CADDY_INTEGRATION=1 go test -p 1 ./internal/worker/ -run RealStackTLS -v

const probeHost = "phase3-probe.belune.local"

func liveCaddyClient(t *testing.T) *caddy.Client {
	t.Helper()
	if os.Getenv("BELUNE_CADDY_INTEGRATION") == "" {
		t.Skip("set BELUNE_CADDY_INTEGRATION=1 to run against a real Caddy")
	}
	adminURL := os.Getenv("CADDY_ADMIN_URL")
	if adminURL == "" {
		adminURL = "http://localhost:2019"
	}
	c := caddy.New(adminURL)
	require.NoError(t, c.Ping(context.Background()), "caddy admin API not reachable — is `task dev:infra` up?")
	return c
}

func probeHandler(t *testing.T) *worker.TaskHandler {
	t.Helper()
	keyring, err := crypto.ParseKeyringEnv("", "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef", "")
	require.NoError(t, err)

	addr := os.Getenv("CADDY_HTTPS_ADDR")
	if addr == "" {
		addr = "localhost:443"
	}
	return &worker.TaskHandler{
		Queries: testQueries,
		DB:      testPool,
		Keyring: keyring,
		Config:  &config.Config{CaddyTLSProbeAddr: addr},
	}
}

// seedTLSDomain inserts a domain (with the app chain it needs) in the given mode.
func seedTLSDomain(t *testing.T, ctx context.Context, sslMode string, certID pgtype.UUID) generated.Domain {
	t.Helper()
	app, _ := seedApp(t)

	domain, err := testQueries.CreateDomain(ctx, generated.CreateDomainParams{
		Path:          "/",
		ApplicationID: app.ID,
		Hostname:      probeHost,
		SslEnabled:    sslMode != proxy.SSLModeOff,
		SslMode:       sslMode,
		CertificateID: certID,
		ContainerPort: pgtype.Int4{Int32: 8080, Valid: true},
	})
	require.NoError(t, err)
	return domain
}

// TestRealStackTLS_ProbeReportsServedCertificate is the core promise of the
// status pipeline: what the badge says is what the wire says.
func TestRealStackTLS_ProbeReportsServedCertificate(t *testing.T) {
	client := liveCaddyClient(t)
	ctx := context.Background()
	t.Cleanup(func() {
		_ = testutil.TruncateAll(ctx, testPool)
		_ = client.RemoveRoute(ctx, probeHost, "/")
		_ = client.SyncCertificates(ctx, nil)
	})
	client.InitCatchAll(ctx)

	h := probeHandler(t)

	// Serve a real certificate for the hostname through Caddy.
	certPEM, keyPEM := probeCertPair(t, probeHost)
	certEnc, err := h.Keyring.Encrypt([]byte(certPEM))
	require.NoError(t, err)
	keyEnc, err := h.Keyring.Encrypt([]byte(keyPEM))
	require.NoError(t, err)
	cert, err := testQueries.CreateCertificate(ctx, generated.CreateCertificateParams{
		Name:             "probe-cert",
		CertPemEncrypted: certEnc,
		KeyPemEncrypted:  keyEnc,
		Subjects:         []string{probeHost},
	})
	require.NoError(t, err)

	domain := seedTLSDomain(t, ctx, proxy.SSLModeCustom, cert.ID)

	cfg := proxy.RouteConfig{
		Hostname: probeHost, TargetURL: "http://127.0.0.1:9", TLS: true,
		SSLMode: proxy.SSLModeCustom, CertPEM: certPEM, KeyPEM: keyPEM,
	}
	require.NoError(t, client.AddRoute(ctx, cfg))

	h.HandleTLSStatusSweep(ctx)

	got, err := testQueries.GetDomain(ctx, domain.ID)
	require.NoError(t, err)
	assert.Equal(t, worker.TLSStatusActive, got.TlsStatus,
		"the probe should report what Caddy actually serves")
	assert.Contains(t, got.TlsIssuer.String, "belune-probe-ca")
	assert.True(t, got.TlsNotAfter.Valid)
	assert.True(t, got.TlsLastCheckedAt.Valid)
	assert.Empty(t, got.TlsError.String)
}

// TestRealStackTLS_PendingWithoutCertificate covers the case the whole feature
// exists for: a domain configured for HTTPS that has no certificate. It must not
// look "active", and once Caddy tells us why, it must say so.
func TestRealStackTLS_PendingWithoutCertificate(t *testing.T) {
	client := liveCaddyClient(t)
	ctx := context.Background()
	t.Cleanup(func() {
		_ = testutil.TruncateAll(ctx, testPool)
		_ = client.RemoveRoute(ctx, probeHost, "/")
	})
	client.InitCatchAll(ctx)

	h := probeHandler(t)
	domain := seedTLSDomain(t, ctx, "automatic", pgtype.UUID{})

	// No route, no certificate: nothing is served for this SNI.
	h.HandleTLSStatusSweep(ctx)

	got, err := testQueries.GetDomain(ctx, domain.ID)
	require.NoError(t, err)
	assert.Equal(t, worker.TLSStatusPending, got.TlsStatus)

	// Now Caddy reports an ACME failure for it — exactly the line shape a real
	// failing issuance emits. The reason must reach the domain, and the status
	// must stop pretending the certificate is merely "on its way".
	recorder := tlsstatus.NewRecorder(testQueries)
	recorder.HandleCaddyLine(ctx, `{"level":"error","logger":"tls.obtain","msg":"could not get certificate from issuer","identifier":"`+probeHost+`","error":"HTTP 429 urn:ietf:params:acme:error:rateLimited - too many certificates already issued"}`)

	got, err = testQueries.GetDomain(ctx, domain.ID)
	require.NoError(t, err)
	assert.Equal(t, worker.TLSStatusFailed, got.TlsStatus)
	// ⚠️ The CA's sentence, NOT the ACME problem URN. cleanACMEReason strips
	// everything up to and including "urn:…:error:rateLimited - " on purpose
	// (4a228d8) because the wrapping is not what an operator can act on. This
	// assertion asked for the wrapping, and had done since the day before that
	// landed — it went unnoticed for three months because this suite is gated
	// behind BELUNE_CADDY_INTEGRATION and runs only when someone remembers.
	assert.Contains(t, got.TlsError.String, "too many certificates already issued")
	assert.NotContains(t, got.TlsError.String, "urn:ietf:params:acme:error",
		"the protocol wrapping is meant to be stripped, not shown")

	// And the next sweep keeps the reason rather than resetting to a bare pending.
	h.HandleTLSStatusSweep(ctx)
	got, err = testQueries.GetDomain(ctx, domain.ID)
	require.NoError(t, err)
	assert.Equal(t, worker.TLSStatusFailed, got.TlsStatus)
	assert.Contains(t, got.TlsError.String, "too many certificates already issued")
}

func probeCertPair(t *testing.T, hostname string) (certPEM, keyPEM string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(21),
		Subject:      pkix.Name{CommonName: "belune-probe-ca"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(90 * 24 * time.Hour),
		DNSNames:     []string{hostname},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	require.NoError(t, err)
	keyDER, err := x509.MarshalECPrivateKey(key)
	require.NoError(t, err)
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})),
		string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}))
}
