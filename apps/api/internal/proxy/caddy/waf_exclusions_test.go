package caddy

import (
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/belune-hq/belune/internal/proxy"
)

// wafDirectivesBeforeExclusions is the configuration exactly as it was before
// exclusions existed. With none, the renderer must still produce this, or every
// install would see its WAF route differ on upgrade and Caddy would reload.
const wafDirectivesBeforeExclusions = `Include @coraza.conf-recommended
Include @crs-setup.conf.example
Include @owasp_crs/*.conf
SecResponseBodyAccess Off
SecAuditEngine Off
SecDebugLogLevel 1
SecRequestBodyLimit 131072
SecRequestBodyInMemoryLimit 131072
SecRequestBodyLimitAction ProcessPartial
SecRule &REQUEST_HEADERS:Content-Length "@eq 0" "id:1000001,phase:1,pass,nolog,ctl:requestBodyAccess=Off"
SecRule REQUEST_HEADERS:Content-Length "@gt 131072" "id:1000002,phase:1,pass,nolog,ctl:requestBodyAccess=Off"
SecRule REQUEST_HEADERS:Transfer-Encoding "@rx .+" "id:1000003,phase:1,pass,nolog,ctl:requestBodyAccess=Off"
SecRuleEngine DetectionOnly
`

func ex(id int64, rule int, global bool, hosts ...string) proxy.WAFExclusion {
	return proxy.WAFExclusion{ID: id, WAFExclusionSpec: proxy.WAFExclusionSpec{RuleID: rule}, Global: global, Hosts: hosts}
}

func withTarget(e proxy.WAFExclusion, coll, name string) proxy.WAFExclusion {
	e.Collection, e.TargetName = coll, name
	return e
}

func withPath(e proxy.WAFExclusion, p string) proxy.WAFExclusion {
	e.PathPattern = p
	return e
}

func TestRenderWAFDirectives_NoExclusionsIsByteForByteWhatShippedBefore(t *testing.T) {
	assert.Equal(t, wafDirectivesBeforeExclusions, renderWAFDirectives(nil, nil))
	assert.Equal(t, wafDirectivesBeforeExclusions, wafDirectives)
	assert.Equal(t, wafDirectivesBeforeExclusions, renderWAFDirectives([]proxy.WAFExclusion{}, []string{}))
}

func TestRenderWAFExclusions_EveryShapeAndScope(t *testing.T) {
	got := renderWAFExclusions([]proxy.WAFExclusion{
		withTarget(ex(1, 942290, true), "REQUEST_COOKIES", "ph_phc_x_posthog"),
		withTarget(ex(2, 942290, false, "B.Example.com", "a.example.com"), "ARGS", "q"),
		withPath(ex(3, 930130, true), "/.env.local"),
		withPath(ex(4, 930130, false, "a.example.com"), "/api/*"),
		ex(5, 941100, true),
		ex(6, 941100, false, "*.example.com"),
	})
	want := strings.Join([]string{
		`SecAction "id:1100001,phase:1,pass,nolog,ctl:ruleRemoveTargetById=942290;REQUEST_COOKIES:ph_phc_x_posthog"`,
		`SecRule REQUEST_HEADERS:X-Belune-Waf-Tx "@rx (?i)^(?:a\.example\.com|b\.example\.com)_[0-9a-f-]{36}$" "id:1100002,phase:1,pass,nolog,ctl:ruleRemoveTargetById=942290;ARGS:q"`,
		`SecRule REQUEST_FILENAME "@streq /.env.local" "id:1100003,phase:1,pass,nolog,ctl:ruleRemoveById=930130"`,
		`SecRule REQUEST_HEADERS:X-Belune-Waf-Tx "@rx (?i)^(?:a\.example\.com)_[0-9a-f-]{36}$" "id:1100004,phase:1,pass,nolog,chain"`,
		`  SecRule REQUEST_FILENAME "@beginsWith /api/" "ctl:ruleRemoveById=930130"`,
		`SecAction "id:1100005,phase:1,pass,nolog,ctl:ruleRemoveById=941100"`,
		`SecRule REQUEST_HEADERS:X-Belune-Waf-Tx "@rx (?i)^(?:[a-z0-9_-]+\.example\.com)_[0-9a-f-]{36}$" "id:1100006,phase:1,pass,nolog,ctl:ruleRemoveById=941100"`,
	}, "\n") + "\n"
	assert.Equal(t, want, got)
}

