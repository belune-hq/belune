package proxy_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/weiliang79/belune/internal/pkg/crypto"
	"github.com/weiliang79/belune/internal/proxy"
	"github.com/weiliang79/belune/internal/proxy/caddy"
	"github.com/weiliang79/belune/internal/status"
	"github.com/weiliang79/belune/internal/store/generated"
	"github.com/weiliang79/belune/internal/testutil"
)

// This closes the one seam the other tests cannot reach: real DB rows, through
// the real keyring, through the real reconciler, into a real Caddy. Everything
// between an operator uploading a PEM pair and Caddy serving it on the wire.
//
//	BELUNE_CADDY_INTEGRATION=1 go test -p 1 ./internal/proxy/ -run RealStack -v

const reconcileHost = "phase2-reconcile.belune.local"

func TestRealStack_ReconcilerPushesCustomCertificate(t *testing.T) {
	if os.Getenv("BELUNE_CADDY_INTEGRATION") == "" {
		t.Skip("set BELUNE_CADDY_INTEGRATION=1 to run against a real Caddy + Postgres")
	}

	ctx := context.Background()
	pool, queries, teardown := testutil.SetupTestDB()
	defer teardown()
	t.Cleanup(func() { _ = testutil.TruncateAll(ctx, pool) })

	keyring, err := crypto.ParseKeyringEnv("", testutil.TestEncryptionKey, "")
	require.NoError(t, err)

	adminURL := os.Getenv("CADDY_ADMIN_URL")
	if adminURL == "" {
		adminURL = "http://localhost:2019"
	}
	client := caddy.New(adminURL)
	require.NoError(t, client.Ping(ctx), "caddy admin API not reachable — is `task dev:infra` up?")
	t.Cleanup(func() {
		_ = client.RemoveRoute(ctx, reconcileHost, "/")
		_ = client.SyncCertificates(ctx, nil)
	})
	client.InitCatchAll(ctx)

	// An operator uploads a certificate: PEM encrypted at rest with the keyring.
	certPEM, keyPEM := selfSignedPEM(t, reconcileHost)
	certEnc, err := keyring.Encrypt([]byte(certPEM))
	require.NoError(t, err)
	keyEnc, err := keyring.Encrypt([]byte(keyPEM))
	require.NoError(t, err)

	cert, err := queries.CreateCertificate(ctx, generated.CreateCertificateParams{
		Name:             "reconcile-test",
		CertPemEncrypted: certEnc,
		KeyPemEncrypted:  keyEnc,
		Subjects:         []string{reconcileHost},
		NotAfter:         pgtype.Timestamptz{Time: time.Now().Add(24 * time.Hour), Valid: true},
	})
	require.NoError(t, err)

	// …and points a running application's domain at it in custom mode.
	app := seedRunningApp(t, ctx, queries)
	_, err = queries.CreateDomain(ctx, generated.CreateDomainParams{
		Path:          "/",
		ApplicationID: app.ID,
		Hostname:      reconcileHost,
		SslEnabled:    true,
		SslMode:       proxy.SSLModeCustom,
		CertificateID: cert.ID,
		ContainerPort: pgtype.Int4{Int32: 8080, Valid: true},
	})
	require.NoError(t, err)

	// One reconcile pass is all the app ever does to converge Caddy on the DB.
	r := proxy.NewReconciler(queries, client, keyring, 30*time.Second)
	require.NoError(t, r.ReconcileNow(ctx))

	// The certificate must now be the one Caddy actually presents for that SNI.
	leaf := dialLeaf(t, reconcileHost)
	assert.Contains(t, leaf.DNSNames, reconcileHost)
	assert.Contains(t, leaf.Issuer.CommonName, "belune-reconcile-ca",
		"Caddy served something other than the certificate stored in the database")
}

// seedRunningApp inserts the user → project → application chain the reconciler
// walks, with the application in the only status it considers routable.
func seedRunningApp(t *testing.T, ctx context.Context, queries *generated.Queries) generated.Application {
	t.Helper()

	user, err := queries.CreateUser(ctx, generated.CreateUserParams{
		Email:        "reconcile@test.com",
		PasswordHash: "x",
		Role:         "admin",
	})
	require.NoError(t, err)

	project, err := queries.CreateProject(ctx, generated.CreateProjectParams{
		Name:     "Reconcile Project",
		Slug:     "reconcile-project",
		UserID:   user.ID,
		ServerID: testutil.LocalServerID(t, ctx, queries),
	})
	require.NoError(t, err)

	app, err := queries.CreateApplication(ctx, generated.CreateApplicationParams{
		ProjectID:   project.ID,
		Name:        "Reconcile App",
		Slug:        "reconcile-project-app",
		Type:        "image",
		BuildType:   "image",
		SourceImage: pgtype.Text{String: "nginx:alpine", Valid: true},
	})
	require.NoError(t, err)

	updated, err := queries.UpdateApplicationStatus(ctx, generated.UpdateApplicationStatusParams{
		ID:     app.ID,
		Status: status.ApplicationRunning,
	})
	require.NoError(t, err)
	return updated
}

func selfSignedPEM(t *testing.T, hostname string) (certPEM, keyPEM string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)

	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(11),
		Subject:      pkix.Name{CommonName: "belune-reconcile-ca"},
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

func dialLeaf(t *testing.T, hostname string) *x509.Certificate {
	t.Helper()
	addr := os.Getenv("CADDY_HTTPS_ADDR")
	if addr == "" {
		addr = "localhost:443"
	}

	var lastErr error
	deadline := time.Now().Add(15 * time.Second)
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
				return state.PeerCertificates[0]
			}
		}
		lastErr = err
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("no certificate served for %s: %v", hostname, lastErr)
	return nil
}

