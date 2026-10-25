package handler_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/belune-hq/belune/internal/crowdsec"
	"github.com/belune-hq/belune/internal/proxy"
	"github.com/belune-hq/belune/internal/proxy/capability"
	"github.com/belune-hq/belune/internal/runtime"
	"github.com/belune-hq/belune/internal/server"
	"github.com/belune-hq/belune/internal/service"
	"github.com/belune-hq/belune/internal/terminal"
	"github.com/belune-hq/belune/internal/testutil"
)

var crowdsecCaddy = capability.NewSet("http.handlers.reverse_proxy", capability.ModuleCrowdSec)

// cscli stands in for the CrowdSec container: replies are matched on the leading
// words of the command, and every call is recorded in order.
type cscli struct {
	mu      sync.Mutex
	calls   [][]string
	replies map[string]func(args []string) (string, error)
}

func newCscli() *cscli {
	c := &cscli{replies: map[string]func([]string) (string, error){}}
	c.reply("lapi status", "ok", nil)
	c.reply("bouncers list", `[{"name":"belune-caddy","last_pull":"2026-10-25T10:00:00Z","version":"v0.14.1"}]`, nil)
	c.reply("decisions list", "[]", nil)
	c.reply("decisions add", "", nil)
	c.reply("decisions delete", "", nil)
	c.reply("allowlists list", "[]", nil)
	c.reply("allowlists create", "", nil)
	c.reply("allowlists add", "", nil)
	c.reply("allowlists remove", "", nil)
	c.reply("bouncers delete", "", nil)
	c.reply("bouncers add", "A-NEW-BOUNCER-KEY", nil)
	return c
}

func (c *cscli) reply(prefix, out string, err error) {
	c.replyFunc(prefix, func([]string) (string, error) { return out, err })
}

func (c *cscli) replyFunc(prefix string, f func([]string) (string, error)) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.replies[prefix] = f
}

func (c *cscli) Run(_ context.Context, args ...string) (string, error) {
	c.mu.Lock()
	c.calls = append(c.calls, append([]string(nil), args...))
	best, f := -1, func([]string) (string, error) { return "", fmt.Errorf("no reply for %v", args) }
	for prefix, fn := range c.replies {
		if strings.HasPrefix(strings.Join(args, " ")+" ", prefix+" ") && len(prefix) > best {
			best, f = len(prefix), fn
		}
	}
	c.mu.Unlock()
	return f(args)
}

func (c *cscli) called(prefix string) [][]string {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out [][]string
	for _, call := range c.calls {
		if strings.HasPrefix(strings.Join(call, " ")+" ", prefix+" ") {
			out = append(out, call)
		}
	}
	return out
}

type csFixture struct {
	t     *testing.T
	url   string
	token string
	cli   *cscli
	svc   *crowdsec.Service
}

// crowdsecFixture serves the shared database through a second server that has the
// CrowdSec service wired (the shared one does not) and a real audit service.
func crowdsecFixture(t *testing.T) *csFixture {
	t.Helper()
	resetDB(t)
	env.Capabilities.Set(crowdsecCaddy)
	env.Reconciler.ReconcileNowCalls = 0
	env.Reconciler.CrowdSec_ = proxy.CrowdSecState{}
	t.Cleanup(func() {
		env.Capabilities.Set(capability.Set{})
		env.Reconciler.CrowdSec_ = proxy.CrowdSecState{}
	})

	auditSvc := service.NewAuditService(env.Queries)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go auditSvc.Run(ctx)

	f := &csFixture{t: t, cli: newCscli()}
	f.svc = crowdsec.NewService(env.Queries, crowdsec.NewController(f.cli), env.Config.Keyring, auditSvc)

	srv := server.New(env.Config, env.Pool, env.Queries, env.Asynq, env.Inspector,
		runtime.NewLocalRuntimes(env.Runtime), env.Proxy, env.Reconciler, env.Redis,
		nil, auditSvc, nil, terminal.NewManager(2), nil)
	srv.SetCapabilities(env.Capabilities)
	srv.SetCrowdSec(f.svc)
	ts := httptest.NewServer(srv.Router())
	t.Cleanup(ts.Close)
	f.url = ts.URL

	f.token = env.SetupAdmin(t, "admin@test.com", "password123")
	// The login above ran before this server existed in the shared harness; the
	// fixture's own server has seen no logins yet.
	f.cli.mu.Lock()
	f.cli.calls = nil
	f.cli.mu.Unlock()
	return f
}

