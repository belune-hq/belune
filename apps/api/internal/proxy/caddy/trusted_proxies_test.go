package caddy

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/belune-hq/belune/internal/proxy"
)

// serverStub is a Caddy admin API that holds one server object and answers PUT,
// PATCH and DELETE on its keys the way a real Caddy does (measured on v2.11.7):
// PUT only creates and is a 409 when the key exists, PATCH only replaces and is a
// 404 when it does not, and DELETE of an absent key is a 404.
type serverStub struct {
	mu     sync.Mutex
	server map[string]any
	writes []string // "METHOD key", in order
	// states records the server's two keys after every write, so a test can look at
	// every intermediate state a request could have seen.
	states []map[string]any
}

func (s *serverStub) handler(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r.URL.Path == serverPath && r.Method == http.MethodGet {
		_ = json.NewEncoder(w).Encode(s.server)
		return
	}
	key, ok := strings.CutPrefix(r.URL.Path, serverPath+"/")
	if !ok {
		http.NotFound(w, r)
		return
	}
	_, exists := s.server[key]
	switch r.Method {
	case http.MethodPut, http.MethodPatch:
		if r.Method == http.MethodPut && exists {
			http.Error(w, `{"error":"key already exists"}`, http.StatusConflict)
			return
		}
		if r.Method == http.MethodPatch && !exists {
			http.Error(w, `{"error":"key does not exist"}`, http.StatusNotFound)
			return
		}
		var v any
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &v)
		s.server[key] = v
	case http.MethodDelete:
		if !exists {
			http.Error(w, `{"error":"key does not exist"}`, http.StatusNotFound)
			return
		}
		delete(s.server, key)
	}
	s.writes = append(s.writes, r.Method+" "+key)
	s.states = append(s.states, map[string]any{
		"trusted_proxies":        s.server["trusted_proxies"],
		"trusted_proxies_strict": s.server["trusted_proxies_strict"],
	})
}

func newServerStub(t *testing.T, server map[string]any) (*Client, *serverStub) {
	t.Helper()
	s := &serverStub{server: server}
	srv := httptest.NewServer(http.HandlerFunc(s.handler))
	t.Cleanup(srv.Close)
	return New(srv.URL), s
}

// exposesForgery is the state the whole feature exists to avoid: proxies are
// trusted but X-Forwarded-For is still read left to right, so a visitor behind
// the CDN chooses their own address.
func exposesForgery(state map[string]any) bool {
	return state["trusted_proxies"] != nil && state["trusted_proxies_strict"] != float64(1)
}

func TestSyncTrustedProxies_NeverLeavesRangesWithoutStrict(t *testing.T) {
	ctx := context.Background()
	cfg := proxy.TrustedProxiesConfig{CIDRs: []string{"10.0.0.0/8", "192.168.0.0/16"}}

	t.Run("adding", func(t *testing.T) {
		c, s := newServerStub(t, map[string]any{"listen": []any{":80"}})
		require.NoError(t, c.SyncTrustedProxies(ctx, cfg))
		assert.Equal(t, []string{"PUT trusted_proxies_strict", "PUT trusted_proxies"}, s.writes, "strict first")
		for i, st := range s.states {
			assert.False(t, exposesForgery(st), "state after write %d trusted proxies with strict off", i)
		}
		assert.Equal(t, map[string]any{"source": "static", "ranges": []any{"10.0.0.0/8", "192.168.0.0/16"}}, s.server["trusted_proxies"])
		assert.EqualValues(t, 1, s.server["trusted_proxies_strict"])
	})

	t.Run("changing the ranges", func(t *testing.T) {
		c, s := newServerStub(t, map[string]any{})
		require.NoError(t, c.SyncTrustedProxies(ctx, cfg))
		s.writes, s.states = nil, nil
		require.NoError(t, c.SyncTrustedProxies(ctx, proxy.TrustedProxiesConfig{CIDRs: []string{"172.16.0.0/12"}}))
		assert.Equal(t, []string{"PATCH trusted_proxies"}, s.writes, "only the ranges changed, and strict is already on")
		assert.Equal(t, map[string]any{"source": "static", "ranges": []any{"172.16.0.0/12"}}, s.server["trusted_proxies"])
	})

	t.Run("removing", func(t *testing.T) {
		c, s := newServerStub(t, map[string]any{})
		require.NoError(t, c.SyncTrustedProxies(ctx, cfg))
		s.writes, s.states = nil, nil
		require.NoError(t, c.SyncTrustedProxies(ctx, proxy.TrustedProxiesConfig{}))
		assert.Equal(t, []string{"DELETE trusted_proxies", "DELETE trusted_proxies_strict"}, s.writes, "ranges first")
		for i, st := range s.states {
			assert.False(t, exposesForgery(st), "state after write %d", i)
		}
		assert.NotContains(t, s.server, "trusted_proxies")
		assert.NotContains(t, s.server, "trusted_proxies_strict")
	})
}

