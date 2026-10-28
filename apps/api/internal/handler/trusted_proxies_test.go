package handler_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/belune-hq/belune/internal/proxy"
	"github.com/belune-hq/belune/internal/runtime"
	"github.com/belune-hq/belune/internal/testutil"
)

const trustedProxiesPath = "/api/maintenance/proxy/trusted-proxies"

func trustedProxiesFixture(t *testing.T) string {
	t.Helper()
	resetDB(t)
	token := env.SetupAdmin(t, "admin@test.com", "password123")
	t.Cleanup(func() {
		env.Reconciler.ReconcileNowCalls = 0
		env.Runtime.ListSystemContainers_ = nil
		env.Runtime.ListNetworks_ = nil
		_, _ = env.Pool.Exec(context.Background(), `DELETE FROM settings WHERE key = $1`, proxy.SettingTrustedProxies)
	})
	return token
}

func putTrusted(t *testing.T, token string, body any) *http.Response {
	t.Helper()
	return env.DoRequest(t, "PUT", trustedProxiesPath, body, testutil.AuthHeader(token))
}

func storedTrusted(t *testing.T) (string, bool) {
	t.Helper()
	var v string
	err := env.Pool.QueryRow(context.Background(), `SELECT value FROM settings WHERE key = $1`, proxy.SettingTrustedProxies).Scan(&v)
	return v, err == nil
}

func TestTrustedProxies_DefaultsToNoneAndOffersThePresetWithoutApplyingIt(t *testing.T) {
	token := trustedProxiesFixture(t)

	resp := env.DoRequest(t, "GET", trustedProxiesPath, nil, testutil.AuthHeader(token))
	require.Equal(t, http.StatusOK, resp.StatusCode)
	body := testutil.ReadJSON(t, resp)

	assert.Empty(t, body["cidrs"], "trusting a proxy is opt-in: the default is none")
	assert.Equal(t, true, body["overlaps_checked"])
	presets, _ := body["presets"].([]any)
	require.Len(t, presets, 1)
	cf := presets[0].(map[string]any)
	assert.Equal(t, "cloudflare", cf["id"])
	assert.NotEmpty(t, cf["checked_at"])
	assert.NotEmpty(t, cf["source"])

	_, stored := storedTrusted(t)
	assert.False(t, stored, "offering a preset must not store it")
}

func TestTrustedProxies_SavesTheListCanonicalAndAppliesItNow(t *testing.T) {
	token := trustedProxiesFixture(t)

	resp := putTrusted(t, token, map[string]any{"cidrs": []string{"192.168.5.7/16", "10.0.0.0/8", "10.0.0.0/8"}})
	require.Equal(t, http.StatusOK, resp.StatusCode)
	body := testutil.ReadJSON(t, resp)
	assert.Equal(t, []any{"10.0.0.0/8", "192.168.0.0/16"}, body["cidrs"], "masked, deduplicated, sorted")
	assert.Equal(t, 1, env.Reconciler.ReconcileNowCalls, "applied now, not on the next 30-second pass")

	v, ok := storedTrusted(t)
	require.True(t, ok)
	assert.JSONEq(t, `["10.0.0.0/8","192.168.0.0/16"]`, v)

	// Emptying the list removes it, which is how it is turned off.
	resp = putTrusted(t, token, map[string]any{"cidrs": []string{}})
	require.Equal(t, http.StatusOK, resp.StatusCode)
	_ = resp.Body.Close()
	v, _ = storedTrusted(t)
	assert.Empty(t, v)
	assert.Equal(t, 2, env.Reconciler.ReconcileNowCalls)
}

func TestTrustedProxies_RefusesWhatCouldNotBeTrustedAndStoresNothing(t *testing.T) {
	token := trustedProxiesFixture(t)

	for name, body := range map[string]any{
		"the whole internet": map[string]any{"cidrs": []string{"0.0.0.0/0"}},
		"junk":               map[string]any{"cidrs": []string{"cloudflare"}},
		"one good, one bad":  map[string]any{"cidrs": []string{"10.0.0.0/8", "::/0"}},
		"no list at all":     map[string]any{},
		"not a list":         map[string]any{"cidrs": "10.0.0.0/8"},
	} {
		t.Run(name, func(t *testing.T) {
			resp := putTrusted(t, token, body)
			defer func() { _ = resp.Body.Close() }()
			assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
			_, stored := storedTrusted(t)
			assert.False(t, stored, "a refused list must leave nothing behind, not even its good half")
		})
	}
	assert.Equal(t, 0, env.Reconciler.ReconcileNowCalls)
}