func (f *csFixture) do(method, path string, body any, extra ...map[string]string) (*http.Response, []byte) {
	f.t.Helper()
	var r io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		require.NoError(f.t, err)
		r = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, f.url+path, r)
	require.NoError(f.t, err)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range testutil.AuthHeader(f.token) {
		req.Header.Set(k, v)
	}
	req.Header.Set("X-Forwarded-For", "203.0.113.50")
	for _, h := range extra {
		for k, v := range h {
			req.Header.Set(k, v)
		}
	}
	resp, err := http.DefaultClient.Do(req)
	require.NoError(f.t, err)
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return resp, b
}

func (f *csFixture) json(method, path string, body any) (int, map[string]any) {
	f.t.Helper()
	resp, b := f.do(method, path, body)
	var out map[string]any
	_ = json.Unmarshal(b, &out)
	return resp.StatusCode, out
}

func (f *csFixture) enable() {
	f.t.Helper()
	code, _ := f.json("PUT", "/api/maintenance/proxy/crowdsec", map[string]any{"enabled": true})
	require.Equal(f.t, http.StatusOK, code)
}

// awaitAudit waits for the audit log to hold exactly these actions, in order. The
// audit service writes in the background, so a read straight after the request can
// see fewer rows than the handler queued.
func (f *csFixture) awaitAudit(want ...string) {
	f.t.Helper()
	var got []string
	ok := assert.Eventually(f.t, func() bool {
		rows, err := env.Pool.Query(context.Background(), `SELECT action FROM audit_logs WHERE resource_type IN ('crowdsec','proxy') ORDER BY created_at, id`)
		if err != nil {
			return false
		}
		defer rows.Close()
		got = nil
		for rows.Next() {
			var a string
			_ = rows.Scan(&a)
			got = append(got, a)
		}
		return fmt.Sprint(got) == fmt.Sprint(want)
	}, 3*time.Second, 10*time.Millisecond)
	if !ok {
		f.t.Fatalf("audit log holds %v, want %v", got, want)
	}
}

func (f *csFixture) auditDetails(action string) map[string]any {
	f.t.Helper()
	var raw []byte
	require.Eventually(f.t, func() bool {
		return env.Pool.QueryRow(context.Background(), `SELECT details FROM audit_logs WHERE action = $1`, action).Scan(&raw) == nil
	}, 2*time.Second, 10*time.Millisecond, "no audit row for %s", action)
	var d map[string]any
	_ = json.Unmarshal(raw, &d)
	return d
}

func TestCrowdSec_AdminOnlyAndNeverNilPanics(t *testing.T) {
	f := crowdsecFixture(t)

	// A member is refused every route.
	resp, _ := f.do("POST", "/api/users", map[string]string{"email": "member@test.com", "password": "password123", "role": "member"})
	require.Equal(t, http.StatusCreated, resp.StatusCode)
	member := env.LoginAs(t, "member@test.com", "password123")
	for _, c := range []struct{ method, path string }{
		{"GET", "/api/maintenance/proxy/crowdsec"},
		{"PUT", "/api/maintenance/proxy/crowdsec"},
		{"GET", "/api/maintenance/proxy/crowdsec/decisions"},
		{"POST", "/api/maintenance/proxy/crowdsec/decisions"},
		{"DELETE", "/api/maintenance/proxy/crowdsec/decisions?target=203.0.113.9"},
		{"GET", "/api/maintenance/proxy/crowdsec/allowlist"},
		{"PUT", "/api/maintenance/proxy/crowdsec/autoban"},
		{"POST", "/api/maintenance/proxy/crowdsec/rotate-key"},
	} {
		resp, _ := f.do(c.method, c.path, map[string]any{}, testutil.AuthHeader(member))
		assert.Equal(t, http.StatusForbidden, resp.StatusCode, c.method+" "+c.path)
	}

	// The shared server never had the service wired.
	r := env.DoRequest(t, "GET", "/api/maintenance/proxy/crowdsec", nil, testutil.AuthHeader(f.token))
	assert.Equal(t, http.StatusServiceUnavailable, r.StatusCode)
	_ = r.Body.Close()
}

