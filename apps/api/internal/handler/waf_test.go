package handler_test

import (
	"context"
	"fmt"
	"net/http"
	"net/netip"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/belune-hq/belune/internal/proxy"
	"github.com/belune-hq/belune/internal/proxy/capability"
	"github.com/belune-hq/belune/internal/store/generated"
	"github.com/belune-hq/belune/internal/testutil"
	"github.com/belune-hq/belune/internal/waf"
)

var wafCaddy = capability.NewSet("http.handlers.reverse_proxy", capability.ModuleWAF)

func wafFixture(t *testing.T) (token string) {
	t.Helper()
	resetDB(t)
	token = env.SetupAdmin(t, "admin@test.com", "password123")
	t.Cleanup(func() {
		env.Capabilities.Set(capability.Set{})
		env.Reconciler.ReconcileNowCalls = 0
		env.Reconciler.WAF_ = proxy.WAFState{}
		env.WAF.Set(waf.Stats{})
		_, _ = env.Pool.Exec(context.Background(), `DELETE FROM settings WHERE key = $1`, proxy.SettingWAFEnabled)
	})
	return token
}

func wafStatus(t *testing.T, token string) map[string]any {
	t.Helper()
	resp := env.DoRequest(t, "GET", "/api/maintenance/proxy/waf", nil, testutil.AuthHeader(token))
	require.Equal(t, http.StatusOK, resp.StatusCode)
	return testutil.ReadJSON(t, resp)
}

func putWAF(t *testing.T, token string, body any) *http.Response {
	t.Helper()
	return env.DoRequest(t, "PUT", "/api/maintenance/proxy/waf", body, testutil.AuthHeader(token))
}

func wafSetting(t *testing.T) (string, bool) {
	t.Helper()
	s, err := env.Queries.GetSetting(context.Background(), proxy.SettingWAFEnabled)
	if err != nil {
		return "", false
	}
	return s.Value, true
}

// available, unavailable and unknown are three different answers. Telling someone
// whose Caddy could not be asked that their image lacks the module sends them to
// rebuild an image that was fine.
func TestWAFStatus_KeepsTheThreeCapabilityStatesApart(t *testing.T) {
	token := wafFixture(t)

	for name, tc := range map[string]struct {
		set   capability.Set
		state string
	}{
		"could not list modules":  {capability.Set{}, "unknown"},
		"a Caddy without the WAF": {stockCaddy, "unavailable"},
		"the Belune Caddy":        {wafCaddy, "available"},
	} {
		env.Capabilities.Set(tc.set)
		c := wafStatus(t, token)["capability"].(map[string]any)
		assert.Equal(t, tc.state, c["state"], name)
		assert.Equal(t, "http.handlers.waf", c["module"], name)
	}
}

func TestWAFStatus_ReportsTheSwitchTheModeAndWhatTheProxyDid(t *testing.T) {
	token := wafFixture(t)
	env.Capabilities.Set(wafCaddy)

	st := wafStatus(t, token)
	assert.Equal(t, false, st["enabled"], "off until an operator turns it on")
	assert.Equal(t, "detection_only", st["mode"])
	assert.EqualValues(t, waf.DefaultRetentionDays, st["retention_days"])
	assert.EqualValues(t, waf.MaxEvents, st["max_events"])

	env.Reconciler.WAF_ = proxy.WAFState{Active: true, Hosts: 3}
	env.WAF.Set(waf.Stats{Recorded: 40, Dropped: 2, Malformed: 1, LastError: "connection refused"})
	state := wafStatus(t, token)["state"].(map[string]any)
	assert.Equal(t, true, state["active"])
	assert.EqualValues(t, 3, state["hosts"])
	rec := wafStatus(t, token)["recorder"].(map[string]any)
	assert.EqualValues(t, 40, rec["recorded"])
	assert.EqualValues(t, 2, rec["dropped"], "events lost under load are visible, not inferred from a low count")
	assert.EqualValues(t, 1, rec["malformed"])
	assert.Equal(t, "connection refused", rec["last_error"])
}

