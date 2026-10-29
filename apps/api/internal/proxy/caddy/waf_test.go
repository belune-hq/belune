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
	"github.com/belune-hq/belune/internal/waf"
)

func TestBuildWAFRoute_Shape(t *testing.T) {
	r := buildWAFRoute([]string{"a.example.com", "b.example.com"}, nil, nil)
	b, err := json.Marshal(r)
	require.NoError(t, err)

	var got struct {
		ID       string `json:"@id"`
		Terminal *bool  `json:"terminal"`
		Match    []struct {
			Host []string `json:"host"`
		} `json:"match"`
		Handle []map[string]any `json:"handle"`
	}
	require.NoError(t, json.Unmarshal(b, &got))

	assert.Equal(t, "route-waf", got.ID)
	assert.Nil(t, got.Terminal, "it must not be terminal: the application's own route has to run after it")
	require.Len(t, got.Match, 1)
	assert.Equal(t, []string{"a.example.com", "b.example.com"}, got.Match[0].Host)

	require.Len(t, got.Handle, 3)
	assert.Equal(t, "headers", got.Handle[0]["handler"], "the id is written before the module reads it")
	assert.Equal(t, "waf", got.Handle[1]["handler"])
	assert.Equal(t, "headers", got.Handle[2]["handler"], "and removed before the application sees it")

	set := got.Handle[0]["request"].(map[string]any)["set"].(map[string]any)
	assert.Equal(t, []any{"{http.request.host}_{http.request.uuid}"}, set[waf.TxHeader],
		"SET, not add: a client cannot supply its own")
	assert.Equal(t, waf.TxHeader, got.Handle[1]["tx_id_req_header"], "the module reads the id from the header Belune writes")
	assert.Equal(t, true, got.Handle[1]["load_owasp_crs"])
	assert.Equal(t, []any{waf.TxHeader}, got.Handle[2]["request"].(map[string]any)["delete"])
}

// Every line of the directives is there because the module's own defaults were
// measured to break traffic or leak, so each is pinned.
func TestWAFDirectives_AreDetectionOnlyAndSafeInFrontOfAnyApplication(t *testing.T) {
	var engine []string
	have := map[string]bool{}
	for _, l := range strings.Split(strings.TrimSpace(wafDirectives), "\n") {
		have[l] = true
		if strings.HasPrefix(l, "SecRuleEngine ") {
			engine = append(engine, l)
		}
	}

	// Detection only, stated once and LAST: nothing an include says can switch it on.
	assert.Equal(t, []string{"SecRuleEngine DetectionOnly"}, engine)
	lines := strings.Split(strings.TrimSpace(wafDirectives), "\n")
	assert.Equal(t, "SecRuleEngine DetectionOnly", lines[len(lines)-1])

	for _, must := range []string{
		"SecResponseBodyAccess Off", // buffering responses has no place in front of arbitrary applications
		"SecAuditEngine Off",        // the recommended audit log records headers and bodies
		"SecDebugLogLevel 1",        // otherwise several WARN lines per request, clean ones too
		"SecRequestBodyLimitAction ProcessPartial",
		// Each of these two body protections was measured, on its own, to be enough to
		// keep a large upload working; against the module's 12.5 MiB default limit
		// and no gating rules, sized and chunked bodies both hang. Pin both, so
		// neither is removed as redundant.
		"SecRequestBodyLimit 131072",
	} {
		assert.True(t, have[must], "missing %q", must)
	}

	// The second protection: a body over about 1 MiB, or one of unknown length,
	// stalls and truncates in the module, so it is never handed to it.
	body := 0
	for _, l := range lines {
		if strings.HasPrefix(l, "SecRule ") && strings.Contains(l, "ctl:requestBodyAccess=Off") {
			body++
		}
	}
	assert.Equal(t, 3, body, "no Content-Length, a large one, and Transfer-Encoding each turn body inspection off")
	assert.Contains(t, wafDirectives, `"@gt 131072"`)
}

func TestWAFHosts_AreNormalisedSoThePassesCompareEqual(t *testing.T) {
	assert.Equal(t, []string{"a.example.com", "b.example.com"},
		wafHosts([]string{"B.Example.com", " a.example.com ", "b.example.com", "", "A.EXAMPLE.COM"}))
	assert.Empty(t, wafHosts(nil))
}

// adminStub is a Caddy admin API that remembers one route by @id and the routes array.
type adminStub struct {
	mu     sync.Mutex
	routes []json.RawMessage
	calls  []string
	reject bool
}

func (a *adminStub) route(id string) (json.RawMessage, int) {
	for i, r := range a.routes {
		var p struct {
			ID string `json:"@id"`
		}
		_ = json.Unmarshal(r, &p)
		if p.ID == id {
			return r, i
		}
	}
	return nil, -1
}