func TestCrowdSecStatus_KeepsTheThreeCapabilityStatesApart(t *testing.T) {
	f := crowdsecFixture(t)
	for name, tc := range map[string]struct {
		set   capability.Set
		state string
	}{
		"could not list modules":      {capability.Set{}, "unknown"},
		"a Caddy without the bouncer": {stockCaddy, "unavailable"},
		"the Belune Caddy":            {crowdsecCaddy, "available"},
	} {
		env.Capabilities.Set(tc.set)
		code, st := f.json("GET", "/api/maintenance/proxy/crowdsec", nil)
		require.Equal(t, http.StatusOK, code)
		c := st["capability"].(map[string]any)
		assert.Equal(t, tc.state, c["state"], name)
		assert.Equal(t, "http.handlers.crowdsec", c["module"], name)
	}
}

func TestCrowdSecStatus_DefaultsAndWhatItReports(t *testing.T) {
	f := crowdsecFixture(t)
	env.Reconciler.CrowdSec_ = proxy.CrowdSecState{Active: true, Hosts: 4}

	code, st := f.json("GET", "/api/maintenance/proxy/crowdsec", nil)
	require.Equal(t, http.StatusOK, code)
	assert.Equal(t, false, st["enabled"], "off until an operator turns it on")
	assert.Equal(t, "203.0.113.50", st["your_ip"])
	assert.Equal(t, true, st["state"].(map[string]any)["active"])
	assert.EqualValues(t, 4, st["state"].(map[string]any)["hosts"])

	api := st["local_api"].(map[string]any)
	assert.Equal(t, true, api["reachable"])
	assert.Equal(t, true, api["bouncer_registered"])
	assert.Equal(t, "2026-10-25T10:00:00Z", api["last_pull"])

	ab := st["autoban"].(map[string]any)
	assert.Equal(t, false, ab["policy"].(map[string]any)["enabled"], "a WAF detection never becomes a ban unless the operator opts in")
	cats := ab["categories"].([]any)
	assert.NotContains(t, cats, "attack-protocol")
	assert.Contains(t, cats, "attack-sqli")
}

func TestCrowdSecStatus_ALocalAPIThatIsDownIsReportedNotFailed(t *testing.T) {
	f := crowdsecFixture(t)
	f.cli.reply("bouncers list", "", crowdsec.ErrUnavailable)
	code, st := f.json("GET", "/api/maintenance/proxy/crowdsec", nil)
	require.Equal(t, http.StatusOK, code)
	api := st["local_api"].(map[string]any)
	assert.Equal(t, false, api["reachable"])
	assert.NotEmpty(t, api["error"])
}

func TestCrowdSecUpdate_SavesTheSwitchAllowlistsTheAdminAndAppliesItNow(t *testing.T) {
	f := crowdsecFixture(t)

	code, st := f.json("PUT", "/api/maintenance/proxy/crowdsec", map[string]any{"enabled": true})
	require.Equal(t, http.StatusOK, code)
	assert.Equal(t, true, st["enabled"])
	assert.Equal(t, 1, env.Reconciler.ReconcileNowCalls, "applied now, not on the next 30-second pass")

	adds := f.cli.called("allowlists add")
	require.Len(t, adds, 1)
	assert.Equal(t, "203.0.113.50", adds[0][3], "the address the admin is acting from can never be banned")

	cfg, err := f.svc.Config(context.Background())
	require.NoError(t, err)
	assert.True(t, cfg.Enabled)

	code, st = f.json("PUT", "/api/maintenance/proxy/crowdsec", map[string]any{"enabled": false})
	require.Equal(t, http.StatusOK, code)
	assert.Equal(t, false, st["enabled"])
	assert.Equal(t, 2, env.Reconciler.ReconcileNowCalls)

	f.awaitAudit("enable_crowdsec", "disable_crowdsec")
}

