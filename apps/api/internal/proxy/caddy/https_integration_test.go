package caddy

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/belune-hq/belune/internal/proxy"
)

// These tests drive a real Caddy admin API — they assert the parts of the HTTPS
// foundation that only a live Caddy can prove: that PATCHing srv0 actually
// rebinds :443, that a TLS connection policy is what makes the handshake
// complete, and that automatic_https.skip really does suppress issuance.
//
// Run against the dev stack (`task dev:infra`), which sets local_certs so Caddy
// issues from its internal CA instead of failing ACME against *.belune.local:
//
//	BELUNE_CADDY_INTEGRATION=1 go test ./internal/proxy/caddy/ -run RealCaddy -v
//
// These drive one shared Caddy, so run them serially (-p 1) alongside the other
// integration packages, or they will fight over its config.

const (
	autoHost      = "phase1-auto.belune.local"
	offHost       = "phase1-off.belune.local"
	customHost    = "phase2-custom.belune.local"
	dashboardHost = "phase5-dashboard.belune.local"
)

func liveClient(t *testing.T) *Client {
	t.Helper()
	if os.Getenv("BELUNE_CADDY_INTEGRATION") == "" {
		t.Skip("set BELUNE_CADDY_INTEGRATION=1 to run against a real Caddy admin API")
	}
	adminURL := os.Getenv("CADDY_ADMIN_URL")
	if adminURL == "" {
		adminURL = "http://localhost:2019"
	}
	c := New(adminURL)
	require.NoError(t, c.Ping(context.Background()), "caddy admin API not reachable — is `task dev:infra` up?")
	return c
}

// routeCfg builds a minimal RouteConfig; the upstream never has to answer, since
// every assertion here is about TLS and Caddy's config tree, not proxying.
func routeCfg(hostname, sslMode string) proxy.RouteConfig {
	return proxy.RouteConfig{
		Hostname:  hostname,
		TargetURL: "http://127.0.0.1:9",
		TLS:       true,
		SSLMode:   sslMode,
	}
}

func serverConfig(t *testing.T, c *Client) map[string]any {
	t.Helper()
	server, err := c.fetchServer(context.Background())
	require.NoError(t, err)
	return server
}

// leafFor completes a TLS handshake against the local Caddy with the given SNI
// and returns the leaf certificate. Issuance is asynchronous, so poll until the
// timeout — which callers asserting the *absence* of a cert keep short.
func leafFor(t *testing.T, hostname string, timeout time.Duration) (*tls.ConnectionState, error) {
	t.Helper()
	addr := os.Getenv("CADDY_HTTPS_ADDR")
	if addr == "" {
		addr = "localhost:443"
	}

	var lastErr error
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		conn, err := tls.DialWithDialer(
			&net.Dialer{Timeout: 3 * time.Second},
			"tcp", addr,
			&tls.Config{ServerName: hostname, InsecureSkipVerify: true}, //nolint:gosec // inspecting the leaf, not trusting it
		)
		if err == nil {
			state := conn.ConnectionState()
			conn.Close()
			if len(state.PeerCertificates) > 0 {
				return &state, nil
			}
			lastErr = fmt.Errorf("handshake completed with no peer certificate")
		} else {
			lastErr = err
		}
		time.Sleep(500 * time.Millisecond)
	}
	return nil, lastErr
}

// TestRealCaddy_HTTPSFoundation is the Phase 1 contract: after Belune writes its
// server config, srv0 terminates TLS on :443 and Caddy has issued a certificate
// for an ssl_mode=automatic hostname purely from the route's host matcher.
func TestRealCaddy_HTTPSFoundation(t *testing.T) {
	c := liveClient(t)
	ctx := context.Background()
	t.Cleanup(func() { _ = c.RemoveRoute(ctx, autoHost, "/") })

	c.InitCatchAll(ctx)
	require.NoError(t, c.AddRoute(ctx, routeCfg(autoHost, "automatic")))

	server := serverConfig(t, c)
	assert.Equal(t, []any{":80", ":443"}, server["listen"], "srv0 must listen on both ports")
	assert.NotEmpty(t, server["tls_connection_policies"], "without a connection policy Caddy accepts TCP on :443 but never completes a handshake")

	autoHTTPS, _ := server["automatic_https"].(map[string]any)
	require.NotNil(t, autoHTTPS)
	assert.Equal(t, true, autoHTTPS["disable_redirects"], "Belune renders its own per-domain HTTP→HTTPS redirect")

	state, err := leafFor(t, autoHost, 15*time.Second)
	require.NoError(t, err, "TLS handshake against local Caddy failed")
	leaf := state.PeerCertificates[0]
	assert.Contains(t, leaf.DNSNames, autoHost, "Caddy should have issued for the hostname in the route matcher")
	t.Logf("issued by %q, valid until %s", leaf.Issuer.CommonName, leaf.NotAfter.Format(time.RFC3339))
}

