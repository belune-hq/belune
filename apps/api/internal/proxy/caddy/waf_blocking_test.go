package caddy

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/belune-hq/belune/internal/proxy"
)

func TestRenderWAFBlocking_IsOneHostScopedPromotion(t *testing.T) {
	got := renderWAFBlocking([]string{"B.example.com", "a.example.com", "a.example.com"})
	assert.Equal(t,
		`SecRule REQUEST_HEADERS:X-Belune-Waf-Tx "@rx (?i)^(?:a\.example\.com|b\.example\.com)_[0-9a-f-]{36}$" "id:1200001,phase:1,pass,nolog,ctl:ruleEngine=On"`+"\n",
		got)
}

// ⛔ The fail-safe direction is the whole safety argument: every way this can go
// wrong has to leave a host detecting, never blocking.
func TestRenderWAFBlocking_RendersNothingWhenItCannotScope(t *testing.T) {
	assert.Empty(t, renderWAFBlocking(nil), "no hosts")
	assert.Empty(t, renderWAFBlocking([]string{}), "no hosts")
	assert.Empty(t, renderWAFBlocking([]string{""}), "a blank host")
	assert.Empty(t, renderWAFBlocking([]string{`a.test" "ctl:ruleEngine=On`, "a b.test", "x\nSecRuleEngine On"}),
		"hosts that are not hostnames are left out, never guessed at")

	// A bad host beside a good one narrows the rule rather than widening it.
	got := renderWAFBlocking([]string{"a.example.com", "x\nSecRuleEngine On"})
	assert.Contains(t, got, `^(?:a\.example\.com)_`)
	assert.NotContains(t, got, "SecRuleEngine")
}

func TestRenderWAFDirectives_BlockingCanNeverBeInstallWide(t *testing.T) {
	for _, hosts := range [][]string{nil, {}, {"a.example.com"}, {"*.example.com"}, {"a.example.com", "b.example.com"}} {
		out := renderWAFDirectives([]proxy.WAFExclusion{ex(1, 941100, true)}, hosts)

		// Only the host-scoped SecRule form may carry the promotion.
		for _, l := range strings.Split(out, "\n") {
			if strings.Contains(l, "ctl:ruleEngine") {
				assert.True(t, strings.HasPrefix(l, "SecRule REQUEST_HEADERS:"+"X-Belune-Waf-Tx"), "a promotion that is not host-scoped: %q", l)
				assert.False(t, strings.HasPrefix(l, "SecAction"), "%q", l)
			}
			// The static engine is never turned On at load.
			assert.False(t, strings.HasPrefix(l, "SecRuleEngine On"), "%q", l)
		}

		// DetectionOnly is still the last engine directive in the file.
		last := ""
		for _, l := range strings.Split(out, "\n") {
			if strings.HasPrefix(l, "SecRuleEngine ") {
				last = l
			}
		}
		assert.Equal(t, "SecRuleEngine DetectionOnly", last)
	}
}

func TestRenderWAFDirectives_BlockingSitsBetweenTheSetupAndTheRuleSet(t *testing.T) {
	out := renderWAFDirectives(nil, []string{"a.example.com"})
	setup := strings.Index(out, "Include @crs-setup.conf.example")
	rule := strings.Index(out, "ctl:ruleEngine=On")
	crs := strings.Index(out, "Include @owasp_crs/*.conf")
	static := strings.LastIndex(out, "SecRuleEngine DetectionOnly")
	require.True(t, setup >= 0 && rule >= 0 && crs >= 0 && static >= 0)
	assert.Less(t, setup, rule)
	assert.Less(t, rule, crs, "a phase-1 rule has to be defined before the rules it governs")
	assert.Less(t, crs, static)
}

func TestRenderWAFDirectives_NoBlockingHostsIsByteForByteWhatShippedBefore(t *testing.T) {
	ex1 := []proxy.WAFExclusion{ex(1, 941100, true)}
	assert.Equal(t, renderWAFDirectives(ex1, nil), renderWAFDirectives(ex1, []string{}))
	assert.NotContains(t, renderWAFDirectives(ex1, nil), "ruleEngine")
	assert.Equal(t, wafDirectivesBeforeExclusions, renderWAFDirectives(nil, nil))
}