func TestWAFUpdate_SavesTheSwitchAndAppliesItNow(t *testing.T) {
	token := wafFixture(t)
	env.Capabilities.Set(wafCaddy)

	resp := putWAF(t, token, map[string]any{"enabled": true})
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, true, testutil.ReadJSON(t, resp)["enabled"])
	v, ok := wafSetting(t)
	require.True(t, ok)
	assert.Equal(t, "true", v)
	assert.Equal(t, 1, env.Reconciler.ReconcileNowCalls, "applied now, not on the next 30-second pass")

	resp = putWAF(t, token, map[string]any{"enabled": false})
	require.Equal(t, http.StatusOK, resp.StatusCode)
	resp.Body.Close()
	v, _ = wafSetting(t)
	assert.Equal(t, "false", v)
	assert.Equal(t, 2, env.Reconciler.ReconcileNowCalls)
}

func TestWAFUpdate_RefusesOnlyACaddyKnownToLackTheModule(t *testing.T) {
	token := wafFixture(t)

	// Known to be missing: refused, and nothing is saved.
	env.Capabilities.Set(stockCaddy)
	resp := putWAF(t, token, map[string]any{"enabled": true})
	assert.Equal(t, http.StatusConflict, resp.StatusCode)
	resp.Body.Close()
	_, saved := wafSetting(t)
	assert.False(t, saved, "a refused switch is not stored")

	// Could not ask: accepted. Refusing would tell someone their image lacks a
	// module it may well have; the reconciler applies it once Caddy answers.
	env.Capabilities.Set(capability.Set{})
	resp = putWAF(t, token, map[string]any{"enabled": true})
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	resp.Body.Close()
	v, _ := wafSetting(t)
	assert.Equal(t, "true", v)

	// Turning it OFF is never refused, whatever Caddy has.
	env.Capabilities.Set(stockCaddy)
	resp = putWAF(t, token, map[string]any{"enabled": false})
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	resp.Body.Close()
}

func TestWAFUpdate_ValidatesTheBody(t *testing.T) {
	token := wafFixture(t)
	env.Capabilities.Set(wafCaddy)
	for name, body := range map[string]any{
		"no enabled field": map[string]any{},
		"a non-boolean":    map[string]any{"enabled": "yes"},
		"not an object":    []string{"x"},
	} {
		resp := putWAF(t, token, body)
		assert.Equal(t, http.StatusBadRequest, resp.StatusCode, name)
		resp.Body.Close()
	}
	_, saved := wafSetting(t)
	assert.False(t, saved)
}

func seedWAFEvents(t *testing.T, rows ...generated.BulkInsertWAFEventsParams) {
	t.Helper()
	_, err := env.Queries.BulkInsertWAFEvents(context.Background(), rows)
	require.NoError(t, err)
}

func evt(ip string, rule int32, host string) generated.BulkInsertWAFEventsParams {
	return generated.BulkInsertWAFEventsParams{
		RecordedAt: pgtype.Timestamptz{Time: time.Now(), Valid: true},
		ClientIp:   netip.MustParseAddr(ip),
		RuleID:     rule,
		Severity:   "critical",
		Message:    "XSS Attack Detected via libinjection",
		Category:   pgtype.Text{String: "attack-xss", Valid: true},
		Host:       pgtype.Text{String: host, Valid: host != ""},
		TxID:       pgtype.Text{String: "tx1", Valid: true},
	}
}