// The sharp version of "an app with no domains": with an empty host list the
// anchored expression degenerates to ^_[0-9a-f-]{36}$, which is not "matches
// nothing" — it matches a transaction id with an EMPTY host, which is what Coraza
// falls back to when the header was missing. So the directive must be ABSENT.
func TestRenderWAFExclusions_AnApplicationWithNoDomainsRendersNothing(t *testing.T) {
	for name, hosts := range map[string][]string{
		"none":           nil,
		"empty":          {},
		"blank":          {"", "  "},
		"nothing usable": {"bad host!", "a/b.example.com", "ex ample.com"},
	} {
		t.Run(name, func(t *testing.T) {
			got := renderWAFExclusions([]proxy.WAFExclusion{
				ex(1, 941100, false, hosts...),
				withTarget(ex(2, 942290, false, hosts...), "ARGS", "q"),
				withPath(ex(3, 930130, false, hosts...), "/x"),
			})
			assert.Empty(t, got, "not install-wide, and not a pattern that matches an unattributed request")
			assert.NotContains(t, got, "^_", "the degenerate anchored expression")
			assert.NotContains(t, got, "SecAction", "an application-scoped exclusion must never become install-wide")
		})
	}
}

func TestRenderWAFExclusions_APartlyUnusableHostListKeepsOnlyTheGoodHosts(t *testing.T) {
	got := renderWAFExclusions([]proxy.WAFExclusion{ex(1, 941100, false, "a.example.com", "bad host!")})
	assert.Contains(t, got, `(?:a\.example\.com)_`)
	assert.NotContains(t, got, "bad")
}

func TestRenderWAFExclusions_RefusesWhatWouldBeInjected(t *testing.T) {
	// Each of these, rendered, would end a quoted string or an action list and let
	// the next thing be a directive of the operator's choosing. The renderer has to
	// hold the line itself, because a row could have been written by anything.
	hostile := []proxy.WAFExclusion{
		withTarget(ex(1, 942290, true), "ARGS", `q,ctl:ruleEngine=Off`),
		withTarget(ex(2, 942290, true), "ARGS", `q"`),
		withTarget(ex(3, 942290, true), "ARGS", "q\nSecRuleEngine Off"),
		withTarget(ex(4, 942290, true), "ARGS", `a;b`),
		withTarget(ex(5, 942290, true), "ARGS", `%{tx.x}`),
		withTarget(ex(6, 942290, true), "ARGS", `/regex/`),
		withTarget(ex(7, 942290, true), "TX", `q`),
		withTarget(ex(8, 942290, true), "", `q`),
		withPath(ex(9, 930130, true), `/x" "ctl:ruleEngine=Off`),
		withPath(ex(10, 930130, true), "/x\nSecRuleEngine Off"),
		withPath(ex(11, 930130, true), `/%{tx.x}`),
		withPath(ex(12, 930130, true), `/a*b`),
		withPath(ex(13, 930130, true), `no-slash`),
		withPath(ex(14, 930130, true), `/*`),
		ex(15, 949110, true),
		ex(16, 959100, true),
		ex(17, 980130, true),
		ex(18, 901001, true),
		ex(19, 100, true),
		ex(20, 99999999, true),
	}
	got := renderWAFExclusions(hostile)
	assert.Empty(t, got, "none of these may reach the directives")
	for _, bad := range []string{"ruleEngine", "SecRuleEngine Off", "949110", "tx.x"} {
		assert.NotContains(t, got, bad)
	}
}

func TestRenderWAFExclusions_IsDeterministicAndPositional(t *testing.T) {
	a := []proxy.WAFExclusion{
		ex(30, 941100, true),
		withTarget(ex(10, 942290, true), "ARGS", "q"),
		withPath(ex(20, 930130, false, "b.example.com", "a.example.com"), "/x"),
	}
	b := []proxy.WAFExclusion{a[2], a[0], a[1]}
	b[0].Hosts = []string{"a.example.com", "B.EXAMPLE.COM"}
	assert.Equal(t, renderWAFExclusions(a), renderWAFExclusions(b), "the same rows are the same bytes, whatever order they arrive in")

	// Ids follow id order, so deleting the middle one renumbers the rest — the set
	// changed, so the bytes were going to change anyway.
	out := renderWAFExclusions(a)
	assert.Regexp(t, `(?s)id:1100001.*id:1100002.*id:1100003`, out)
	assert.NotContains(t, out, "id:1100004")
}

