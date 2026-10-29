package caddy

import (
	"fmt"
	"log/slog"
	"regexp"
	"sort"
	"strings"

	"github.com/belune-hq/belune/internal/proxy"
	"github.com/belune-hq/belune/internal/waf"
)

// wafExclusionRuleBase is where exclusion rule ids start. Clear of the Core Rule
// Set (900000-999999) and of the three body rules in the head (1000001-3).
//
// Ids are POSITIONAL: the Nth exclusion, in id order, is base+N. They are bounded
// by MaxWAFExclusions, collide with nothing by construction, and cannot overflow
// the way a row id that grows forever eventually would. They move only when the
// set changes, which already changes the bytes.
const wafExclusionRuleBase = 1100000

// wafBlockingRuleID is the one rule that promotes the engine. Clear of the Core
// Rule Set (900000-999999), the three body rules (1000001-3) and the exclusions
// (1100000 upward, bounded by MaxWAFExclusions). A duplicate id would likely make
// Caddy reject the whole write and take the WAF route down, not one rule.
const wafBlockingRuleID = 1200001

var (
	// A hostname is placed in a regular expression, so it is held to what a
	// hostname is: lower-case letters, digits, '-', '_' and '.', with a '*' label
	// standing for any one label, as Caddy's own host matcher reads it.
	hostLabelRe = regexp.MustCompile(`^(?:\*|[a-z0-9_-]{1,63})$`)
)

// hostScopeCondition matches the transaction header for these hosts. That header
// is the one Belune SETS from {http.request.host} before the module runs (the
// client cannot supply it), so it is Caddy's own normalised host: no port, and
// the same name the route matched on. Matching the client's Host header would
// have needed the port and the case handled by hand.
//
// Anchored at both ends, so a.test does not cover a.test.evil or xa.test (measured).
func hostScopeCondition(hosts []string) (string, bool) {
	alts := make([]string, 0, len(hosts))
	for _, h := range wafHosts(hosts) {
		labels := strings.Split(h, ".")
		parts := make([]string, 0, len(labels))
		ok := true
		for _, l := range labels {
			if !hostLabelRe.MatchString(l) {
				ok = false
				break
			}
			if l == "*" {
				parts = append(parts, `[a-z0-9_-]+`)
			} else {
				parts = append(parts, regexp.QuoteMeta(l))
			}
		}
		if !ok {
			// A host that is not one is left out, never guessed at: a smaller set is
			// a narrower exclusion.
			slog.Warn("caddy: a WAF exclusion names a host it cannot scope to; leaving it out", "host", clip(h))
			continue
		}
		alts = append(alts, strings.Join(parts, `\.`))
	}
	if len(alts) == 0 {
		return "", false
	}
	return fmt.Sprintf(`REQUEST_HEADERS:%s "@rx (?i)^(?:%s)_[0-9a-f-]{36}$"`, waf.TxHeader, strings.Join(alts, "|")), true
}

func clip(s string) string {
	if len(s) > 64 {
		return s[:64] + "…"
	}
	return s
}

// renderWAFExclusions renders one rule per exclusion, in id order.
//
// Fail safe in every direction: an exclusion that does not validate, or that is
// scoped to an application with no domains, renders NOTHING. The consequence of
// skipping is a rule that keeps detecting; the consequence of guessing could be a
// rule switched off for the whole install.
func renderWAFExclusions(exclusions []proxy.WAFExclusion) string {
	sorted := append([]proxy.WAFExclusion(nil), exclusions...)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].ID < sorted[j].ID })

	var b strings.Builder
	n := 0
	for _, e := range sorted {
		if n >= proxy.MaxWAFExclusions {
			slog.Warn("caddy: more WAF exclusions than the ceiling; the rest are ignored", "ceiling", proxy.MaxWAFExclusions)
			break
		}
		if err := proxy.ValidateWAFExclusion(e.WAFExclusionSpec); err != nil {
			slog.Warn("caddy: skipping a WAF exclusion that does not validate", "id", e.ID, "error", err)
			continue
		}

		var condition string
		if !e.Global {
			c, ok := hostScopeCondition(e.Hosts)
			if !ok {
				continue // an application with no domains: inert, not install-wide
			}
			condition = c
		}

		n++
		id := wafExclusionRuleBase + n
		var ctl string
		switch e.Shape() {
		case "target":
			ctl = fmt.Sprintf("ctl:ruleRemoveTargetById=%d;%s:%s", e.RuleID, e.Collection, e.TargetName)
		default:
			ctl = fmt.Sprintf("ctl:ruleRemoveById=%d", e.RuleID)
		}

		if e.Shape() == "path" {
			op := fmt.Sprintf(`REQUEST_FILENAME "@streq %s"`, e.PathPattern)
			if prefix, ok := strings.CutSuffix(e.PathPattern, "*"); ok {
				op = fmt.Sprintf(`REQUEST_FILENAME "@beginsWith %s"`, prefix)
			}
			if e.Global {
				fmt.Fprintf(&b, "SecRule %s \"id:%d,phase:1,pass,nolog,%s\"\n", op, id, ctl)
			} else {
				// The host is the first link and carries the id; the path is the
				// second, and it carries the action.
				fmt.Fprintf(&b, "SecRule %s \"id:%d,phase:1,pass,nolog,chain\"\n", condition, id)
				fmt.Fprintf(&b, "  SecRule %s \"%s\"\n", op, ctl)
			}
			continue
		}

		if e.Global {
			fmt.Fprintf(&b, "SecAction \"id:%d,phase:1,pass,nolog,%s\"\n", id, ctl)
		} else {
			fmt.Fprintf(&b, "SecRule %s \"id:%d,phase:1,pass,nolog,%s\"\n", condition, id, ctl)
		}
	}
	return b.String()
}

// renderWAFBlocking promotes the engine to On for these hosts, and only these.
//
// ⛔ Fail safe, by construction. The engine is DetectionOnly unless this rule
// matches, so everything that can go wrong here goes wrong toward detecting:
//   - no hosts, or none that can be scoped, renders NOTHING;
//   - a host that is not a plain hostname is left out, never guessed at;
//   - there is NO global form. An exclusion falls back to a SecAction when it is
//     install-wide; a blocking rule that lost its host scope and did the same
//     would switch the engine on for every application. It cannot: the only
//     output shape is the host-scoped SecRule.
//
// The condition matches the transaction header Belune sets from
// {http.request.host}, which the client cannot supply, so a visitor cannot aim
// enforcement at a host by sending its name.
func renderWAFBlocking(hosts []string) string {
	condition, ok := hostScopeCondition(hosts)
	if !ok {
		return ""
	}
	return fmt.Sprintf("SecRule %s \"id:%d,phase:1,pass,nolog,ctl:ruleEngine=On\"\n", condition, wafBlockingRuleID)
}
