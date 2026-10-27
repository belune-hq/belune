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
	assert.Equal(t, "24h", sum["window"])
	assert.EqualValues(t, 24, sum["window_hours"])
	assert.EqualValues(t, 3, sum["events"], "older events are outside the window")
	assert.EqualValues(t, 4, sum["total"], "but they are still kept, and the total says so")
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

// The summary used to cover a hardcoded 24 hours and the dashboard rendered
// nothing when that came back empty, so a day after an attack the whole summary
// vanished and left a bare table.
func TestWAFStatus_TheSummaryWindowIsSelectable(t *testing.T) {
	token := wafFixture(t)
	at := func(d time.Duration) generated.BulkInsertWAFEventsParams {
		e := evt("203.0.113.9", 941100, "a")
		e.RecordedAt = pgtype.Timestamptz{Time: time.Now().Add(-d), Valid: true}
		return e
	}
	seedWAFEvents(t,
		at(10*time.Minute), // inside every window
		at(6*time.Hour),    // outside 1h
		at(72*time.Hour),   // outside 24h
		at(30*24*time.Hour),
	)

	summary := func(q string) map[string]any {
		resp := env.DoRequest(t, "GET", "/api/maintenance/proxy/waf"+q, nil, testutil.AuthHeader(token))
		require.Equal(t, http.StatusOK, resp.StatusCode, q)
		return testutil.ReadJSON(t, resp)["summary"].(map[string]any)
	}

	for _, tc := range []struct {
		query  string
		window string
		hours  int
		events int
	}{
		{"", "24h", 24, 2}, // the default must not move: it is what v0.2.4 clients get
		{"?window=1h", "1h", 1, 1},
		{"?window=24h", "24h", 24, 2},
		{"?window=7d", "7d", 168, 3},
		{"?window=all", "all", 0, 4},
	} {
		sum := summary(tc.query)
		assert.Equal(t, tc.window, sum["window"], tc.query)
		assert.EqualValues(t, tc.hours, sum["window_hours"], tc.query)
		assert.EqualValues(t, tc.events, sum["events"], tc.query)
		// Whatever the window, the total is every event still kept — that is what
		// lets an empty window say "nothing lately" rather than "nothing ever".
		assert.EqualValues(t, 4, sum["total"], tc.query)
	}
}

func TestWAFStatus_RejectsAWindowItDoesNotOffer(t *testing.T) {
	token := wafFixture(t)
	for _, q := range []string{"?window=30d", "?window=1", "?window=24", "?window=hour", "?window=ALL"} {
		resp := env.DoRequest(t, "GET", "/api/maintenance/proxy/waf"+q, nil, testutil.AuthHeader(token))
		assert.Equal(t, http.StatusBadRequest, resp.StatusCode, q)
		_ = resp.Body.Close()
	}
}

