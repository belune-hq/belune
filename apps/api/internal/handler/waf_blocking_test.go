package handler_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/belune-hq/belune/internal/store/generated"
	"github.com/belune-hq/belune/internal/testutil"
)

const wafBlockingPath = "/api/maintenance/proxy/waf/blocking"

func blockingFixture(t *testing.T) string {
	t.Helper()
	resetDB(t)
	token := env.SetupAdmin(t, "admin@test.com", "password123")
	t.Cleanup(func() { env.Reconciler.ReconcileNowCalls = 0 })
	return token
}

func blockingDomain(t *testing.T, appID, host, path string) {
	t.Helper()
	_, err := env.Queries.CreateDomain(context.Background(), generated.CreateDomainParams{
		ApplicationID: mustUUID(t, appID), Hostname: host, Path: path, SslMode: "off",
		ContainerPort: pgtype.Int4{Int32: 8080, Valid: true},
	})
	require.NoError(t, err)
}

func putBlocking(t *testing.T, token, appID string, body any) *http.Response {
	t.Helper()
	return env.DoRequest(t, "PUT", fmt.Sprintf("%s/%s", wafBlockingPath, appID), body, testutil.AuthHeader(token))
}

func blockingList(t *testing.T, token string) []map[string]any {
	t.Helper()
	resp := env.DoRequest(t, "GET", wafBlockingPath, nil, testutil.AuthHeader(token))
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var out []map[string]any
	for _, a := range testutil.ReadJSON(t, resp)["applications"].([]any) {
		out = append(out, a.(map[string]any))
	}
	return out
}

func TestWAFBlocking_ListsApplicationsWithTheHostsTurningItOnWouldAffect(t *testing.T) {
	token := blockingFixture(t)
	assert.Empty(t, blockingList(t, token))

	appID := wafApp(t, token)
	assert.Empty(t, blockingList(t, token), "an application with no domain has nothing to enforce on, so it is not offered")

	blockingDomain(t, appID, "shop.example.com", "/")
	blockingDomain(t, appID, "www.example.com", "/")
	list := blockingList(t, token)
	require.Len(t, list, 1)
	assert.Equal(t, false, list[0]["blocking"], "detection-only until the operator opts in")
	assert.Equal(t, []any{"shop.example.com", "www.example.com"}, list[0]["hosts"], "the blast radius, by name")
	assert.Equal(t, []any{}, list[0]["shared_hosts"])
}

func TestWAFBlocking_TurnItOnAndOffIsAppliedNow(t *testing.T) {
	token := blockingFixture(t)
	appID := wafApp(t, token)
	blockingDomain(t, appID, "shop.example.com", "/")

	resp := putBlocking(t, token, appID, map[string]any{"enabled": true})
	require.Equal(t, http.StatusOK, resp.StatusCode)
	got := testutil.ReadJSON(t, resp)
	assert.Equal(t, true, got["blocking"])
	assert.Equal(t, []any{"shop.example.com"}, got["hosts"])
	assert.Equal(t, 1, env.Reconciler.ReconcileNowCalls, "applied now, not on the next 30-second pass")
	assert.Equal(t, true, blockingList(t, token)[0]["blocking"])

	resp = putBlocking(t, token, appID, map[string]any{"enabled": false})
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, 2, env.Reconciler.ReconcileNowCalls)
	assert.Equal(t, false, blockingList(t, token)[0]["blocking"])
}

// Turning blocking on puts real visitors behind a rule set that can refuse them,
// at least as consequential as an exclusion, which is audited with what it covers.
func TestWAFBlocking_IsAuditedWithTheHostsItAffects(t *testing.T) {
	token := blockingFixture(t)
	base := newAuditedMCP(t)
	appID := wafApp(t, token)
	blockingDomain(t, appID, "shop.example.com", "/")

	call := func(enabled bool) {
		b, _ := json.Marshal(map[string]any{"enabled": enabled})
		req, err := http.NewRequest("PUT", base+fmt.Sprintf("%s/%s", wafBlockingPath, appID), bytes.NewReader(b))
		require.NoError(t, err)
		req.Header.Set("Content-Type", "application/json")
		for k, v := range testutil.AuthHeader(token) {
			req.Header.Set(k, v)
		}
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		require.Equal(t, http.StatusOK, resp.StatusCode)
	}
	details := func(action string) map[string]any {
		var raw string
		require.Eventually(t, func() bool {
			return env.Pool.QueryRow(context.Background(),
				`SELECT details::text FROM audit_logs WHERE action = $1 ORDER BY created_at DESC LIMIT 1`, action).Scan(&raw) == nil
		}, 3*time.Second, 10*time.Millisecond, action+" was not audited")
		var d map[string]any
		require.NoError(t, json.Unmarshal([]byte(raw), &d))
		return d
	}

	call(true)
	on := details("enable_waf_blocking")
	assert.Equal(t, []any{"shop.example.com"}, on["hosts"], "the audit row names what was put behind enforcement")
	assert.Equal(t, false, on["was_blocking"])

	call(false)
	assert.Equal(t, true, details("disable_waf_blocking")["was_blocking"])
}