func TestCrowdSecUpdate_RefusesOnlyACaddyKnownToLackTheModule(t *testing.T) {
	f := crowdsecFixture(t)

	env.Capabilities.Set(stockCaddy)
	code, _ := f.json("PUT", "/api/maintenance/proxy/crowdsec", map[string]any{"enabled": true})
	assert.Equal(t, http.StatusConflict, code)
	cfg, err := f.svc.Config(context.Background())
	require.NoError(t, err)
	assert.False(t, cfg.Enabled, "a refused switch is not stored")

	// Could not ask: accepted. Refusing would tell someone their image lacks a
	// module it may well have.
	env.Capabilities.Set(capability.Set{})
	code, _ = f.json("PUT", "/api/maintenance/proxy/crowdsec", map[string]any{"enabled": true})
	assert.Equal(t, http.StatusOK, code)

	// Turning it OFF is never refused, whatever the Caddy.
	env.Capabilities.Set(stockCaddy)
	code, _ = f.json("PUT", "/api/maintenance/proxy/crowdsec", map[string]any{"enabled": false})
	assert.Equal(t, http.StatusOK, code)
}

func TestCrowdSecUpdate_RejectsAMalformedBody(t *testing.T) {
	f := crowdsecFixture(t)
	for _, body := range []any{map[string]any{}, map[string]any{"enabled": "yes"}, "nope"} {
		code, _ := f.json("PUT", "/api/maintenance/proxy/crowdsec", body)
		assert.Equal(t, http.StatusBadRequest, code)
	}
}

func TestCrowdSecDecisions_ListsWithTheirSource(t *testing.T) {
	f := crowdsecFixture(t)
	f.cli.reply("decisions list", `[
	 {"created_at":"2026-10-25T10:00:00Z","decisions":[{"id":1,"duration":"59m","origin":"cscli","scenario":"belune: scanner","scope":"Ip","type":"ban","value":"203.0.113.9"}]},
	 {"created_at":"2026-10-25T10:01:00Z","decisions":[{"id":2,"duration":"23h","origin":"cscli","scenario":"belune-waf: attack-sqli: 14 requests in 10m","scope":"Range","type":"ban","value":"198.51.100.0/24"}]},
	 {"created_at":"2026-10-25T10:02:00Z","decisions":[{"id":3,"duration":"1h","origin":"cscli","scenario":"by hand","scope":"Ip","type":"ban","value":"2001:db8::1"}]}
	]`, nil)

	resp, b := f.do("GET", "/api/maintenance/proxy/crowdsec/decisions", nil)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var got struct {
		Decisions []struct {
			Value, Scope, Source, Reason string
			ExpiresAt                    time.Time `json:"expires_at"`
		}
	}
	require.NoError(t, json.Unmarshal(b, &got))
	require.Len(t, got.Decisions, 3)
	assert.Equal(t, "dashboard", got.Decisions[0].Source)
	assert.Equal(t, "waf", got.Decisions[1].Source)
	assert.Equal(t, "range", got.Decisions[1].Scope)
	assert.Equal(t, "cli", got.Decisions[2].Source)
	assert.True(t, got.Decisions[0].ExpiresAt.After(time.Now().Add(55*time.Minute)))
}