func TestRenderWAFExclusions_NeverReusesAnIdTheRuleSetOrTheBodyRulesUse(t *testing.T) {
	many := make([]proxy.WAFExclusion, 0, proxy.MaxWAFExclusions+20)
	for i := 0; i < proxy.MaxWAFExclusions+20; i++ {
		many = append(many, ex(int64(i+1), 941100+i%10, true))
	}
	out := renderWAFExclusions(many)
	ids := regexp.MustCompile(`id:(\d+)`).FindAllStringSubmatch(out, -1)
	require.Len(t, ids, proxy.MaxWAFExclusions, "the ceiling holds in the renderer too")
	seen := map[string]bool{}
	for _, m := range ids {
		assert.False(t, seen[m[1]], "duplicate id %s would make Caddy refuse the whole config", m[1])
		seen[m[1]] = true
		assert.Greater(t, m[1], "1000003", "clear of the body rules")
		assert.Less(t, len(m[1]), 8)
	}
}

func TestRenderWAFDirectives_ExclusionsGoBeforeTheRuleSetAndTheEngineStaysLast(t *testing.T) {
	out := renderWAFDirectives([]proxy.WAFExclusion{ex(1, 941100, true)}, nil)
	setup := strings.Index(out, "Include @crs-setup.conf.example")
	exclusion := strings.Index(out, "id:1100001")
	rules := strings.Index(out, "Include @owasp_crs/*.conf")
	require.True(t, setup >= 0 && exclusion > setup && rules > exclusion,
		"a ctl:ruleRemove* only affects rules that have not run yet; after the include it left phase-1 rules firing")

	lines := strings.Split(strings.TrimSpace(out), "\n")
	assert.Equal(t, "SecRuleEngine DetectionOnly", lines[len(lines)-1], "nothing may be able to switch the engine on after it")
	assert.Equal(t, 1, strings.Count(out, "SecRuleEngine"), "and exclusions never mention it")
}

// Every rendered line is one of a small number of shapes. A renderer that emitted
// something else — whatever the cause — fails here before it reaches Caddy.
func TestRenderWAFExclusions_EveryLineMatchesAnAllowedShape(t *testing.T) {
	shapes := []*regexp.Regexp{
		regexp.MustCompile(`^SecAction "id:11\d{5},phase:1,pass,nolog,ctl:(?:ruleRemoveById=\d{6}|ruleRemoveTargetById=\d{6};[A-Z_]+:[A-Za-z0-9_.-]{1,80})"$`),
		regexp.MustCompile(`^SecRule REQUEST_HEADERS:X-Belune-Waf-Tx "@rx \(\?i\)\^\(\?:[A-Za-z0-9_.\\\[\]+*|()-]+\)_\[0-9a-f-\]\{36\}\$" "id:11\d{5},phase:1,pass,nolog,(?:chain|ctl:(?:ruleRemoveById=\d{6}|ruleRemoveTargetById=\d{6};[A-Z_]+:[A-Za-z0-9_.-]{1,80}))"$`),
		regexp.MustCompile(`^SecRule REQUEST_FILENAME "@(?:streq|beginsWith) /[A-Za-z0-9._~/-]*" "id:11\d{5},phase:1,pass,nolog,ctl:ruleRemoveById=\d{6}"$`),
		regexp.MustCompile(`^  SecRule REQUEST_FILENAME "@(?:streq|beginsWith) /[A-Za-z0-9._~/-]*" "ctl:ruleRemoveById=\d{6}"$`),
	}
	out := renderWAFExclusions([]proxy.WAFExclusion{
		withTarget(ex(1, 942290, true), "REQUEST_COOKIES", "ph_x"),
		withTarget(ex(2, 942290, false, "a.example.com"), "ARGS_NAMES", "q"),
		withPath(ex(3, 930130, true), "/.env.local"),
		withPath(ex(4, 930130, false, "a.example.com", "*.b.example.com"), "/api/*"),
		ex(5, 941100, true),
		ex(6, 941100, false, "a.example.com"),
	})
	for _, line := range strings.Split(strings.TrimSuffix(out, "\n"), "\n") {
		ok := false
		for _, re := range shapes {
			if re.MatchString(line) {
				ok = true
				break
			}
		}
		assert.True(t, ok, "unexpected directive shape: %s", line)
	}
}
