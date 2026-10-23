package worker_test

import (
	"context"
	"errors"
	"net"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/belune-hq/belune/internal/config"
	"github.com/belune-hq/belune/internal/pkg/crypto"
	"github.com/belune-hq/belune/internal/proxy"
	"github.com/belune-hq/belune/internal/store/generated"
	"github.com/belune-hq/belune/internal/testutil"
	"github.com/belune-hq/belune/internal/worker"
)

// A name with no A record is DNS-01's flagship case: the CA reads a TXT record and
// never connects to the host. The precheck used to condemn it ("a certificate can
// never be issued"), and the domain issued fine once its app ran.
//
// Needs no Caddy: with no probe address the handshake simply fails, which is the
// "no certificate yet" path the precheck's verdict is layered on.
func TestTLSSweep_DNSChallengeWithoutARecordIsNotFailed(t *testing.T) {
	ctx := context.Background()
	t.Cleanup(func() { _ = testutil.TruncateAll(ctx, testPool) })

	// RFC 6761 guarantees .invalid never resolves. Skip rather than pass
	// vacuously where the resolver is unreachable and says nothing.
	_, err := net.DefaultResolver.LookupHost(ctx, "no-a-record.invalid")
	var dnsErr *net.DNSError
	if !errors.As(err, &dnsErr) || !dnsErr.IsNotFound {
		t.Skipf("resolver gave no NXDOMAIN for .invalid (%v)", err)
	}

	keyring, err := crypto.ParseKeyringEnv("", testutil.TestEncryptionKey, "")
	require.NoError(t, err)
	h := &worker.TaskHandler{
		Queries: testQueries, DB: testPool, Keyring: keyring,
		Config: &config.Config{PublicIP: "203.0.113.7"},
	}

	seed := func(host, mode string) generated.Domain {
		app, _ := seedApp(t)
		d, err := testQueries.CreateDomain(ctx, generated.CreateDomainParams{
			Path: "/", ApplicationID: app.ID, Hostname: host, SslEnabled: true, SslMode: mode,
			ContainerPort: pgtype.Int4{Int32: 8080, Valid: true},
		})
		require.NoError(t, err)
		return d
	}
	dns01 := seed("no-a-record.invalid", proxy.SSLModeDNSChallenge)

	h.HandleTLSStatusSweep(ctx)

	got, err := testQueries.GetDomain(ctx, dns01.ID)
	require.NoError(t, err)
	assert.NotEqual(t, worker.TLSStatusFailed, got.TlsStatus, "must not be condemned for a missing A record")
	assert.Empty(t, got.TlsError.String, "the advisory must never be written to tls_error")
	assert.Contains(t, got.TlsAdvisory.String, "will not serve traffic", "but the true claim, about serving, is kept")

	// The behaviour the fix must not disturb: over HTTP-01 the same name IS fatal.
	auto := seed("no-a-record-auto.invalid", proxy.SSLModeAutomatic)
	h.HandleTLSStatusSweep(ctx)
	got, err = testQueries.GetDomain(ctx, auto.ID)
	require.NoError(t, err)
	assert.Equal(t, worker.TLSStatusFailed, got.TlsStatus)
	assert.Contains(t, got.TlsError.String, "can never be issued")
}