func TestCrowdSecBan(t *testing.T) {
	f := crowdsecFixture(t)

	t.Run("refused while the feature is off", func(t *testing.T) {
		code, body := f.json("POST", "/api/maintenance/proxy/crowdsec/decisions", map[string]any{"target": "203.0.113.9", "duration": "1h"})
		assert.Equal(t, http.StatusConflict, code)
		assert.Contains(t, body["error"], "switched off")
		assert.Empty(t, f.cli.called("decisions add"))
	})

	f.enable()

	t.Run("bans, tags the reason and audits who and what", func(t *testing.T) {
		code, body := f.json("POST", "/api/maintenance/proxy/crowdsec/decisions",
			map[string]any{"target": "203.0.113.9", "duration": "24h", "reason": "  port\nscanner "})
		require.Equal(t, http.StatusCreated, code)
		assert.Equal(t, "203.0.113.9", body["value"])
		assert.Equal(t, "belune: port scanner", body["reason"])

		adds := f.cli.called("decisions add")
		require.Len(t, adds, 1)
		assert.Equal(t, []string{"decisions", "add", "--ip=203.0.113.9", "--type=ban", "--duration=1440m", "--reason=belune: port scanner"}, adds[0])

		d := f.auditDetails("crowdsec_ban")
		assert.Equal(t, "24h", d["duration"])
		var uid, resource string
		require.NoError(t, env.Pool.QueryRow(context.Background(),
			`SELECT user_id::text, resource_id FROM audit_logs WHERE action = 'crowdsec_ban'`).Scan(&uid, &resource))
		assert.NotEmpty(t, uid, "who")
		assert.Equal(t, "203.0.113.9", resource, "what")
	})

	for name, tc := range map[string]struct {
		body map[string]any
		want int
		msg  string
	}{
		"your own address":       {map[string]any{"target": "203.0.113.50", "duration": "1h"}, 400, "connected from"},
		"a range containing you": {map[string]any{"target": "203.0.113.0/24", "duration": "1h"}, 400, "connected from"},
		"loopback":               {map[string]any{"target": "127.0.0.1", "duration": "1h"}, 400, "private"},
		"private":                {map[string]any{"target": "10.1.2.3", "duration": "1h"}, 400, "private"},
		"a continent":            {map[string]any{"target": "8.0.0.0/8", "duration": "1h"}, 400, "wider than"},
		"forever":                {map[string]any{"target": "203.0.113.9", "duration": "999999h"}, 400, "between"},
		"no target":              {map[string]any{"duration": "1h"}, 400, "required"},
		"no duration":            {map[string]any{"target": "203.0.113.9"}, 400, "duration"},
		"not an address":         {map[string]any{"target": "203.0.113.9; rm -rf /", "duration": "1h"}, 400, "not a valid"},
	} {
		t.Run("refuses "+name, func(t *testing.T) {
			before := len(f.cli.called("decisions add"))
			code, body := f.json("POST", "/api/maintenance/proxy/crowdsec/decisions", tc.body)
			assert.Equal(t, tc.want, code)
			assert.Contains(t, fmt.Sprint(body["error"]), tc.msg)
			assert.Len(t, f.cli.called("decisions add"), before, "a refused ban must never reach CrowdSec")
		})
	}

	t.Run("an allowlisted address is a conflict, not a failure", func(t *testing.T) {
		f.cli.reply("decisions add", "", &crowdsec.CommandError{Command: "decisions add", ExitCode: 1, Stderr: "203.0.113.77 is allowlisted by item 203.0.113.77 from belune-admins"})
		code, body := f.json("POST", "/api/maintenance/proxy/crowdsec/decisions", map[string]any{"target": "203.0.113.77", "duration": "1h"})
		assert.Equal(t, http.StatusConflict, code)
		assert.Contains(t, fmt.Sprint(body["error"]), "allowlist")
		f.cli.reply("decisions add", "", nil)
	})

	t.Run("cscli's own error text is not shown", func(t *testing.T) {
		f.cli.reply("decisions add", "", &crowdsec.CommandError{Command: "decisions add", ExitCode: 1, Stderr: "Error: secret internal detail /etc/crowdsec/x"})
		code, body := f.json("POST", "/api/maintenance/proxy/crowdsec/decisions", map[string]any{"target": "203.0.113.78", "duration": "1h"})
		assert.Equal(t, http.StatusBadGateway, code)
		assert.NotContains(t, fmt.Sprint(body["error"]), "/etc/crowdsec")
		f.cli.reply("decisions add", "", nil)
	})
}

// The reverse of every ban must be possible whatever else is wrong.
func TestCrowdSecUnban_IsNeverGatedOnTheFeatureOrTheCaddy(t *testing.T) {
	f := crowdsecFixture(t)
	// Never enabled; the Caddy lacks the module; the local API has the ban.
	env.Capabilities.Set(stockCaddy)

	resp, _ := f.do("DELETE", "/api/maintenance/proxy/crowdsec/decisions?target=203.0.113.9", nil)
	assert.Equal(t, http.StatusNoContent, resp.StatusCode)
	deletes := f.cli.called("decisions delete")
	require.Len(t, deletes, 1)
	assert.Equal(t, "--ip=203.0.113.9", deletes[0][2])
	f.awaitAudit("crowdsec_unban")

	resp, _ = f.do("DELETE", "/api/maintenance/proxy/crowdsec/decisions?target=198.51.100.0%2F24", nil)
	assert.Equal(t, http.StatusNoContent, resp.StatusCode)
	f.awaitAudit("crowdsec_unban", "crowdsec_unban")
	assert.Equal(t, "--range=198.51.100.0/24", f.cli.called("decisions delete")[1][2])

	for _, bad := range []string{"", "nonsense", "1.2.3.4;ls"} {
		resp, _ = f.do("DELETE", "/api/maintenance/proxy/crowdsec/decisions?target="+bad, nil)
		assert.Equal(t, http.StatusBadRequest, resp.StatusCode, bad)
	}
	assert.Len(t, f.cli.called("decisions delete"), 2)
}

