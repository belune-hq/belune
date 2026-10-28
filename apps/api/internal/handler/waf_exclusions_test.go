package handler_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/belune-hq/belune/internal/proxy"
	"github.com/belune-hq/belune/internal/testutil"
)

const wafExclusionsPath = "/api/maintenance/proxy/waf/exclusions"

func exclusionFixture(t *testing.T) string {
	t.Helper()
	resetDB(t)
	token := env.SetupAdmin(t, "admin@test.com", "password123")
	t.Cleanup(func() {
		env.Reconciler.ReconcileNowCalls = 0
		_, _ = env.Pool.Exec(context.Background(), `DELETE FROM waf_exclusions`)
	})
	return token
}

func postExclusion(t *testing.T, token string, body map[string]any) *http.Response {
	t.Helper()
	return env.DoRequest(t, "POST", wafExclusionsPath, body, testutil.AuthHeader(token))
}

func listExclusions(t *testing.T, token string) map[string]any {
	t.Helper()
	resp := env.DoRequest(t, "GET", wafExclusionsPath, nil, testutil.AuthHeader(token))
	require.Equal(t, http.StatusOK, resp.StatusCode)
	return testutil.ReadJSON(t, resp)
}

func exclusionCount(t *testing.T) int {
	t.Helper()
	var n int
	require.NoError(t, env.Pool.QueryRow(context.Background(), `SELECT count(*) FROM waf_exclusions`).Scan(&n))
	return n
}

func addDomain(t *testing.T, appID, host string) {
	t.Helper()
	_, err := env.Pool.Exec(context.Background(), `INSERT INTO domains (application_id, hostname) VALUES ($1, $2)`, appID, host)
	require.NoError(t, err)
}

func TestWAFExclusions_StartEmptyAndSayWhatTheyCanHold(t *testing.T) {
	token := exclusionFixture(t)
	body := listExclusions(t, token)
	assert.Empty(t, body["exclusions"])
	assert.EqualValues(t, proxy.MaxWAFExclusions, body["max"])
	assert.Contains(t, body["collections"], "REQUEST_COOKIES")
}

func TestWAFExclusions_CreateEachShapeAndApplyItNow(t *testing.T) {
	token := exclusionFixture(t)

	resp := postExclusion(t, token, map[string]any{
		"rule_id": 942290, "collection": "REQUEST_COOKIES", "target_name": "ph_phc_x_posthog",
	})
	require.Equal(t, http.StatusCreated, resp.StatusCode)
	got := testutil.ReadJSON(t, resp)
	assert.Equal(t, "target", got["shape"])
	assert.Equal(t, "install", got["scope"])
	assert.Nil(t, got["application_id"])
	assert.Equal(t, 1, env.Reconciler.ReconcileNowCalls, "applied now, not on the next 30-second pass")

	resp = postExclusion(t, token, map[string]any{"rule_id": 930130, "path_pattern": "/.env.local"})
	require.Equal(t, http.StatusCreated, resp.StatusCode)
	assert.Equal(t, "path", testutil.ReadJSON(t, resp)["shape"])

	resp = postExclusion(t, token, map[string]any{"rule_id": 941100, "note": "our editor posts markup"})
	require.Equal(t, http.StatusCreated, resp.StatusCode)
	rule := testutil.ReadJSON(t, resp)
	assert.Equal(t, "rule", rule["shape"])
	assert.Equal(t, "our editor posts markup", rule["note"])

	assert.Len(t, listExclusions(t, token)["exclusions"], 3)
}

func TestWAFExclusions_RefuseWhatWouldBeUnsafeAndStoreNothing(t *testing.T) {
	token := exclusionFixture(t)

	for name, body := range map[string]map[string]any{
		"the verdict rule":               {"rule_id": 949110, "note": "x"},
		"the outbound verdict":           {"rule_id": 959100, "note": "x"},
		"an initialisation rule":         {"rule_id": 901001, "note": "x"},
		"not a CRS rule":                 {"rule_id": 5, "note": "x"},
		"a comma that adds an action":    {"rule_id": 942290, "collection": "ARGS", "target_name": "q,ctl:ruleEngine=Off"},
		"a quote":                        {"rule_id": 942290, "collection": "ARGS", "target_name": `q"`},
		"a macro":                        {"rule_id": 942290, "collection": "ARGS", "target_name": "%{tx.x}"},
		"an unknown collection":          {"rule_id": 942290, "collection": "TX", "target_name": "q"},
		"a name with no collection":      {"rule_id": 942290, "target_name": "q"},
		"a target and a path together":   {"rule_id": 942290, "collection": "ARGS", "target_name": "q", "path_pattern": "/x"},
		"a path with a quote":            {"rule_id": 930130, "path_pattern": `/x" "ctl:ruleEngine=Off`},
		"a path with a macro":            {"rule_id": 930130, "path_pattern": "/%{tx.x}"},
		"a path that is every path":      {"rule_id": 930130, "path_pattern": "/*"},
		"a path with no root":            {"rule_id": 930130, "path_pattern": "x"},
		"turning a rule off, no note":    {"rule_id": 941100},
		"turning a rule off, blank note": {"rule_id": 941100, "note": "   "},
		"a note that is too long":        {"rule_id": 941100, "note": string(bytes.Repeat([]byte("n"), 501))},
	} {
		t.Run(name, func(t *testing.T) {
			resp := postExclusion(t, token, body)
			defer func() { _ = resp.Body.Close() }()
			assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
		})
	}
	assert.Zero(t, exclusionCount(t), "a refused exclusion leaves nothing behind")
	assert.Equal(t, 0, env.Reconciler.ReconcileNowCalls)
}

