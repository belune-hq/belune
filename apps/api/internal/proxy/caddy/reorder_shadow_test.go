package caddy

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/weiliang79/belune/internal/proxy"
)

// idsOf reads the @id of each route; a match-less route has none.
func idsOf(t *testing.T, raw []json.RawMessage) []string {
	t.Helper()
	out := make([]string, 0, len(raw))
	for _, r := range raw {
		var p struct {
			ID string `json:"@id"`
		}
		require.NoError(t, json.Unmarshal(r, &p))
		out = append(out, p.ID)
	}
	return out
}

// A Caddy restarted on its own comes back with the Caddyfile's default site as an
// anonymous, match-less route. It sorts ahead of every real hostname, so unless
// reorderRoutes drops it, every domain serves the dashboard.
func TestReorderRoutes_DropsTheCaddyfileDefaultSite(t *testing.T) {
	var mu sync.Mutex
	var patched []json.RawMessage

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			_, _ = io.WriteString(w, `[
				{"handle":[{"handler":"reverse_proxy","upstreams":[{"dial":"belune:8080"}]}]},
				{"@id":"route-app.test","match":[{"host":["app.test"]}],"handle":[{"handler":"static_response"}]},
				{"@id":"route-catch-all","handle":[{"handler":"reverse_proxy","upstreams":[{"dial":"belune:8080"}]}]}
			]`)
		case http.MethodPatch:
			mu.Lock()
			defer mu.Unlock()
			require.NoError(t, json.NewDecoder(r.Body).Decode(&patched))
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer srv.Close()

	require.NoError(t, New(srv.URL).reorderRoutes(context.Background()))

	mu.Lock()
	defer mu.Unlock()
	assert.Equal(t, []string{"route-app.test", "route-catch-all"}, idsOf(t, patched),
		"the anonymous default site must not survive, and the catch-all stays last")
}

// The same filter must leave a healthy array alone: a host-matched route is never
// dropped, and an install with no domains keeps its catch-all.
func TestReorderRoutes_KeepsHostRoutesAndSynthesisesTheCatchAll(t *testing.T) {
	var patched []json.RawMessage
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			_, _ = io.WriteString(w, `[
				{"@id":"route-b.test","match":[{"host":["b.test"]}],"handle":[]},
				{"@id":"route-a.test","match":[{"host":["a.test"]}],"handle":[]}
			]`)
			return
		}
		require.NoError(t, json.NewDecoder(r.Body).Decode(&patched))
	}))
	defer srv.Close()

	require.NoError(t, New(srv.URL).reorderRoutes(context.Background()))
	assert.Equal(t, []string{"route-a.test", "route-b.test", "route-catch-all"}, idsOf(t, patched))
}

// The real thing, which a fake admin API cannot prove: Caddy built from the
// production Caddyfile, which materialises the default site for free, then one
// AddRoute. Reproduces a Caddy restart without an API restart (no InitCatchAll).
//
//	docker run -d --name shadow-drill -p 127.0.0.1:12019:2019 \
//	  -v $PWD/infra/caddy/Caddyfile.template:/etc/caddy/Caddyfile:ro \
//	  -v $PWD/infra/caddy/sites:/etc/caddy/sites:ro caddy:2-alpine
//	CADDY_ADMIN_URL=http://localhost:12019 BELUNE_CADDY_INTEGRATION=1 \
//	  go test -p 1 ./internal/proxy/caddy/ -run RealCaddy_RestartedCaddy -v
//
// It asserts the route ARRAY only, over the admin API: no request is sent, so it
// needs no :80 mapping and cannot be satisfied by an unrelated listener.
//
// Never point this at the dev stack's Caddy: that stack is devcontainer-managed.
func TestRealCaddy_RestartedCaddyDoesNotShadowDomains(t *testing.T) {
	c := liveClient(t)
	ctx := context.Background()

	const host = "shadow-drill.belune.local"
	t.Cleanup(func() { _ = c.RemoveRoute(ctx, host, "/") })

	// Deliberately no InitCatchAll: that runs at API startup only, which is
	// exactly what a Caddy restart does not repeat.
	cfg := routeCfg(host, proxy.SSLModeOff)
	cfg.TLS = false
	require.NoError(t, c.AddRoute(ctx, cfg))

	raw, err := c.fetchRawRoutes(ctx)
	require.NoError(t, err)

	ids := idsOf(t, raw)
	require.NotEmpty(t, ids)
	assert.Contains(t, ids, routeIDFor(host, "/"))
	assert.Equal(t, catchAllRouteID, ids[len(ids)-1])
	for _, r := range raw {
		var p struct {
			ID    string `json:"@id"`
			Match []struct {
				Host []string `json:"host"`
			} `json:"match"`
		}
		require.NoError(t, json.Unmarshal(r, &p))
		if p.ID == catchAllRouteID {
			continue
		}
		require.NotEmpty(t, p.Match, "a match-less route other than the catch-all would answer for every host: %s", r)
		require.NotEmpty(t, p.Match[0].Host)
	}
}