func TestCrowdSecAllowlist(t *testing.T) {
	f := crowdsecFixture(t)
	f.cli.reply("allowlists list", `[{"name":"belune-admins","items":[
	  {"value":"192.0.2.10","description":"login: admin@example.com","created_at":"2026-10-25T10:00:00Z","expiration":"2026-11-24T10:00:00Z"}]}]`, nil)

	resp, b := f.do("GET", "/api/maintenance/proxy/crowdsec/allowlist", nil)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var got struct {
		Items []struct{ Value, Description string }
	}
	require.NoError(t, json.Unmarshal(b, &got))
	require.Len(t, got.Items, 1)
	assert.Equal(t, "login: admin@example.com", got.Items[0].Description)

	resp, _ = f.do("POST", "/api/maintenance/proxy/crowdsec/allowlist", map[string]any{"target": "198.51.100.0/24", "duration": "30d", "note": "office"})
	assert.Equal(t, http.StatusNoContent, resp.StatusCode)
	assert.Contains(t, f.cli.called("allowlists add")[0], "office")

	resp, _ = f.do("POST", "/api/maintenance/proxy/crowdsec/allowlist", map[string]any{"target": "nonsense", "duration": "30d"})
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)

	resp, _ = f.do("DELETE", "/api/maintenance/proxy/crowdsec/allowlist?target=192.0.2.10", nil)
	assert.Equal(t, http.StatusNoContent, resp.StatusCode)

	f.awaitAudit("crowdsec_allow", "crowdsec_unallow")
}

func TestCrowdSecAutoban(t *testing.T) {
	f := crowdsecFixture(t)
	good := map[string]any{
		"enabled": true, "threshold": 20, "window_minutes": 15, "duration_minutes": 120, "max_active": 10,
		"categories": []string{"attack-xss", "attack-sqli"},
	}

	code, st := f.json("PUT", "/api/maintenance/proxy/crowdsec/autoban", good)
	require.Equal(t, http.StatusOK, code)
	policy := st["autoban"].(map[string]any)["policy"].(map[string]any)
	assert.Equal(t, true, policy["enabled"])
	assert.EqualValues(t, 20, policy["threshold"])
	assert.Equal(t, []any{"attack-sqli", "attack-xss"}, policy["categories"])

	d := f.auditDetails("crowdsec_autoban_policy")
	assert.Equal(t, true, d["enabled"], "turning WAF detections into bans is a decision the audit log records")

	// What may never be a ban.
	for name, mutate := range map[string]func(m map[string]any){
		"protocol":  func(m map[string]any) { m["categories"] = []string{"attack-protocol"} },
		"scanner":   func(m map[string]any) { m["categories"] = []string{"attack-reputation-scanner"} },
		"threshold": func(m map[string]any) { m["threshold"] = 1 },
		"forever":   func(m map[string]any) { m["duration_minutes"] = 999999 },
		"no cap":    func(m map[string]any) { m["max_active"] = 0 },
		"none":      func(m map[string]any) { m["categories"] = []string{} },
	} {
		body := map[string]any{}
		for k, v := range good {
			body[k] = v
		}
		mutate(body)
		code, _ := f.json("PUT", "/api/maintenance/proxy/crowdsec/autoban", body)
		assert.Equal(t, http.StatusBadRequest, code, name)
	}
}

func TestCrowdSecRotateKey(t *testing.T) {
	f := crowdsecFixture(t)
	f.enable()
	env.Reconciler.ReconcileNowCalls = 0

	code, _ := f.json("POST", "/api/maintenance/proxy/crowdsec/rotate-key", nil)
	require.Equal(t, http.StatusOK, code)
	assert.Len(t, f.cli.called("bouncers delete"), 1)
	assert.Equal(t, []string{"bouncers", "add", "belune-caddy", "-o", "raw"}, f.cli.called("bouncers add")[0])
	assert.Equal(t, 1, env.Reconciler.ReconcileNowCalls, "the new key reaches Caddy now, not in 30 seconds")

	var sealed []byte
	require.NoError(t, env.Pool.QueryRow(context.Background(), `SELECT bouncer_key_encrypted FROM crowdsec_config`).Scan(&sealed))
	assert.NotContains(t, string(sealed), "A-NEW-BOUNCER-KEY", "the key is stored sealed")
	plain, err := env.Config.Keyring.Decrypt(sealed)
	require.NoError(t, err)
	assert.Equal(t, "A-NEW-BOUNCER-KEY", string(plain))

	// It is never in any response.
	_, b := f.do("GET", "/api/maintenance/proxy/crowdsec", nil)
	assert.NotContains(t, string(b), "A-NEW-BOUNCER-KEY")
	f.awaitAudit("enable_crowdsec", "crowdsec_rotate_key")
}