func TestWAFEvents_PagesFiltersAndKeepsAttributionNullable(t *testing.T) {
	token := wafFixture(t)
	var rows []generated.BulkInsertWAFEventsParams
	for i := 0; i < 5; i++ {
		rows = append(rows, evt("203.0.113.9", 941100, "shop.example.com"))
	}
	rows = append(rows, evt("198.51.100.7", 942100, "")) // attribution failed for this one
	seedWAFEvents(t, rows...)

	get := func(q string) map[string]any {
		resp := env.DoRequest(t, "GET", "/api/maintenance/proxy/waf/events"+q, nil, testutil.AuthHeader(token))
		require.Equal(t, http.StatusOK, resp.StatusCode, q)
		return testutil.ReadJSON(t, resp)
	}

	page1 := get("?limit=4")
	ev1 := page1["events"].([]any)
	require.Len(t, ev1, 4)
	require.NotNil(t, page1["next_before"], "there is another page")

	page2 := get("?limit=4&before=" + fmt.Sprintf("%.0f", page1["next_before"]))
	ev2 := page2["events"].([]any)
	assert.Len(t, ev2, 2, "the remainder")
	assert.Nil(t, page2["next_before"], "and no page after it")

	// The unattributed event keeps its row, with host and application null.
	var unattributed map[string]any
	for _, e := range append(ev1, ev2...) {
		if e.(map[string]any)["client_ip"] == "198.51.100.7" {
			unattributed = e.(map[string]any)
		}
	}
	require.NotNil(t, unattributed)
	assert.Nil(t, unattributed["host"])
	assert.Nil(t, unattributed["application_id"])
	assert.Equal(t, "attack-xss", unattributed["category"])

	assert.Len(t, get("?client_ip=203.0.113.9")["events"], 5)
	assert.Len(t, get("?rule_id=942100")["events"], 1)
	assert.Len(t, get("?client_ip=::ffff:203.0.113.9")["events"], 5, "a mapped IPv4 address is the same client")
}

func TestWAFEvents_RejectsBadQueries(t *testing.T) {
	token := wafFixture(t)
	for _, q := range []string{"?limit=0", "?limit=201", "?limit=x", "?before=abc", "?before=0", "?client_ip=nope", "?rule_id=x", "?rule_id=-1"} {
		resp := env.DoRequest(t, "GET", "/api/maintenance/proxy/waf/events"+q, nil, testutil.AuthHeader(token))
		assert.Equal(t, http.StatusBadRequest, resp.StatusCode, q)
		resp.Body.Close()
	}
}

func TestWAFStatus_SummarisesTheLastDay(t *testing.T) {
	token := wafFixture(t)
	old := evt("192.0.2.1", 1, "x")
	old.RecordedAt = pgtype.Timestamptz{Time: time.Now().Add(-72 * time.Hour), Valid: true}
	seedWAFEvents(t, evt("203.0.113.9", 941100, "a"), evt("203.0.113.9", 941100, "a"), evt("198.51.100.7", 942100, "b"), old)

	sum := wafStatus(t, token)["summary"].(map[string]any)
	assert.EqualValues(t, 24, sum["window_hours"])
	assert.EqualValues(t, 3, sum["events"], "older events are outside the window")
	assert.EqualValues(t, 2, sum["clients"])
	top := sum["top_rules"].([]any)
	require.Len(t, top, 2)
	assert.EqualValues(t, 941100, top[0].(map[string]any)["rule_id"])
	assert.Equal(t, "203.0.113.9", sum["top_clients"].([]any)[0].(map[string]any)["client_ip"])
}

// What the panel shows an admin. The recorder reads the rule-match lines from
// Caddy's stderr, so they cannot be removed there; they are made safe where an
// admin would read them. These are real lines, with the payload and URI in them.
func TestPlatformLogs_WAFLinesAreRedactedForTheAdmin(t *testing.T) {
	token := wafFixture(t)
	prev := env.Config.CaddyContainerName
	env.Config.CaddyContainerName = "belune-caddy-1"
	t.Cleanup(func() {
		env.Config.CaddyContainerName = prev
		env.Runtime.ContainerLogsTail_ = ""
	})

	raw, err := os.ReadFile("../waf/testdata/caddy-coraza-crs-4.25.0.jsonl")
	require.NoError(t, err)
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	require.Contains(t, string(raw), "<script", "the fixture must carry a payload for this to prove anything")
	env.Runtime.ContainerLogsTail_ = dockerLogFrames(lines...)

	resp := env.DoRequest(t, "GET", "/api/maintenance/logs?service=caddy", nil, testutil.AuthHeader(token))
	require.Equal(t, http.StatusOK, resp.StatusCode)
	content := testutil.ReadJSON(t, resp)["content"].(string)

	for _, leak := range []string{"<script", "alert(", "%3C", "/s?q", "Matched Data", "[data", "[uri"} {
		assert.NotContains(t, content, leak)
	}
	assert.Contains(t, content, "WAF rule 941100 (critical) matched")
	assert.Contains(t, content, "storage cleaning happened too recently", "every other line is untouched")
}