// TestRealCaddy_SSLModeOffIsSkipped proves the skip list does its job: an
// ssl_mode=off hostname gets no certificate (so Caddy never starts a doomed ACME
// order for it), and the entry is pruned again when the route goes away.
func TestRealCaddy_SSLModeOffIsSkipped(t *testing.T) {
	c := liveClient(t)
	ctx := context.Background()
	t.Cleanup(func() { _ = c.RemoveRoute(ctx, offHost, "/") })

	c.InitCatchAll(ctx)
	require.NoError(t, c.AddRoute(ctx, routeCfg(offHost, proxy.SSLModeOff)))

	autoHTTPS, _ := serverConfig(t, c)["automatic_https"].(map[string]any)
	require.NotNil(t, autoHTTPS)
	assert.Contains(t, autoHTTPS["skip"], offHost, "ssl_mode=off must be in automatic_https.skip")

	// No certificate exists for this SNI, so the handshake cannot produce a leaf.
	_, err := leafFor(t, offHost, 3*time.Second)
	assert.Error(t, err, "a skipped hostname must not have a certificate")

	require.NoError(t, c.RemoveRoute(ctx, offHost, "/"))
	autoHTTPS, _ = serverConfig(t, c)["automatic_https"].(map[string]any)
	require.NotNil(t, autoHTTPS)
	assert.NotContains(t, autoHTTPS["skip"], offHost, "skip entry must be pruned with the route")
}

// TestRealCaddy_SyncRestoresServerAfterRestart simulates what a Caddy restart
// does — the config reverts to the static Caddyfile, losing the :443 listener,
// the TLS policy and the skip list — and asserts the reconciler's sync call
// rebuilds all of it. Without this, HTTPS would silently stay down until the API
// itself was restarted.
func TestRealCaddy_SyncRestoresServerAfterRestart(t *testing.T) {
	c := liveClient(t)
	ctx := context.Background()

	c.InitCatchAll(ctx)

	// Roll srv0 back to its Caddyfile shape.
	require.NoError(t, patchJSON(ctx, c, "/config/apps/http/servers/srv0/listen", []string{":80"}))
	require.NoError(t, deleteJSON(ctx, c, "/config/apps/http/servers/srv0/tls_connection_policies"))
	before := serverConfig(t, c)
	require.Equal(t, []any{":80"}, before["listen"], "precondition: HTTPS listener is gone")

	require.NoError(t, c.SyncAutoHTTPS(ctx, []string{offHost}, nil))

	server := serverConfig(t, c)
	assert.Equal(t, []any{":80", ":443"}, server["listen"], "sync must restore the HTTPS listener")
	assert.NotEmpty(t, server["tls_connection_policies"], "sync must restore the TLS connection policy")
	autoHTTPS, _ := server["automatic_https"].(map[string]any)
	require.NotNil(t, autoHTTPS)
	assert.Equal(t, []any{offHost}, autoHTTPS["skip"], "sync must re-assert the skip list from DB truth")
}

// TestRealCaddy_ServerWriteKeepsAccessLogs guards the interaction that made
// ConfigureAccessLogs fragile: ensureServer PATCHes the whole srv0 object, so it
// must merge into the live config rather than replace it, or every route change
// would silently drop access logging.
func TestRealCaddy_ServerWriteKeepsAccessLogs(t *testing.T) {
	c := liveClient(t)
	ctx := context.Background()

	c.InitCatchAll(ctx)
	require.NoError(t, c.ConfigureAccessLogs(ctx))
	// Idempotent: re-running (as an API restart does) must not 409 out.
	require.NoError(t, c.ConfigureAccessLogs(ctx))

	require.NoError(t, c.SyncAutoHTTPS(ctx, nil, nil))

	logs, _ := serverConfig(t, c)["logs"].(map[string]any)
	require.NotNil(t, logs, "server write dropped the access-log config")
	assert.Equal(t, "http.access.srv0", logs["default_logger_name"])
}

// selfSignedFor mints a certificate/key PEM pair for the given hostname.
func selfSignedFor(t *testing.T, hostname string) proxy.HostCertificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)

	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(7),
		Subject:      pkix.Name{CommonName: "belune-integration-ca"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		DNSNames:     []string{hostname},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	require.NoError(t, err)
	keyDER, err := x509.MarshalECPrivateKey(key)
	require.NoError(t, err)

	return proxy.HostCertificate{
		Hostname: hostname,
		CertPEM:  string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})),
		KeyPEM:   string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})),
	}
}

