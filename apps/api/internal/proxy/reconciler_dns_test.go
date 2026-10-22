package proxy_test

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/belune-hq/belune/internal/pkg/crypto"
	"github.com/belune-hq/belune/internal/proxy"
	"github.com/belune-hq/belune/internal/proxy/capability"
	"github.com/belune-hq/belune/internal/store/generated"
	"github.com/belune-hq/belune/internal/testutil"
)

// Gate 2 for DNS-01, and the invariant it must not break: a dns_challenge domain
// whose Caddy lacks the provider module keeps its route (so it stays in the same
// sets as before) and is reported as Degraded, while a capable one gets exactly
// one policy carrying every hostname for that provider.
func TestReconcile_DNSChallengePolicies(t *testing.T) {
	if os.Getenv("BELUNE_CADDY_INTEGRATION") == "" {
		t.Skip("set BELUNE_CADDY_INTEGRATION=1 (needs Docker for Postgres)")
	}
	ctx := context.Background()
	pool, queries, teardown := testutil.SetupTestDB()
	defer teardown()
	t.Cleanup(func() { _ = testutil.TruncateAll(ctx, pool) })

	keyring, err := crypto.ParseKeyringEnv("", testutil.TestEncryptionKey, "")
	require.NoError(t, err)
	enc, err := keyring.Encrypt([]byte(`{"api_token":"tok"}`))
	require.NoError(t, err)
	prov, err := queries.CreateDNSProvider(ctx, generated.CreateDNSProviderParams{
		Name: "cf", Provider: "cloudflare", CredentialsEncrypted: enc,
	})
	require.NoError(t, err)

	app := seedRunningApp(t, ctx, queries)
	mk := func(host string, withProvider bool) {
		p := createDomainParams(app.ID, host, proxy.SSLModeDNSChallenge)
		if withProvider {
			p.DnsProviderID = prov.ID
		}
		_, err := queries.CreateDomain(ctx, p)
		require.NoError(t, err)
	}
	mk("*.example.com", true)
	mk("apex.example.com", true)
	mk("legacy.example.com", false) // a row from before the mode was withdrawn

	run := func(set capability.Set) (*mockProxy, proxy.ReconcilerStatus) {
		mock := &mockProxy{}
		r := proxy.NewReconciler(queries, mock, keyring, time.Second).WithCapabilities(&countingProvider{set: set})
		require.NoError(t, r.ReconcileNow(ctx))
		return mock, r.Status()
	}

	t.Run("capable proxy", func(t *testing.T) {
		mock, st := run(capability.NewSet("dns.providers.cloudflare", "http.handlers.reverse_proxy"))
		require.Len(t, mock.dns, 1, "one provider, one policy")
		assert.Equal(t, []string{"*.example.com", "apex.example.com"}, mock.dns[0].Subjects)
		assert.Equal(t, "tok", mock.dns[0].Credentials["api_token"])

		require.Len(t, st.Degraded, 1, "the provider-less legacy row is surfaced, not silently dropped")
		assert.Equal(t, "legacy.example.com", st.Degraded[0].Hostname)
		assert.Equal(t, proxy.ReasonNoProvider, st.Degraded[0].Reason)
		assert.Equal(t, proxy.FeatureDNSChallenge, st.Degraded[0].Feature)
	})

	t.Run("stock proxy keeps the routes and degrades the domains", func(t *testing.T) {
		mock, st := run(capability.NewSet("http.handlers.reverse_proxy"))
		assert.Empty(t, mock.dns, "no policy may be pushed for a module Caddy lacks")
		assert.Len(t, st.Degraded, 3)
		hosts := map[string]bool{}
		for _, c := range mock.ensured {
			hosts[c.Hostname] = true
		}
		assert.True(t, hosts["*.example.com"] && hosts["apex.example.com"], "a degraded domain's route must still be pushed")
		for _, d := range st.Degraded[:2] {
			assert.Equal(t, proxy.ReasonModuleAbsent, d.Reason)
			assert.Equal(t, "dns.providers.cloudflare", d.Module)
		}
	})

	t.Run("the credential never reaches the degradation report", func(t *testing.T) {
		_, st := run(capability.NewSet("http.handlers.reverse_proxy"))
		raw, _ := json.Marshal(st)
		assert.NotContains(t, string(raw), "tok")
	})
}