func (a *adminStub) handler(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	defer a.mu.Unlock()
	body, _ := io.ReadAll(r.Body)
	a.calls = append(a.calls, r.Method+" "+r.URL.Path)

	if a.reject && r.Method != http.MethodGet {
		http.Error(w, `{"error":"loading config: unknown module"}`, http.StatusInternalServerError)
		return
	}
	switch {
	case strings.HasPrefix(r.URL.Path, "/id/"):
		id := strings.TrimPrefix(r.URL.Path, "/id/")
		cur, idx := a.route(id)
		switch r.Method {
		case http.MethodGet:
			if idx < 0 {
				http.Error(w, `{"error":"unknown object ID '`+id+`'"}`, http.StatusInternalServerError)
				return
			}
			_, _ = w.Write(cur)
		case http.MethodPatch:
			a.routes[idx] = body
		case http.MethodDelete:
			a.routes = append(a.routes[:idx], a.routes[idx+1:]...)
		}
	case r.URL.Path == "/config/apps/http/servers/srv0":
		// The server object, as InitCatchAll reads and rewrites it: only its routes
		// matter here.
		switch r.Method {
		case http.MethodGet:
			_ = json.NewEncoder(w).Encode(map[string]any{"routes": a.routes})
		case http.MethodPatch:
			var srv struct {
				Routes []json.RawMessage `json:"routes"`
			}
			_ = json.Unmarshal(body, &srv)
			if srv.Routes != nil {
				a.routes = srv.Routes
			}
		}
	case r.URL.Path == "/config/apps/http/servers/srv0/routes/0" && r.Method == http.MethodPut:
		a.routes = append([]json.RawMessage{body}, a.routes...)
	case r.URL.Path == "/config/apps/http/servers/srv0/routes":
		switch r.Method {
		case http.MethodGet:
			_ = json.NewEncoder(w).Encode(a.routes)
		case http.MethodPost:
			a.routes = append(a.routes, body)
		case http.MethodPatch:
			var next []json.RawMessage
			_ = json.Unmarshal(body, &next)
			a.routes = next
		}
	}
}

func newAdminStub(t *testing.T, routes ...string) (*Client, *adminStub) {
	t.Helper()
	a := &adminStub{}
	for _, r := range routes {
		a.routes = append(a.routes, json.RawMessage(r))
	}
	srv := httptest.NewServer(http.HandlerFunc(a.handler))
	t.Cleanup(srv.Close)
	return New(srv.URL), a
}

func (a *adminStub) writes() []string {
	var out []string
	for _, c := range a.calls {
		if !strings.HasPrefix(c, "GET ") {
			out = append(out, c)
		}
	}
	return out
}

func TestSyncWAF_WritesOnlyWhatDiffers(t *testing.T) {
	ctx := context.Background()
	c, a := newAdminStub(t,
		`{"@id":"route-app.example.com","match":[{"host":["app.example.com"]}],"handle":[{"handler":"reverse_proxy"}]}`,
		`{"@id":"route-catch-all","handle":[{"handler":"reverse_proxy"}]}`)

	// Created at index 0: routes are first-match-wins and the application's
	// reverse_proxy ends the chain, so it must come before them.
	require.NoError(t, c.SyncWAF(ctx, proxy.WAFConfig{Enabled: true, Hosts: []string{"app.example.com"}}))
	assert.Equal(t, []string{"PUT /config/apps/http/servers/srv0/routes/0"}, a.writes())
	_, idx := a.route("route-waf")
	assert.Equal(t, 0, idx)

	// The same config again is a GET and nothing else: a write is a config reload.
	a.calls = nil
	require.NoError(t, c.SyncWAF(ctx, proxy.WAFConfig{Enabled: true, Hosts: []string{"APP.example.com", "app.example.com"}}))
	assert.Empty(t, a.writes(), "case and duplicates in the host list are not drift")

	// A new application: replaced where it stands, so it stays first.
	require.NoError(t, c.SyncWAF(ctx, proxy.WAFConfig{Enabled: true, Hosts: []string{"app.example.com", "new.example.com"}}))
	assert.Equal(t, []string{"PATCH /id/route-waf"}, a.writes())
	_, idx = a.route("route-waf")
	assert.Equal(t, 0, idx)

	// Switched off: removed.
	a.calls = nil
	require.NoError(t, c.SyncWAF(ctx, proxy.WAFConfig{Enabled: false, Hosts: []string{"app.example.com"}}))
	assert.Equal(t, []string{"DELETE /id/route-waf"}, a.writes())
	_, idx = a.route("route-waf")
	assert.Equal(t, -1, idx)

	// Off and already absent: nothing.
	a.calls = nil
	require.NoError(t, c.SyncWAF(ctx, proxy.WAFConfig{Enabled: false}))
	assert.Empty(t, a.writes())
}