// A detection carries no request data of its own by design (000070), so the URL,
// method, status and content type are joined from the request log by the uuid
// Caddy writes into both the access entry and Coraza's transaction id (000073).
//
// The three cases that matter are all here: a detection whose request is still on
// record, one whose request log has been swept (detections outlive request logs,
// so this is the COMMON case, not an error), and one that never carried a
// transaction id at all.
func TestWAFEvents_JoinTheRequestBehindEachDetection(t *testing.T) {
	token := wafFixture(t)
	project := env.CreateProject(t, token, "Shop", "shop")
	app := env.CreateApplication(t, token, extractID(project["id"]), map[string]any{
		"name":        "Shop",
		"type":        "git",
		"build_type":  "dockerfile",
		"source_repo": "https://github.com/test/shop",
	})

	// The request the WAF inspected: Caddy minted this uuid, the tailer stored it.
	require.NoError(t, env.Queries.InsertRequestLog(context.Background(), generated.InsertRequestLogParams{
		ApplicationID: mustUUID(t, extractID(app["id"])),
		Method:        "POST",
		Path:          "/checkout?step=2",
		StatusCode:    500,
		LatencyMs:     12,
		Hostname:      "shop.example.com",
		ClientIp:      pgtype.Text{String: "203.0.113.9", Valid: true},
		UserAgent:     pgtype.Text{String: "curl/8.7.1", Valid: true},
		ContentType:   pgtype.Text{String: "text/plain", Valid: true},
		RequestUuid:   pgtype.Text{String: "uuid-on-record", Valid: true},
	}))

	// Two rules of ONE request, so the detail is shared and asked for once.
	onRecord := evt("203.0.113.9", 920420, "shop.example.com")
	onRecord.TxID = pgtype.Text{String: "uuid-on-record", Valid: true}
	swept := evt("198.51.100.7", 941100, "shop.example.com")
	swept.TxID = pgtype.Text{String: "uuid-swept-away", Valid: true}
	noTx := evt("198.51.100.8", 941100, "shop.example.com")
	noTx.TxID = pgtype.Text{}
	seedWAFEvents(t, onRecord, onRecord, swept, noTx)

	resp := env.DoRequest(t, "GET", "/api/maintenance/proxy/waf/events", nil, testutil.AuthHeader(token))
	require.Equal(t, http.StatusOK, resp.StatusCode)
	body := testutil.ReadJSON(t, resp)

	byIP := map[string][]map[string]any{}
	for _, e := range body["events"].([]any) {
		ev := e.(map[string]any)
		ip := ev["client_ip"].(string)
		byIP[ip] = append(byIP[ip], ev)
	}

	// On record: both rules of the one request carry the same detail.
	require.Len(t, byIP["203.0.113.9"], 2)
	for _, ev := range byIP["203.0.113.9"] {
		req, ok := ev["request"].(map[string]any)
		require.True(t, ok, "the request behind this detection should be joined")
		assert.Equal(t, "POST", req["method"])
		assert.Equal(t, "/checkout?step=2", req["path"])
		assert.Equal(t, float64(500), req["status"])
		// The field that makes rule 920420 judgeable instead of a shrug.
		assert.Equal(t, "text/plain", req["content_type"])
		assert.Equal(t, "curl/8.7.1", req["user_agent"])
	}

	// Swept, and never-had-a-tx: the detection still renders, with no detail and
	// no error. This is what most of a detection's life looks like.
	require.Len(t, byIP["198.51.100.7"], 1)
	assert.Nil(t, byIP["198.51.100.7"][0]["request"], "a swept request log is absent detail, not a failure")
	require.Len(t, byIP["198.51.100.8"], 1)
	assert.Nil(t, byIP["198.51.100.8"][0]["request"], "an event with no tx_id has nothing to join")

	// The detail must not leak across transaction ids.
	assert.Equal(t, 920420, int(byIP["203.0.113.9"][0]["rule_id"].(float64)))
}

// A request log belonging to a DIFFERENT request must never be attached, however
// close it sits: the uuid is the only thing that may match them up.
func TestWAFEvents_TheJoinIsTheUUIDAndNothingElse(t *testing.T) {
	token := wafFixture(t)
	project := env.CreateProject(t, token, "Shop", "shop")
	app := env.CreateApplication(t, token, extractID(project["id"]), map[string]any{
		"name":        "Shop",
		"type":        "git",
		"build_type":  "dockerfile",
		"source_repo": "https://github.com/test/shop",
	})

	// Same client, same host, same moment — and a different request.
	require.NoError(t, env.Queries.InsertRequestLog(context.Background(), generated.InsertRequestLogParams{
		ApplicationID: mustUUID(t, extractID(app["id"])),
		Method:        "GET",
		Path:          "/someone-elses-request",
		StatusCode:    200,
		Hostname:      "shop.example.com",
		ClientIp:      pgtype.Text{String: "203.0.113.9", Valid: true},
		RequestUuid:   pgtype.Text{String: "a-different-uuid", Valid: true},
	}))

	e := evt("203.0.113.9", 920420, "shop.example.com")
	e.TxID = pgtype.Text{String: "the-detections-uuid", Valid: true}
	seedWAFEvents(t, e)

	resp := env.DoRequest(t, "GET", "/api/maintenance/proxy/waf/events", nil, testutil.AuthHeader(token))
	require.Equal(t, http.StatusOK, resp.StatusCode)
	events := testutil.ReadJSON(t, resp)["events"].([]any)
	require.Len(t, events, 1)
	assert.Nil(t, events[0].(map[string]any)["request"],
		"a request log that shares the client, host and moment but not the uuid is a different request")
}