// Caddy sits on every project network, so a container there can open a connection
// to it. Trusting its subnet makes every deployed container a proxy that can claim
// any address — a case worth saying out loud, though private ranges stay allowed.
func TestTrustedProxies_SaysWhenARangeOverlapsANetworkCaddyIsOn(t *testing.T) {
	token := trustedProxiesFixture(t)
	env.Runtime.ListSystemContainers_ = []runtime.ContainerInfo{{
		ID: "caddy-id", Name: "infra-caddy-1", Labels: map[string]string{"belune-system": "caddy"},
	}}
	env.Runtime.ListNetworks_ = []runtime.NetworkInfo{
		{Name: "belune-proj-a", Subnets: []string{"172.18.0.0/16"}, Containers: []runtime.NetworkContainer{{ID: "caddy-id"}, {ID: "app"}}},
		{Name: "unrelated", Subnets: []string{"10.50.0.0/16"}, Containers: []runtime.NetworkContainer{{ID: "other"}}},
	}

	// A range that contains a project network.
	resp := putTrusted(t, token, map[string]any{"cidrs": []string{"172.16.0.0/12"}})
	require.Equal(t, http.StatusOK, resp.StatusCode)
	body := testutil.ReadJSON(t, resp)
	assert.Equal(t, true, body["overlaps_checked"])
	overlaps, _ := body["overlaps"].([]any)
	require.Len(t, overlaps, 1)
	assert.Equal(t, map[string]any{"cidr": "172.16.0.0/12", "network": "belune-proj-a", "subnet": "172.18.0.0/16"}, overlaps[0])

	// A private range that overlaps no network Caddy is on is allowed without a word,
	// including one that overlaps a network Caddy is NOT on.
	resp = putTrusted(t, token, map[string]any{"cidrs": []string{"10.0.0.0/8"}})
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Empty(t, testutil.ReadJSON(t, resp)["overlaps"])

	// And a warning is not a refusal: the first list was saved.
	resp = putTrusted(t, token, map[string]any{"cidrs": []string{"172.16.0.0/12"}})
	require.Equal(t, http.StatusOK, resp.StatusCode)
	_ = resp.Body.Close()
	v, _ := storedTrusted(t)
	assert.JSONEq(t, `["172.16.0.0/12"]`, v)
}

func TestTrustedProxies_SaysWhenItCouldNotLookRatherThanClaimingNoOverlap(t *testing.T) {
	token := trustedProxiesFixture(t)
	env.Runtime.ListContainersErr = assert.AnError
	t.Cleanup(func() { env.Runtime.ListContainersErr = nil })

	resp := putTrusted(t, token, map[string]any{"cidrs": []string{"172.16.0.0/12"}})
	require.Equal(t, http.StatusOK, resp.StatusCode)
	body := testutil.ReadJSON(t, resp)
	assert.Equal(t, false, body["overlaps_checked"], "no overlaps found and could not look are different answers")
}

// Granting a proxy the right to claim any address leaves a record of who, when,
// what it replaced and what is wrong with it.
func TestTrustedProxies_AreAuditedWithBeforeAndAfter(t *testing.T) {
	token := trustedProxiesFixture(t)
	base := newAuditedMCP(t) // a second router over the same DB, with a real audit writer

	send := func(cidrs []string) {
		t.Helper()
		b, _ := json.Marshal(map[string]any{"cidrs": cidrs})
		req, err := http.NewRequest("PUT", base+trustedProxiesPath, bytes.NewReader(b))
		require.NoError(t, err)
		req.Header.Set("Content-Type", "application/json")
		for k, v := range testutil.AuthHeader(token) {
			req.Header.Set(k, v)
		}
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		_ = resp.Body.Close()
		require.Equal(t, http.StatusOK, resp.StatusCode)
	}
	send([]string{"10.0.0.0/8"})
	send([]string{"192.168.0.0/16"})

	var details string
	require.Eventually(t, func() bool {
		return env.Pool.QueryRow(context.Background(),
			`SELECT details::text FROM audit_logs WHERE action = 'update_trusted_proxies' ORDER BY created_at DESC LIMIT 1`,
		).Scan(&details) == nil && bytes.Contains([]byte(details), []byte("192.168.0.0/16"))
	}, 3*time.Second, 10*time.Millisecond, "the change was not audited")

	var d struct {
		Before []string `json:"before"`
		After  []string `json:"after"`
	}
	require.NoError(t, json.Unmarshal([]byte(details), &d))
	assert.Equal(t, []string{"10.0.0.0/8"}, d.Before)
	assert.Equal(t, []string{"192.168.0.0/16"}, d.After)
}