// TestRealStack_ReconcilerKeepsDashboardRoute guards the sharpest edge in the
// dashboard-domain design: the reconciler deletes any host-matched route in Caddy
// that has no matching row in the domains table, and the dashboard has none by
// design. Without the exemption it would publish the dashboard and then delete it
// again within one interval — and the operator would watch their panel die 30
// seconds after configuring it.
func TestRealStack_ReconcilerKeepsDashboardRoute(t *testing.T) {
	if os.Getenv("BELUNE_CADDY_INTEGRATION") == "" {
		t.Skip("set BELUNE_CADDY_INTEGRATION=1 to run against a real Caddy + Postgres")
	}

	ctx := context.Background()
	pool, queries, teardown := testutil.SetupTestDB()
	defer teardown()
	t.Cleanup(func() { _ = testutil.TruncateAll(ctx, pool) })

	keyring, err := crypto.ParseKeyringEnv("", testutil.TestEncryptionKey, "")
	require.NoError(t, err)

	adminURL := os.Getenv("CADDY_ADMIN_URL")
	if adminURL == "" {
		adminURL = "http://localhost:2019"
	}
	client := caddy.New(adminURL)
	require.NoError(t, client.Ping(ctx))
	t.Cleanup(func() { _ = client.SetDashboardRoute(ctx, "", proxy.SSLModeAutomatic) })
	client.InitCatchAll(ctx)

	const host = "panel.belune.local"
	_, err = queries.UpsertSetting(ctx, generated.UpsertSettingParams{
		Key:   proxy.SettingDashboardDomain,
		Value: host,
	})
	require.NoError(t, err)

	r := proxy.NewReconciler(queries, client, keyring, 30*time.Second)

	// The first pass publishes it from the setting…
	require.NoError(t, r.ReconcileNow(ctx))
	assert.Equal(t, host, dashboardRouteHostname(t, client),
		"reconciler should publish the dashboard route from the setting")

	// …and a second pass must not then sweep it away as an unknown domain.
	require.NoError(t, r.ReconcileNow(ctx))
	assert.Equal(t, host, dashboardRouteHostname(t, client),
		"reconciler deleted its own dashboard route as a stale app domain")
}

// dashboardRouteHostname reads the hostname Caddy currently serves the dashboard
// on, via the live config rather than our in-memory idea of it.
func dashboardRouteHostname(t *testing.T, c *caddy.Client) string {
	t.Helper()
	routes, err := c.ListRoutes(context.Background())
	require.NoError(t, err)
	for _, r := range routes {
		if r.Hostname == "panel.belune.local" {
			return r.Hostname
		}
	}
	return ""
}

// The recovery gap, measured against a real restart. The interval is an hour, so
// the route can only come back through the probe.
//
//	docker run -d --name restart-drill -p 127.0.0.1:12019:2019 \
//	  -v $PWD/infra/caddy/Caddyfile.template:/etc/caddy/Caddyfile:ro \
//	  -v $PWD/infra/caddy/sites:/etc/caddy/sites:ro caddy:2-alpine
//	CADDY_ADMIN_URL=http://localhost:12019 CADDY_RESTART_CONTAINER=restart-drill \
//	  BELUNE_CADDY_INTEGRATION=1 go test -p 1 ./internal/proxy/ -run RestartRecovers -v
//
// Never point this at the dev stack's Caddy: that stack is devcontainer-managed.
func TestRealStack_CaddyRestartRecoversWithoutTheTick(t *testing.T) {
	if os.Getenv("BELUNE_CADDY_INTEGRATION") == "" {
		t.Skip("set BELUNE_CADDY_INTEGRATION=1 to run against a real Caddy + Postgres")
	}
	container := os.Getenv("CADDY_RESTART_CONTAINER")
	if container == "" {
		t.Skip("set CADDY_RESTART_CONTAINER to a throwaway Caddy container this test may restart")
	}
	adminURL := os.Getenv("CADDY_ADMIN_URL")
	require.NotEmpty(t, adminURL, "CADDY_ADMIN_URL must point at the throwaway container")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pool, queries, teardown := testutil.SetupTestDB()
	defer teardown()
	t.Cleanup(func() { _ = testutil.TruncateAll(context.Background(), pool) })

	const host = "restart-drill.belune.local"
	app := seedRunningApp(t, ctx, queries)
	_, err := queries.CreateDomain(ctx, createDomainParams(app.ID, host, proxy.SSLModeOff))
	require.NoError(t, err)

	client := caddy.New(adminURL)
	require.NoError(t, client.Ping(ctx))
	t.Cleanup(func() { _ = client.RemoveRoute(context.Background(), host, "/") })

	hasRoute := func() bool {
		routes, err := client.ListRoutes(ctx)
		if err != nil {
			return false
		}
		for _, r := range routes {
			if r.Hostname == host {
				return true
			}
		}
		return false
	}

	r := proxy.NewReconciler(queries, client, nil, time.Hour)
	go r.Run(ctx)
	require.Eventually(t, hasRoute, 10*time.Second, 200*time.Millisecond, "the opening pass")

	out, err := exec.Command("docker", "restart", container).CombinedOutput()
	require.NoError(t, err, string(out))
	restarted := time.Now()

	// The route is gone the moment Caddy comes back with only its Caddyfile.
	// Wait for it to answer, then for the probe to put the route back.
	require.Eventually(t, func() bool { return client.Ping(ctx) == nil }, 15*time.Second, 100*time.Millisecond)
	require.Eventually(t, hasRoute, 10*time.Second, 200*time.Millisecond,
		"the route must come back through the probe; the tick is an hour away")
	t.Logf("route restored %s after docker restart returned", time.Since(restarted).Round(100*time.Millisecond))
}