// --- grouped by client ------------------------------------------------------

func wafClients(t *testing.T, token, query string) map[string]any {
	t.Helper()
	resp := env.DoRequest(t, "GET", "/api/maintenance/proxy/waf/clients"+query, nil, testutil.AuthHeader(token))
	require.Equal(t, http.StatusOK, resp.StatusCode, query)
	return testutil.ReadJSON(t, resp)
}

// groupFor picks one client's group out of the response, so a test does not
// depend on the order the loudest happened to land in.
func groupFor(t *testing.T, body map[string]any, ip string) map[string]any {
	t.Helper()
	for _, c := range body["clients"].([]any) {
		g := c.(map[string]any)
		if g["client_ip"] == ip {
			return g
		}
	}
	t.Fatalf("no group for %s in %v", ip, body["clients"])
	return nil
}

// wafApp is an application for request logs to hang off; the detections
// themselves need none.
func wafApp(t *testing.T, token string) string {
	t.Helper()
	project := env.CreateProject(t, token, "Shop", "shop")
	app := env.CreateApplication(t, token, extractID(project["id"]), map[string]any{
		"name":        "Shop",
		"type":        "git",
		"build_type":  "dockerfile",
		"source_repo": "https://github.com/test/shop",
	})
	return extractID(app["id"])
}

func reqLog(t *testing.T, appID, path, uuid string) {
	t.Helper()
	require.NoError(t, env.Queries.InsertRequestLog(context.Background(), generated.InsertRequestLogParams{
		ApplicationID: mustUUID(t, appID),
		Method:        "GET",
		Path:          path,
		StatusCode:    200,
		Hostname:      "shop.example.com",
		RequestUuid:   pgtype.Text{String: uuid, Valid: true},
	}))
}

func TestWAFClients_CollapseAClientsDetectionsIntoOneRow(t *testing.T) {
	token := wafFixture(t)
	appID := wafApp(t, token)

	// One scanner: two requests, three rule matches between them, two URLs, and
	// one of the two requests would have been refused.
	reqLog(t, appID, "/admin.php", "tx-a")
	reqLog(t, appID, "/?q=%3Cscript%3E", "tx-b")
	a1 := evt("203.0.113.9", 941100, "shop.example.com")
	a1.TxID = pgtype.Text{String: "tx-a", Valid: true}
	a2 := evt("203.0.113.9", 942100, "shop.example.com")
	a2.TxID = pgtype.Text{String: "tx-a", Valid: true}
	b1 := evt("203.0.113.9", int32(waf.AnomalySummaryRule), "shop.example.com")
	b1.TxID = pgtype.Text{String: "tx-b", Valid: true}
	// A second client, quieter, and with nothing that would have been blocked.
	c1 := evt("198.51.100.7", 920420, "shop.example.com")
	c1.TxID = pgtype.Text{String: "tx-c", Valid: true}
	seedWAFEvents(t, a1, a2, b1, c1)

	body := wafClients(t, token, "")
	assert.Equal(t, "24h", body["window"])
	assert.Equal(t, false, body["has_more"])
	require.Len(t, body["clients"], 2)

	// Loudest first, so the scan does not have to be hunted for.
	assert.Equal(t, "203.0.113.9", body["clients"].([]any)[0].(map[string]any)["client_ip"])

	loud := groupFor(t, body, "203.0.113.9")
	assert.Equal(t, float64(3), loud["events"], "three rule matches")
	assert.Equal(t, float64(2), loud["requests"], "from two requests")
	assert.Equal(t, float64(3), loud["rules"], "three distinct rules")
	assert.Equal(t, float64(2), loud["urls"], "two distinct URLs, read through the request log")
	assert.Equal(t, true, loud["would_block"], "one request passed the anomaly threshold")
	assert.NotEmpty(t, loud["last_seen"])

	quiet := groupFor(t, body, "198.51.100.7")
	assert.Equal(t, float64(1), quiet["events"])
	assert.Equal(t, float64(1), quiet["requests"])
	assert.Equal(t, false, quiet["would_block"], "a content-type match is not a verdict")
	assert.Equal(t, float64(0), quiet["urls"], "no request log on record for it")
}