func TestWAFExclusions_RefuseTheVerdictWithAReasonThatSaysWhy(t *testing.T) {
	token := exclusionFixture(t)
	resp := postExclusion(t, token, map[string]any{"rule_id": 949110, "note": "too many blocks"})
	require.Equal(t, http.StatusBadRequest, resp.StatusCode)
	msg, _ := testutil.ReadJSON(t, resp)["error"].(string)
	assert.Contains(t, msg, "verdict")
	assert.Contains(t, msg, "Exclude the rule that scored instead")
}

func TestWAFExclusions_DuplicatesUnknownApplicationsAndTheCeiling(t *testing.T) {
	token := exclusionFixture(t)
	body := map[string]any{"rule_id": 942290, "collection": "ARGS", "target_name": "q"}
	require.Equal(t, http.StatusCreated, postExclusion(t, token, body).StatusCode)
	assert.Equal(t, http.StatusConflict, postExclusion(t, token, body).StatusCode, "the same carve-out twice")

	resp := postExclusion(t, token, map[string]any{"rule_id": 941100, "note": "x", "application_id": "00000000-0000-0000-0000-000000000099"})
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)

	// Fill to the ceiling, then one more.
	for i := exclusionCount(t); i < proxy.MaxWAFExclusions; i++ {
		_, err := env.Pool.Exec(context.Background(),
			`INSERT INTO waf_exclusions (rule_id, collection, target_name) VALUES (942290, 'ARGS', $1)`, fmt.Sprintf("fill%d", i))
		require.NoError(t, err)
	}
	require.Equal(t, proxy.MaxWAFExclusions, exclusionCount(t))
	resp = postExclusion(t, token, map[string]any{"rule_id": 942290, "collection": "ARGS", "target_name": "one-too-many"})
	require.Equal(t, http.StatusConflict, resp.StatusCode)
	msg, _ := testutil.ReadJSON(t, resp)["error"].(string)
	assert.Contains(t, msg, "100")
	assert.Equal(t, proxy.MaxWAFExclusions, exclusionCount(t))
}

// The reason the migration says SET NULL is wrong: NULL means install-wide, so it
// would not orphan an application's exclusion, it would PROMOTE it to one that
// disables the rule for every application.
func TestWAFExclusions_DeletingTheApplicationDeletesItsExclusionsAndNeverPromotesThem(t *testing.T) {
	token := exclusionFixture(t)
	appID := wafApp(t, token)
	addDomain(t, appID, "shop.example.com")

	resp := postExclusion(t, token, map[string]any{"rule_id": 941100, "note": "ours", "application_id": appID})
	require.Equal(t, http.StatusCreated, resp.StatusCode)
	got := testutil.ReadJSON(t, resp)
	assert.Equal(t, "application", got["scope"])
	assert.Equal(t, []any{"shop.example.com"}, got["hosts"])
	require.Equal(t, 1, exclusionCount(t))

	_, err := env.Pool.Exec(context.Background(), `DELETE FROM applications WHERE id = $1`, appID)
	require.NoError(t, err)

	assert.Zero(t, exclusionCount(t), "the exclusion went with its application")
	var installWide int
	require.NoError(t, env.Pool.QueryRow(context.Background(),
		`SELECT count(*) FROM waf_exclusions WHERE application_id IS NULL`).Scan(&installWide))
	assert.Zero(t, installWide, "and was not promoted to an install-wide exclusion")
}

func TestWAFExclusions_AnApplicationWithNoDomainsIsInertAndSaysSo(t *testing.T) {
	token := exclusionFixture(t)
	appID := wafApp(t, token)

	resp := postExclusion(t, token, map[string]any{"rule_id": 941100, "note": "ours", "application_id": appID})
	require.Equal(t, http.StatusCreated, resp.StatusCode)
	got := testutil.ReadJSON(t, resp)
	assert.Equal(t, true, got["inert"])
	assert.Empty(t, got["hosts"])
	assert.Equal(t, "application", got["scope"], "inert, not install-wide")

	// Hosts follow the domains: add one and it is no longer inert; drop it and it is again.
	addDomain(t, appID, "shop.example.com")
	list := listExclusions(t, token)["exclusions"].([]any)
	assert.Equal(t, false, list[0].(map[string]any)["inert"])
	_, err := env.Pool.Exec(context.Background(), `DELETE FROM domains WHERE application_id = $1`, appID)
	require.NoError(t, err)
	list = listExclusions(t, token)["exclusions"].([]any)
	assert.Equal(t, true, list[0].(map[string]any)["inert"])
}