func TestSyncWAF_NoApplicationsMeansNoRoute(t *testing.T) {
	c, a := newAdminStub(t, `{"@id":"route-catch-all","handle":[{"handler":"reverse_proxy"}]}`)
	require.NoError(t, c.SyncWAF(context.Background(), proxy.WAFConfig{Enabled: true}))
	assert.Empty(t, a.writes(), "an empty host matcher is not a route worth having")
}

func TestSyncWAF_ARefusalIsAnError(t *testing.T) {
	c, a := newAdminStub(t, `{"@id":"route-catch-all","handle":[{"handler":"reverse_proxy"}]}`)
	a.reject = true
	err := c.SyncWAF(context.Background(), proxy.WAFConfig{Enabled: true, Hosts: []string{"app.example.com"}})
	require.Error(t, err, "Caddy rejects the whole write when the module is absent; the reconciler must hear about it")
	assert.Contains(t, err.Error(), "unknown module")
}

// The WAF route matches the application hosts, so by its matcher it would sort in
// among them; and read as an application route it would look like one with no target.
func TestWAFRoute_StaysFirstAndIsNotAnApplicationRoute(t *testing.T) {
	ctx := context.Background()
	// The dashboard's route has a host that sorts BEFORE the applications'. Without
	// it this test would pass even if the WAF route were sorted like any other,
	// because its own first host happens to sort first.
	c, a := newAdminStub(t,
		`{"@id":"route-dashboard","match":[{"host":["0.dash.example.com"]}],"handle":[{"handler":"reverse_proxy","upstreams":[{"dial":"belune:8080"}]}]}`,
		`{"@id":"route-b.example.com","match":[{"host":["b.example.com"]}],"handle":[{"handler":"reverse_proxy","upstreams":[{"dial":"b:80"}]}]}`,
		`{"@id":"route-a.example.com","match":[{"host":["a.example.com"]}],"handle":[{"handler":"reverse_proxy","upstreams":[{"dial":"a:80"}]}]}`,
		`{"@id":"route-catch-all","handle":[{"handler":"reverse_proxy"}]}`)
	require.NoError(t, c.SyncWAF(ctx, proxy.WAFConfig{Enabled: true, Hosts: []string{"a.example.com", "b.example.com"}}))

	// Any later route change reorders the array.
	require.NoError(t, c.reorderRoutes(ctx))
	var ids []string
	for _, r := range a.routes {
		var p struct {
			ID string `json:"@id"`
		}
		_ = json.Unmarshal(r, &p)
		ids = append(ids, p.ID)
	}
	assert.Equal(t, []string{"route-waf", "route-dashboard", "route-a.example.com", "route-b.example.com", "route-catch-all"}, ids,
		"the WAF first, then the rest by specificity, the catch-all last")

	routes, err := c.ListRoutes(ctx)
	require.NoError(t, err)
	var hosts []string
	for _, r := range routes {
		hosts = append(hosts, r.Hostname)
	}
	assert.Equal(t, []string{"0.dash.example.com", "a.example.com", "b.example.com"}, hosts,
		"the reconciler must not see the WAF route as an application route")
}

// The join between a detection and the request behind it rests on one thing: the
// WAF route interpolating {http.request.uuid}. Referencing that placeholder is
// what makes Caddy mint a request id AND write it as the top-level `uuid` of the
// access entry, which the log tailer stores as request_logs.request_uuid and
// migration 000073 joins waf_events.tx_id to.
//
// Nothing fails loudly if this stops: Coraza falls back to a random transaction id
// of its own, the access log simply omits `uuid`, and every detection renders with
// no request detail and no error anywhere. So the placeholder is asserted here
// rather than left to be noticed.
func TestWAFRoute_CarriesTheRequestUUIDTheRequestLogJoinsOn(t *testing.T) {
	b, err := json.Marshal(buildWAFRoute([]string{"a.example.com"}, nil, nil))
	require.NoError(t, err)

	var got struct {
		Handle []struct {
			Handler string `json:"handler"`
			Request struct {
				Set map[string][]string `json:"set"`
			} `json:"request"`
		} `json:"handle"`
	}
	require.NoError(t, json.Unmarshal(b, &got))
	require.NotEmpty(t, got.Handle)

	values := got.Handle[0].Request.Set[waf.TxHeader]
	require.Len(t, values, 1, "the transaction id header must be set exactly once")
	assert.Contains(t, values[0], "{http.request.uuid}",
		"the tx id must interpolate {http.request.uuid}: that is what makes Caddy log `uuid` on the access entry, which is the only key joining a detection to its request")

	// The uuid must be the tail, because splitTxID reads it back as the part after
	// the last underscore and request_logs stores Caddy's bare uuid.
	assert.True(t, strings.HasSuffix(values[0], "_{http.request.uuid}"),
		"splitTxID takes the uuid as the segment after the last underscore, so it must end the value; got %q", values[0])
}