// A duplicate rule id likely makes Caddy reject the whole write and take the WAF
// route down, so the blocking id has to be clear of every other generated id.
func TestWAFBlockingRuleID_IsClearOfEveryOtherGeneratedID(t *testing.T) {
	assert.Greater(t, wafBlockingRuleID, wafExclusionRuleBase+proxy.MaxWAFExclusions)

	exs := make([]proxy.WAFExclusion, 0, proxy.MaxWAFExclusions)
	for i := 1; i <= proxy.MaxWAFExclusions; i++ {
		exs = append(exs, ex(int64(i), 941100, true))
	}
	out := renderWAFDirectives(exs, []string{"a.example.com"})
	ids := regexp.MustCompile(`id:(\d+),`).FindAllStringSubmatch(out, -1)
	seen := map[string]bool{}
	for _, m := range ids {
		assert.False(t, seen[m[1]], "duplicate rule id %s", m[1])
		seen[m[1]] = true
	}
}

func TestWAFBlockingHosts_AreOnlyHostsTheRouteInspects(t *testing.T) {
	hosts := []string{"a.example.com", "b.example.com"}
	assert.Equal(t, []string{"a.example.com"}, wafBlockingHosts(hosts, []string{"A.example.com", "dash.example.com"}),
		"a host the route does not match is never enforced on, whoever asked")
	assert.Empty(t, wafBlockingHosts(hosts, nil))
}

func TestBuildWAFRoute_CarriesTheBlockingRule(t *testing.T) {
	r := buildWAFRoute([]string{"a.example.com"}, nil, []string{"a.example.com"})
	b, err := json.Marshal(r)
	require.NoError(t, err)
	assert.Contains(t, string(b), "ctl:ruleEngine=On")
	plain, err := json.Marshal(buildWAFRoute([]string{"a.example.com"}, nil, nil))
	require.NoError(t, err)
	assert.NotContains(t, string(plain), "ruleEngine")
}

// The measurement that decides the shape of per-host blocking, against a real
// Caddy: the opted-in host is refused, the other is served and still detected.
//
//	BELUNE_CADDY_INTEGRATION=1 CADDY_HTTP_URL=http://127.0.0.1:80 \
//	  go test ./internal/proxy/caddy/ -run RealCaddy_WAF_Blocking -v
func TestRealCaddy_WAF_BlockingIsPerHost(t *testing.T) {
	rig := newWAFRig(t)
	ctx := context.Background()
	const otherHost = "waf-e2e-other.belune.local"
	upstream := rigUpstream(t, rig)
	require.NoError(t, rig.c.AddRoute(ctx, proxy.RouteConfig{Hostname: otherHost, TargetURL: upstream, SSLMode: proxy.SSLModeOff}))
	t.Cleanup(func() { _ = rig.c.RemoveRoute(ctx, otherHost, "/") })

	attack := "/?q=" + url.QueryEscape("<script>alert(1)</script> union select 1,2--")
	status := func(host string) int {
		req, err := http.NewRequest(http.MethodGet, rig.httpURL+attack, nil)
		require.NoError(t, err)
		req.Host = host
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		defer func() { _ = resp.Body.Close() }()
		return resp.StatusCode
	}
	sync := func(blocking ...string) {
		require.NoError(t, rig.c.SyncWAF(ctx, proxy.WAFConfig{
			Enabled: true, Hosts: []string{wafAppHost, otherHost}, BlockingHosts: blocking,
		}), "Caddy refused the route with the blocking rule")
	}

	sync()
	assert.Equal(t, http.StatusOK, status(wafAppHost), "detection-only by default")
	assert.Equal(t, http.StatusOK, status(otherHost))

	sync(wafAppHost)
	assert.Equal(t, http.StatusForbidden, status(wafAppHost), "the opted-in host is refused")
	assert.Equal(t, http.StatusOK, status(otherHost), "the other host is not")

	// The step that makes it proof: take it away and the 403 goes, on cue.
	sync()
	assert.Equal(t, http.StatusOK, status(wafAppHost))
}

func rigUpstream(t *testing.T, rig *wafRig) string {
	t.Helper()
	routes, err := rig.c.ListRoutes(context.Background())
	require.NoError(t, err)
	for _, r := range routes {
		if r.Hostname == wafAppHost {
			return r.TargetURL
		}
	}
	t.Fatal("the rig's route is missing")
	return ""
}
