package caddy

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/belune-hq/belune/internal/proxy"
	"github.com/belune-hq/belune/internal/proxy/caddyconfig"
)

const testBouncerKey = "S3cret+bouncer/key=1234567890abcdefghijklmnopq"

func csConfig(hosts ...string) proxy.CrowdSecConfig {
	return proxy.CrowdSecConfig{Enabled: true, APIURL: "http://crowdsec:8080", APIKey: testBouncerKey, Hosts: hosts}
}

func TestBuildCrowdSecApp_NeverLetsAnUnreachableAPIKillCaddy(t *testing.T) {
	app := buildCrowdSecApp(csConfig("a.example.com"))
	assert.Equal(t, "http://crowdsec:8080", app["api_url"])
	assert.Equal(t, testBouncerKey, app["api_key"])
	assert.Equal(t, true, app["enable_streaming"], "decisions are held in memory, so a request never waits on the local API")
	assert.Equal(t, "5s", app["ticker_interval"])
	// Stated, not left to the default: true makes Caddy QUIT when the local API is
	// unreachable, turning a restarting CrowdSec container into an outage of every site.
	hard, present := app["enable_hard_fails"]
	assert.True(t, present, "false must be written out rather than relied on")
	assert.Equal(t, false, hard)
	assert.NotContains(t, app, "appsec_url", "AppSec is a different feature")
	assert.NotContains(t, app, "metrics_interval", "no usage metrics are pushed to the local API")
}

// The Proxy page shows Caddy's running configuration, and the bouncer's key is in
// it. Redaction is by key NAME, so this pins that the name the module reads is one
// the viewer hides.
func TestBuildCrowdSecApp_TheKeyIsRedactedInTheConfigView(t *testing.T) {
	cfg := map[string]any{"apps": map[string]any{"crowdsec": buildCrowdSecApp(csConfig("a.example.com"))}}
	raw, err := json.Marshal(cfg)
	require.NoError(t, err)

	out, n, err := caddyconfig.Redact(raw)
	require.NoError(t, err)
	shown, err := json.Marshal(out)
	require.NoError(t, err)

	assert.NotContains(t, string(shown), testBouncerKey, "the bouncer key must not render in a browser")
	assert.Equal(t, 1, n)
	assert.Contains(t, string(shown), "http://crowdsec:8080", "everything else stays visible")
}

func TestBuildCrowdSecRoute_Shape(t *testing.T) {
	b, err := json.Marshal(buildCrowdSecRoute([]string{"a.example.com", "b.example.com"}, nil))
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
	assert.Equal(t, "route-crowdsec", got.ID)
	assert.Nil(t, got.Terminal, "it must not be terminal: a request that passes goes on to the application")
	require.Len(t, got.Match, 1)
	assert.Equal(t, []string{"a.example.com", "b.example.com"}, got.Match[0].Host)
	require.Len(t, got.Handle, 1)
	assert.Equal(t, "crowdsec", got.Handle[0]["handler"])
	assert.NotContains(t, string(b), `"not"`, "with nothing to exempt the matcher is the plain host list")
}

// A wildcard host covers the dashboard when the dashboard is served on a name
// under it. The exemption has to be Caddy's to apply, so it is a `not` matcher.
func TestBuildCrowdSecRoute_ExemptsTheDashboardFromAWildcard(t *testing.T) {
	b, err := json.Marshal(buildCrowdSecRoute([]string{"*.example.com"}, []string{"dash.example.com"}))
	require.NoError(t, err)
	assert.JSONEq(t, `{"@id":"route-crowdsec","handle":[{"handler":"crowdsec"}],
		"match":[{"host":["*.example.com"],"not":[{"host":["dash.example.com"]}]}]}`, string(b))
}

// caddyStub is a Caddy admin API that enforces the two ordering rules that were
// measured against a real Caddy:
//
//   - a route that names the crowdsec handler is refused while the crowdsec app is
//     not configured, and
//   - the crowdsec app cannot be deleted while a route still names the handler.
//
// Both refusals are whole-config and change nothing, as Caddy's are.
type caddyStub struct {
	mu     sync.Mutex
	routes []json.RawMessage
	app    json.RawMessage
	calls  []string
}

func (a *caddyStub) routeIndex(id string) int {
	for i, r := range a.routes {
		var p struct {
			ID string `json:"@id"`
		}
		_ = json.Unmarshal(r, &p)
		if p.ID == id {
			return i
		}
	}
	return -1
}

func (a *caddyStub) usesHandler(raw []byte) bool {
	return strings.Contains(string(raw), `"handler":"crowdsec"`)
}

func (a *caddyStub) refuse(w http.ResponseWriter, why string) {
	http.Error(w, `{"error":"loading new config: `+why+`"}`, http.StatusInternalServerError)
}