// TestRealCaddy_CustomCertificateIsServed is the Phase 2 contract: an uploaded
// certificate pushed in-band (load_pem) is the one Caddy actually serves for that
// SNI — chosen over the internal CA it would otherwise mint — and it never
// touches Caddy's filesystem.
func TestRealCaddy_CustomCertificateIsServed(t *testing.T) {
	c := liveClient(t)
	ctx := context.Background()
	t.Cleanup(func() {
		_ = c.RemoveRoute(ctx, customHost, "/")
		_ = c.SyncCertificates(ctx, nil)
	})

	c.InitCatchAll(ctx)
	cert := selfSignedFor(t, customHost)

	cfg := routeCfg(customHost, proxy.SSLModeCustom)
	cfg.CertPEM, cfg.KeyPEM = cert.CertPEM, cert.KeyPEM
	require.NoError(t, c.AddRoute(ctx, cfg))

	state, err := leafFor(t, customHost, 15*time.Second)
	require.NoError(t, err, "TLS handshake against local Caddy failed")
	leaf := state.PeerCertificates[0]

	assert.Contains(t, leaf.DNSNames, customHost)
	assert.Contains(t, leaf.Issuer.CommonName, "belune-integration-ca",
		"Caddy served its own internal cert instead of the uploaded one")
}

// TestRealCaddy_SyncCertificatesRestoresAfterRestart proves the reconciler's cert
// sync closes the gap a Caddy restart opens: load_pem certificates live in memory
// only, so without a re-push the domain would fall back to a cert Caddy mints
// itself — or to no certificate at all.
func TestRealCaddy_SyncCertificatesRestoresAfterRestart(t *testing.T) {
	c := liveClient(t)
	ctx := context.Background()
	t.Cleanup(func() { _ = c.SyncCertificates(ctx, nil) })

	cert := selfSignedFor(t, customHost)
	require.NoError(t, c.SyncCertificates(ctx, []proxy.HostCertificate{cert}))

	loaded, err := c.listPEMCerts(ctx)
	require.NoError(t, err)
	require.Len(t, loaded, 1)
	assert.Equal(t, []string{customHost}, loaded[0].Tags)

	// Simulate the restart: Caddy comes back with no in-band certificates.
	require.NoError(t, c.SyncCertificates(ctx, nil))
	loaded, err = c.listPEMCerts(ctx)
	require.NoError(t, err)
	require.Empty(t, loaded, "precondition: certificates are gone")

	// The reconciler's pass re-pushes from DB truth.
	require.NoError(t, c.SyncCertificates(ctx, []proxy.HostCertificate{cert}))
	loaded, err = c.listPEMCerts(ctx)
	require.NoError(t, err)
	require.Len(t, loaded, 1)
	assert.Equal(t, cert.CertPEM, loaded[0].Certificate)
}

func patchJSON(ctx context.Context, c *Client, path string, payload any) error {
	_, err := c.doConfig(ctx, http.MethodPatch, path, payload)
	return err
}

func deleteJSON(ctx context.Context, c *Client, path string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, c.adminURL+path, nil)
	if err != nil {
		return err
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	var body json.RawMessage
	_ = json.NewDecoder(resp.Body).Decode(&body)
	return nil
}

// TestRealCaddy_DashboardRoute covers the reason the dashboard could never have
// HTTPS: it was served by a matcher-less catch-all, and automatic HTTPS only
// issues for names that appear in a host matcher. With its own route, Caddy
// obtains a certificate for it exactly as it does for an app domain.
func TestRealCaddy_DashboardRoute(t *testing.T) {
	c := liveClient(t)
	ctx := context.Background()
	t.Cleanup(func() { _ = c.SetDashboardRoute(ctx, "", proxy.SSLModeAutomatic) })

	c.InitCatchAll(ctx)
	require.NoError(t, c.SetDashboardRoute(ctx, dashboardHost, proxy.SSLModeAutomatic))

	// Caddy issues for it, which is the whole point.
	state, err := leafFor(t, dashboardHost, 15*time.Second)
	require.NoError(t, err, "no certificate served for the dashboard hostname")
	assert.Contains(t, state.PeerCertificates[0].DNSNames, dashboardHost)

	// Idempotent: the reconciler re-asserts this every 30s and must not churn
	// Caddy's config (each write is a full config reload).
	before := routeIDs(t, c)
	require.NoError(t, c.SetDashboardRoute(ctx, dashboardHost, proxy.SSLModeAutomatic))
	assert.Equal(t, before, routeIDs(t, c), "re-applying the same hostname rewrote the route table")

	// The catch-all matches everything, so it must stay last or it would shadow
	// the dashboard route.
	ids := routeIDs(t, c)
	assert.Equal(t, "route-catch-all", ids[len(ids)-1], "catch-all must remain the last route")

	// Clearing it falls back to the catch-all (the bare-IP install).
	require.NoError(t, c.SetDashboardRoute(ctx, "", proxy.SSLModeAutomatic))
	assert.NotContains(t, routeIDs(t, c), "route-dashboard")
}

func routeIDs(t *testing.T, c *Client) []string {
	t.Helper()
	raw, err := c.fetchRawRoutes(context.Background())
	require.NoError(t, err)
	ids := make([]string, 0, len(raw))
	for _, r := range raw {
		var probe struct {
			ID string `json:"@id"`
		}
		_ = json.Unmarshal(r, &probe)
		ids = append(ids, probe.ID)
	}
	return ids
}