func TestWAFBlocking_NamesTheHostsASiblingKeepsDetectionOnly(t *testing.T) {
	token := blockingFixture(t)
	a := wafApp(t, token)
	blockingDomain(t, a, "shared.example.com", "/")
	blockingDomain(t, a, "mine.example.com", "/")
	project := env.CreateProject(t, token, "Other", "other")
	other := extractID(env.CreateApplication(t, token, extractID(project["id"]), map[string]any{
		"name": "Other", "type": "git", "build_type": "dockerfile", "source_repo": "https://github.com/test/other",
	})["id"])
	blockingDomain(t, other, "shared.example.com", "/api")

	require.Equal(t, http.StatusOK, putBlocking(t, token, a, map[string]any{"enabled": true}).StatusCode)
	for _, app := range blockingList(t, token) {
		if app["application_id"] == a {
			assert.Equal(t, []any{"shared.example.com"}, app["shared_hosts"],
				"served by an application that has not opted in, so it will not be enforced on")
		}
	}
}

func TestWAFBlocking_RefusesWhatItCannotApply(t *testing.T) {
	token := blockingFixture(t)
	appID := wafApp(t, token)

	assert.Equal(t, http.StatusNotFound, putBlocking(t, token, appID, map[string]any{"enabled": true}).StatusCode, "no domain")
	assert.Equal(t, http.StatusNotFound, putBlocking(t, token, "00000000-0000-0000-0000-000000000000", map[string]any{"enabled": true}).StatusCode)
	assert.Equal(t, http.StatusBadRequest, putBlocking(t, token, "not-a-uuid", map[string]any{"enabled": true}).StatusCode)

	blockingDomain(t, appID, "shop.example.com", "/")
	assert.Equal(t, http.StatusBadRequest, putBlocking(t, token, appID, map[string]any{}).StatusCode, "enabled is required, not defaulted")
	assert.Equal(t, http.StatusBadRequest, putBlocking(t, token, appID, map[string]any{"enabled": "yes"}).StatusCode)
	assert.Equal(t, 0, env.Reconciler.ReconcileNowCalls, "a refusal changes nothing, so it applies nothing")
}

func TestWAFBlocking_IsAdminOnly(t *testing.T) {
	token := blockingFixture(t)
	appID := wafApp(t, token)
	blockingDomain(t, appID, "shop.example.com", "/")

	resp := env.DoRequest(t, "PUT", fmt.Sprintf("%s/%s", wafBlockingPath, appID), map[string]any{"enabled": true}, nil)
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
}

// A blocked request logs its deny-ing rule twice; the list shows it once.
func TestWAFEvents_ShowTheDenyingRuleOnceNotTwice(t *testing.T) {
	token := wafFixture(t)
	mk := func(rule int32, tx, disp string) generated.BulkInsertWAFEventsParams {
		e := evt("203.0.113.9", rule, "shop.example.com")
		e.TxID = pgtype.Text{String: tx, Valid: tx != ""}
		e.Disposition = pgtype.Text{String: disp, Valid: true}
		return e
	}
	seedWAFEvents(t,
		mk(942290, "tx-1", "detected"),
		mk(949110, "tx-1", "detected"), mk(949110, "tx-1", "blocked"), // the pair
		mk(949110, "tx-2", "detected"),                        // only reported: kept
		mk(949110, "", "detected"), mk(949110, "", "blocked"), // no transaction: nothing to pair, both kept
	)

	resp := env.DoRequest(t, "GET", "/api/maintenance/proxy/waf/events", nil, testutil.AuthHeader(token))
	require.Equal(t, http.StatusOK, resp.StatusCode)
	type key struct{ tx, disp string }
	got := map[key]int{}
	for _, e := range testutil.ReadJSON(t, resp)["events"].([]any) {
		ev := e.(map[string]any)
		tx, _ := ev["tx_id"].(string)
		if ev["rule_id"].(float64) == 949110 {
			got[key{tx, ev["disposition"].(string)}]++
		}
	}
	assert.Equal(t, map[key]int{
		{"tx-1", "blocked"}:  1,
		{"tx-2", "detected"}: 1,
		{"", "detected"}:     1,
		{"", "blocked"}:      1,
	}, got)
}