func (a *caddyStub) handler(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	defer a.mu.Unlock()
	body, _ := io.ReadAll(r.Body)
	a.calls = append(a.calls, r.Method+" "+r.URL.Path)

	switch {
	case r.URL.Path == "/config/apps/crowdsec":
		switch r.Method {
		case http.MethodGet:
			if a.app == nil {
				_, _ = w.Write([]byte("null"))
				return
			}
			_, _ = w.Write(a.app)
		case http.MethodPut:
			if a.app != nil {
				http.Error(w, `{"error":"key already exists: crowdsec"}`, http.StatusConflict)
				return
			}
			a.app = body
		case http.MethodPatch:
			if a.app == nil {
				http.Error(w, `{"error":"key does not exist"}`, http.StatusNotFound)
				return
			}
			a.app = body
		case http.MethodDelete:
			if a.app == nil {
				http.Error(w, `{"error":"key does not exist: crowdsec"}`, http.StatusNotFound)
				return
			}
			for _, rt := range a.routes {
				if a.usesHandler(rt) {
					a.refuse(w, "getting crowdsec app: crowdsec: invalid configuration")
					return
				}
			}
			a.app = nil
		}

	case strings.HasPrefix(r.URL.Path, "/id/"):
		id := strings.TrimPrefix(r.URL.Path, "/id/")
		idx := a.routeIndex(id)
		switch r.Method {
		case http.MethodGet:
			if idx < 0 {
				http.Error(w, `{"error":"unknown object ID '`+id+`'"}`, http.StatusInternalServerError)
				return
			}
			_, _ = w.Write(a.routes[idx])
		case http.MethodPatch:
			if a.usesHandler(body) && a.app == nil {
				a.refuse(w, "getting crowdsec app: invalid configuration")
				return
			}
			a.routes[idx] = body
		case http.MethodDelete:
			a.routes = append(a.routes[:idx], a.routes[idx+1:]...)
		}

	case strings.HasPrefix(r.URL.Path, "/config/apps/http/servers/srv0/routes/") && r.Method == http.MethodPut:
		n, _ := strconv.Atoi(strings.TrimPrefix(r.URL.Path, "/config/apps/http/servers/srv0/routes/"))
		if a.usesHandler(body) && a.app == nil {
			a.refuse(w, "getting crowdsec app: invalid configuration")
			return
		}
		a.routes = append(a.routes[:n], append([]json.RawMessage{body}, a.routes[n:]...)...)

	case r.URL.Path == "/config/apps/http/servers/srv0/routes":
		switch r.Method {
		case http.MethodGet:
			_ = json.NewEncoder(w).Encode(a.routes)
		case http.MethodPatch:
			var next []json.RawMessage
			_ = json.Unmarshal(body, &next)
			a.routes = next
		}
	}
}

func (a *caddyStub) ids() []string {
	var out []string
	for _, r := range a.routes {
		var p struct {
			ID string `json:"@id"`
		}
		_ = json.Unmarshal(r, &p)
		out = append(out, p.ID)
	}
	return out
}

func (a *caddyStub) writes() []string {
	var out []string
	for _, c := range a.calls {
		if !strings.HasPrefix(c, "GET ") {
			out = append(out, c)
		}
	}
	return out
}

func newCaddyStub(t *testing.T, routes ...string) (*Client, *caddyStub) {
	t.Helper()
	a := &caddyStub{}
	for _, r := range routes {
		a.routes = append(a.routes, json.RawMessage(r))
	}
	srv := httptest.NewServer(http.HandlerFunc(a.handler))
	t.Cleanup(srv.Close)
	return New(srv.URL), a
}

const appRoute = `{"@id":"route-app","match":[{"host":["a.example.com"]}],"handle":[{"handler":"reverse_proxy"}]}`

func TestSyncCrowdSec_AppGoesInBeforeTheRouteAndComesOutAfterIt(t *testing.T) {
	ctx := context.Background()
	c, a := newCaddyStub(t, appRoute)

	require.NoError(t, c.SyncCrowdSec(ctx, csConfig("a.example.com")),
		"a route naming the handler without the app is refused whole; the stub refuses it as Caddy does")
	assert.Equal(t, []string{
		"PUT /config/apps/crowdsec",
		"PUT /config/apps/http/servers/srv0/routes/0",
	}, a.writes())
	assert.Equal(t, []string{"route-crowdsec", "route-app"}, a.ids(), "in front of the applications")

	a.calls = nil
	off := csConfig("a.example.com")
	off.Enabled = false
	require.NoError(t, c.SyncCrowdSec(ctx, off), "an app deleted under a route that uses it is refused; the route goes first")
	assert.Equal(t, []string{
		"DELETE /id/route-crowdsec",
		"DELETE /config/apps/crowdsec",
	}, a.writes())
	assert.Equal(t, []string{"route-app"}, a.ids())
	assert.Nil(t, a.app)

	// Switching it off again, with nothing there, writes nothing.
	a.calls = nil
	require.NoError(t, c.SyncCrowdSec(ctx, off))
	assert.Empty(t, a.writes())
}