// The trap this query was written around: nothing enforces one request_logs row
// per uuid, so joining to count URLs can multiply the hit count. Two rows for
// one request must still be one request and one hit.
func TestWAFClients_TwoRequestLogsForOneRequestDoNotDoubleTheHits(t *testing.T) {
	token := wafFixture(t)
	appID := wafApp(t, token)
	reqLog(t, appID, "/admin.php", "tx-dup")
	reqLog(t, appID, "/admin.php", "tx-dup")

	e := evt("203.0.113.9", 941100, "shop.example.com")
	e.TxID = pgtype.Text{String: "tx-dup", Valid: true}
	seedWAFEvents(t, e)

	g := groupFor(t, wafClients(t, token, ""), "203.0.113.9")
	assert.Equal(t, float64(1), g["events"], "one rule matched once")
	assert.Equal(t, float64(1), g["requests"])
	assert.Equal(t, float64(1), g["urls"], "one URL, however many log rows carry it")
}

func TestWAFClients_HonourTheWindowTheFiltersAndTheCap(t *testing.T) {
	token := wafFixture(t)
	recent := evt("203.0.113.9", 941100, "shop.example.com")
	old := evt("192.0.2.1", 942100, "shop.example.com")
	old.RecordedAt = pgtype.Timestamptz{Time: time.Now().Add(-72 * time.Hour), Valid: true}
	seedWAFEvents(t, recent, old)

	assert.Len(t, wafClients(t, token, "")["clients"], 1, "the 72-hour-old client is outside 24h")
	assert.Len(t, wafClients(t, token, "?window=7d")["clients"], 2)
	assert.Len(t, wafClients(t, token, "?window=all")["clients"], 2)
	assert.Len(t, wafClients(t, token, "?window=7d&client_ip=192.0.2.1")["clients"], 1)
	assert.Len(t, wafClients(t, token, "?window=7d&client_ip=::ffff:192.0.2.1")["clients"], 1,
		"a mapped IPv4 address is the same client")
	assert.Len(t, wafClients(t, token, "?window=7d&rule_id=942100")["clients"], 1)

	// The cap says so rather than pretending it listed everything.
	capped := wafClients(t, token, "?window=7d&limit=1")
	assert.Len(t, capped["clients"], 1)
	assert.Equal(t, true, capped["has_more"], "more clients matched than were listed")
}

func TestWAFClients_RejectBadQueries(t *testing.T) {
	token := wafFixture(t)
	for _, q := range []string{"?window=30d", "?limit=0", "?limit=201", "?limit=x", "?client_ip=nope", "?rule_id=x", "?rule_id=-1"} {
		resp := env.DoRequest(t, "GET", "/api/maintenance/proxy/waf/clients"+q, nil, testutil.AuthHeader(token))
		assert.Equal(t, http.StatusBadRequest, resp.StatusCode, q)
		_ = resp.Body.Close()
	}
}

// The grouped counts and the list behind an expanded client have to agree, which
// they only do if the list can be scoped to the same window.
func TestWAFEvents_CanBeScopedToTheSummaryWindow(t *testing.T) {
	token := wafFixture(t)
	recent := evt("203.0.113.9", 941100, "shop.example.com")
	old := evt("203.0.113.9", 942100, "shop.example.com")
	old.RecordedAt = pgtype.Timestamptz{Time: time.Now().Add(-72 * time.Hour), Valid: true}
	seedWAFEvents(t, recent, old)

	get := func(q string) []any {
		resp := env.DoRequest(t, "GET", "/api/maintenance/proxy/waf/events"+q, nil, testutil.AuthHeader(token))
		require.Equal(t, http.StatusOK, resp.StatusCode, q)
		return testutil.ReadJSON(t, resp)["events"].([]any)
	}

	assert.Len(t, get(""), 2, "no window asked for is every event still kept")
	assert.Len(t, get("?window=24h"), 1)
	assert.Len(t, get("?window=7d"), 2)
	assert.Len(t, get("?window=all"), 2)

	resp := env.DoRequest(t, "GET", "/api/maintenance/proxy/waf/events?window=30d", nil, testutil.AuthHeader(token))
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
	_ = resp.Body.Close()
}