func TestSyncTrustedProxies_ARepairsAStrictThatWasLost(t *testing.T) {
	// Ranges present (somebody's hand edit) but strict off: the forgeable state.
	ctx := context.Background()
	c, s := newServerStub(t, map[string]any{
		"trusted_proxies": map[string]any{"source": "static", "ranges": []any{"10.0.0.0/8"}},
	})
	require.NoError(t, c.SyncTrustedProxies(ctx, proxy.TrustedProxiesConfig{CIDRs: []string{"10.0.0.0/8"}}))
	assert.EqualValues(t, 1, s.server["trusted_proxies_strict"], "equal ranges are not enough to call it correct")
}

func TestSyncTrustedProxies_IsAReadWhenNothingChanged(t *testing.T) {
	ctx := context.Background()
	cfg := proxy.TrustedProxiesConfig{CIDRs: []string{"192.168.0.0/16", "10.0.0.0/8"}}
	c, s := newServerStub(t, map[string]any{})
	require.NoError(t, c.SyncTrustedProxies(ctx, cfg))
	s.writes = nil

	// The same list in another order is not drift, and neither is nothing-to-remove.
	require.NoError(t, c.SyncTrustedProxies(ctx, proxy.TrustedProxiesConfig{CIDRs: []string{"10.0.0.0/8", "192.168.0.0/16"}}))
	assert.Empty(t, s.writes, "a write is a Caddy reload")

	c2, s2 := newServerStub(t, map[string]any{})
	require.NoError(t, c2.SyncTrustedProxies(ctx, proxy.TrustedProxiesConfig{}))
	assert.Empty(t, s2.writes)
}

func TestSyncTrustedProxies_ForeignFieldsAreADifference(t *testing.T) {
	// A trusted_proxies with an extra field (an interval, say) was not written by
	// Belune; the setting is the whole truth, so it is rewritten to exactly that.
	assert.False(t, trustedProxiesEqual(map[string]any{
		"source": "static", "ranges": []any{"10.0.0.0/8"}, "interval": "1h",
	}, []string{"10.0.0.0/8"}))
	assert.False(t, trustedProxiesEqual(map[string]any{"source": "cloudflare", "ranges": []any{}}, nil))
}

// A real Caddy decides what a value is. This is the proof that a range it refuses
// changes nothing: the admin API loads a config whole or not at all.
//
//	BELUNE_CADDY_INTEGRATION=1 CADDY_ADMIN_URL=http://127.0.0.1:12020 \
//	  go test ./internal/proxy/caddy/ -run RealCaddy_TrustedProxies -v
//
// against a Caddy that has a server named srv0 (the dev stack does).
func TestRealCaddy_TrustedProxies_ARefusedValueChangesNothing(t *testing.T) {
	c := liveClient(t)
	ctx := context.Background()
	t.Cleanup(func() { _ = c.SyncTrustedProxies(ctx, proxy.TrustedProxiesConfig{}) })

	good := proxy.TrustedProxiesConfig{CIDRs: []string{"10.0.0.0/8"}}
	require.NoError(t, c.SyncTrustedProxies(ctx, good))
	before, err := c.fetchServer(ctx)
	require.NoError(t, err)

	// Bypasses the API's validation on purpose: this is "what if Caddy refuses".
	err = c.SyncTrustedProxies(ctx, proxy.TrustedProxiesConfig{CIDRs: []string{"not-a-cidr"}})
	require.Error(t, err, "Caddy accepted a range that is not one")

	after, err := c.fetchServer(ctx)
	require.NoError(t, err)
	assert.Equal(t, before["trusted_proxies"], after["trusted_proxies"], "the old value must still be serving")
	assert.EqualValues(t, 1, after["trusted_proxies_strict"])
	assert.NotNil(t, after["listen"], "and the rest of the server is untouched")

	// And the good value is still a read.
	require.NoError(t, c.SyncTrustedProxies(ctx, good))
}