func TestSyncCrowdSec_WritesOnlyWhatDiffers(t *testing.T) {
	ctx := context.Background()
	c, a := newCaddyStub(t, appRoute)
	require.NoError(t, c.SyncCrowdSec(ctx, csConfig("a.example.com")))

	// Every pass asserts the state; an unchanged one is reads only, because a write
	// is a config reload.
	a.calls = nil
	require.NoError(t, c.SyncCrowdSec(ctx, csConfig("A.example.com", "a.example.com")))
	assert.Empty(t, a.writes())

	a.calls = nil
	rotated := csConfig("a.example.com")
	rotated.APIKey = "a-new-key"
	require.NoError(t, c.SyncCrowdSec(ctx, rotated))
	assert.Equal(t, []string{"PATCH /config/apps/crowdsec"}, a.writes(), "a rotated key replaces the app and leaves the route alone")
	assert.Contains(t, string(a.app), "a-new-key")

	a.calls = nil
	require.NoError(t, c.SyncCrowdSec(ctx, csConfig("a.example.com", "new.example.com")))
	assert.Equal(t, []string{
		"PATCH /config/apps/crowdsec", // back to the original key
		"PATCH /id/route-crowdsec",
	}, a.writes())
	assert.Equal(t, []string{"route-crowdsec", "route-app"}, a.ids(), "replaced where it stands, so it stays first")
}

func TestSyncCrowdSec_NoHostsMeansNothingInCaddy(t *testing.T) {
	ctx := context.Background()
	c, a := newCaddyStub(t, appRoute)
	require.NoError(t, c.SyncCrowdSec(ctx, csConfig("a.example.com")))

	require.NoError(t, c.SyncCrowdSec(ctx, csConfig()))
	assert.Equal(t, []string{"route-app"}, a.ids())
	assert.Nil(t, a.app, "an app with no route is a connection to the local API for nothing")
}

func TestSyncCrowdSec_ErrorsAreRedactedAndStillUnwrap(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if r.Method == http.MethodGet {
			if strings.HasPrefix(r.URL.Path, "/id/") {
				http.Error(w, `{"error":"unknown object ID"}`, http.StatusInternalServerError)
				return
			}
			_, _ = w.Write([]byte("null"))
			return
		}
		http.Error(w, `{"error":"loading new config: bad module config: `+string(body)+`"}`, http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)

	err := New(srv.URL).SyncCrowdSec(context.Background(), csConfig("a.example.com"))
	require.Error(t, err)
	assert.NotContains(t, err.Error(), testBouncerKey, "Caddy's refusals quote the config, which holds the key")
	assert.Contains(t, err.Error(), "[REDACTED]")
	assert.Contains(t, err.Error(), "bad module config")

	// A Caddy that is not there still reads as not there.
	srv.Close()
	err = New(srv.URL).SyncCrowdSec(context.Background(), csConfig("a.example.com"))
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrCaddyUnreachable), "redaction must not hide what kind of failure this is")
}

func TestSyncWAF_GoesBehindTheCrowdSecRoute(t *testing.T) {
	ctx := context.Background()
	c, a := newCaddyStub(t, appRoute)
	require.NoError(t, c.SyncCrowdSec(ctx, csConfig("a.example.com")))

	require.NoError(t, c.SyncWAF(ctx, proxy.WAFConfig{Enabled: true, Hosts: []string{"a.example.com"}}))
	assert.Equal(t, []string{"route-crowdsec", "route-waf", "route-app"}, a.ids(),
		"a banned address is turned away before the WAF has spent anything looking at it")
}

func TestReorderRoutes_PinsCrowdSecThenWAFThenTheApplications(t *testing.T) {
	ctx := context.Background()
	// Deliberately shuffled, and with the catch-all in the middle.
	c, a := newCaddyStub(t,
		appRoute,
		`{"@id":"route-waf","match":[{"host":["a.example.com"]}],"handle":[{"handler":"waf"}]}`,
		`{"@id":"`+catchAllRouteID+`","handle":[{"handler":"reverse_proxy"}]}`,
		`{"@id":"route-crowdsec","match":[{"host":["a.example.com"]}],"handle":[{"handler":"crowdsec"}]}`,
		`{"@id":"route-b","match":[{"host":["b.example.com"]}],"handle":[{"handler":"reverse_proxy"}]}`,
	)
	require.NoError(t, c.reorderRoutes(ctx))
	ids := a.ids()
	require.Len(t, ids, 5)
	assert.Equal(t, []string{"route-crowdsec", "route-waf"}, ids[:2])
	assert.Equal(t, catchAllRouteID, ids[4], "the catch-all stays last")
}

func TestListRoutes_DoesNotReadTheBouncerRouteAsAnApplication(t *testing.T) {
	ctx := context.Background()
	c, _ := newCaddyStub(t, appRoute)
	require.NoError(t, c.SyncCrowdSec(ctx, csConfig("a.example.com", "b.example.com")))

	routes, err := c.ListRoutes(ctx)
	require.NoError(t, err)
	require.Len(t, routes, 1, "it matches every application host; read as an application it would look like a route with no target")
	assert.Equal(t, "a.example.com", routes[0].Hostname)
}