func TestWAFExclusions_TheDatabaseRefusesTheVerdictRuleToo(t *testing.T) {
	exclusionFixture(t)
	_, err := env.Pool.Exec(context.Background(), `INSERT INTO waf_exclusions (rule_id) VALUES (949110)`)
	assert.Error(t, err, "a row that arrives any way but the API still cannot exclude the verdict")
	_, err = env.Pool.Exec(context.Background(), `INSERT INTO waf_exclusions (rule_id, collection) VALUES (942290, 'ARGS')`)
	assert.Error(t, err, "a collection without a name")
	_, err = env.Pool.Exec(context.Background(), `INSERT INTO waf_exclusions (rule_id, collection, target_name, path_pattern) VALUES (942290, 'ARGS', 'q', '/x')`)
	assert.Error(t, err, "a target and a path together")
}

func TestWAFExclusions_DeleteReversesItAndAppliesItNow(t *testing.T) {
	token := exclusionFixture(t)
	resp := postExclusion(t, token, map[string]any{"rule_id": 942290, "collection": "ARGS", "target_name": "q"})
	require.Equal(t, http.StatusCreated, resp.StatusCode)
	id := testutil.ReadJSON(t, resp)["id"]

	del := env.DoRequest(t, "DELETE", fmt.Sprintf("%s/%v", wafExclusionsPath, id), nil, testutil.AuthHeader(token))
	assert.Equal(t, http.StatusNoContent, del.StatusCode)
	assert.Zero(t, exclusionCount(t))
	assert.Equal(t, 2, env.Reconciler.ReconcileNowCalls)

	again := env.DoRequest(t, "DELETE", fmt.Sprintf("%s/%v", wafExclusionsPath, id), nil, testutil.AuthHeader(token))
	assert.Equal(t, http.StatusNotFound, again.StatusCode)
	bad := env.DoRequest(t, "DELETE", wafExclusionsPath+"/abc", nil, testutil.AuthHeader(token))
	assert.Equal(t, http.StatusBadRequest, bad.StatusCode)
}

func TestWAFStatus_SaysWhenDetectionsAreBeingSilencedOnPurpose(t *testing.T) {
	token := exclusionFixture(t)
	appID := wafApp(t, token)
	require.Equal(t, http.StatusCreated, postExclusion(t, token, map[string]any{"rule_id": 942290, "collection": "ARGS", "target_name": "q"}).StatusCode)
	require.Equal(t, http.StatusCreated, postExclusion(t, token, map[string]any{"rule_id": 941100, "note": "ours", "application_id": appID}).StatusCode)

	resp := env.DoRequest(t, "GET", "/api/maintenance/proxy/waf", nil, testutil.AuthHeader(token))
	require.Equal(t, http.StatusOK, resp.StatusCode)
	ex := testutil.ReadJSON(t, resp)["exclusions"].(map[string]any)
	assert.EqualValues(t, 2, ex["total"])
	assert.EqualValues(t, 1, ex["inert"], "the application-scoped one has no domain")
}

// An exclusion is a security downgrade. The audit row has to answer, after the
// person who made it has gone, who turned what off, where, and why.
func TestWAFExclusions_AreAuditedWithTheirReasonAndWhatTheyWere(t *testing.T) {
	token := exclusionFixture(t)
	base := newAuditedMCP(t)

	call := func(method, path string, body any) *http.Response {
		t.Helper()
		var rdr *bytes.Reader
		if body != nil {
			b, _ := json.Marshal(body)
			rdr = bytes.NewReader(b)
		} else {
			rdr = bytes.NewReader(nil)
		}
		req, err := http.NewRequest(method, base+path, rdr)
		require.NoError(t, err)
		req.Header.Set("Content-Type", "application/json")
		for k, v := range testutil.AuthHeader(token) {
			req.Header.Set(k, v)
		}
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		return resp
	}

	resp := call("POST", wafExclusionsPath, map[string]any{"rule_id": 941100, "note": "our editor posts markup"})
	require.Equal(t, http.StatusCreated, resp.StatusCode)
	id := testutil.ReadJSON(t, resp)["id"]

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
	created := details("create_waf_exclusion")
	assert.EqualValues(t, 941100, created["rule_id"])
	assert.Equal(t, "rule", created["shape"])
	assert.Equal(t, "install", created["scope"])
	assert.Equal(t, "our editor posts markup", created["note"], "the reason survives the person who typed it")

	del := call("DELETE", fmt.Sprintf("%s/%v", wafExclusionsPath, id), nil)
	require.Equal(t, http.StatusNoContent, del.StatusCode)
	removed := details("delete_waf_exclusion")
	assert.Equal(t, "our editor posts markup", removed["note"], "a deletion records what it removed")
}