func TestCrowdSec_AMissingContainerSaysWhatToDo(t *testing.T) {
	f := crowdsecFixture(t)
	f.cli.reply("decisions list", "", crowdsec.ErrUnavailable)
	resp, b := f.do("GET", "/api/maintenance/proxy/crowdsec/decisions", nil)
	assert.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)
	assert.Contains(t, string(b), "docker compose up -d")
}

// A login is the moment an admin's address becomes unbannable, and it must never be
// the moment CrowdSec stops someone signing in.
func TestCrowdSecLogin_AllowlistsAnAdminAndNeverFailsTheLogin(t *testing.T) {
	login := func(f *csFixture, email, ip string) *http.Response {
		raw, _ := json.Marshal(map[string]string{"email": email, "password": "password123"})
		req, err := http.NewRequest("POST", f.url+"/api/auth/login", bytes.NewReader(raw))
		require.NoError(t, err)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Forwarded-For", ip)
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		_ = resp.Body.Close()
		return resp
	}
	waitAdds := func(f *csFixture, n int) {
		require.Eventually(t, func() bool { return len(f.cli.called("allowlists add")) == n }, 2*time.Second, 5*time.Millisecond)
	}

	t.Run("an admin's address is allowlisted", func(t *testing.T) {
		f := crowdsecFixture(t)
		f.enable()
		f.cli.mu.Lock()
		f.cli.calls = nil
		f.cli.mu.Unlock()

		assert.Equal(t, http.StatusOK, login(f, "admin@test.com", "198.51.100.23").StatusCode)
		waitAdds(f, 1)
		add := f.cli.called("allowlists add")[0]
		assert.Equal(t, "198.51.100.23", add[3])
		assert.Contains(t, add, "login: admin@test.com")
	})

	t.Run("a member's address is not", func(t *testing.T) {
		f := crowdsecFixture(t)
		f.enable()
		resp, _ := f.do("POST", "/api/users", map[string]string{"email": "member@test.com", "password": "password123", "role": "member"})
		require.Equal(t, http.StatusCreated, resp.StatusCode)
		f.cli.mu.Lock()
		f.cli.calls = nil
		f.cli.mu.Unlock()

		assert.Equal(t, http.StatusOK, login(f, "member@test.com", "198.51.100.24").StatusCode)
		time.Sleep(100 * time.Millisecond)
		assert.Empty(t, f.cli.called("allowlists add"))
	})

	t.Run("nothing happens while the feature is off", func(t *testing.T) {
		f := crowdsecFixture(t)
		assert.Equal(t, http.StatusOK, login(f, "admin@test.com", "198.51.100.25").StatusCode)
		time.Sleep(100 * time.Millisecond)
		assert.Empty(t, f.cli.called("allowlists"))
	})

	t.Run("a CrowdSec that is down does not stop the login", func(t *testing.T) {
		f := crowdsecFixture(t)
		f.enable()
		f.cli.reply("allowlists list", "", crowdsec.ErrUnavailable)
		start := time.Now()
		assert.Equal(t, http.StatusOK, login(f, "admin@test.com", "198.51.100.26").StatusCode)
		assert.Less(t, time.Since(start), 5*time.Second)
	})

	t.Run("a hanging CrowdSec does not delay the login", func(t *testing.T) {
		f := crowdsecFixture(t)
		f.enable()
		release := make(chan struct{})
		t.Cleanup(func() { close(release) })
		f.cli.replyFunc("allowlists list", func([]string) (string, error) { <-release; return "[]", nil })
		start := time.Now()
		assert.Equal(t, http.StatusOK, login(f, "admin@test.com", "198.51.100.27").StatusCode)
		assert.Less(t, time.Since(start), 3*time.Second, "the allowlisting is in the background")
	})
}
