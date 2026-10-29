package caddy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sort"
	"strings"

	"github.com/belune-hq/belune/internal/pkg/metrics"
	"github.com/belune-hq/belune/internal/proxy"
	"github.com/belune-hq/belune/internal/waf"
)

// wafRouteID is the WAF's route: one route in front of every application route,
// not a handler in each. It is not an application route (nothing in the domains
// table backs it), so ListRoutes skips it, and reorderRoutes pins it first.
const wafRouteID = "route-waf"

// wafDirectives is the Coraza configuration, and every line of it is here because
// the module's own defaults were measured to break traffic or leak. Measured
// against Caddy v2.11.7 with coraza-caddy v2.6.2, the build in belune-caddy:2:
//
//   - SecRuleEngine DetectionOnly is the static default, stated last, after the
//     includes, so nothing they say can change it at load time. It can be
//     promoted per transaction by exactly one thing: the host-scoped
//     ctl:ruleEngine=On rules renderWAFBlocking generates from the applications
//     that opted in. That is why the default is DetectionOnly and the promotion
//     is per host, never the inverse: with the engine On and a per-host demotion,
//     a rule that is missing, skipped or malformed would start refusing visitors
//     nobody asked to refuse, where here it leaves them detected.
//
//   - Request bodies are inspected only when they are small and of known size. Left
//     to the module's own defaults, a request body over about 1 MiB stalls and
//     arrives truncated (the connection hangs), even in DetectionOnly and even with
//     SecRequestBodyLimitAction ProcessPartial. A detection-only feature that can
//     break uploads is worse than none, so two protections are applied, and each
//     was measured against a real Caddy to be sufficient on its own: capping
//     SecRequestBodyLimit at 128 KiB, and the three SecRules that turn body
//     inspection off for a body with no Content-Length, a large one, or any
//     Transfer-Encoding. Both stay. The failure is total and silent, the cause is
//     in the module and is not understood, and either one alone is a single line
//     from being deleted as redundant. The accepted cost is that a payload inside a
//     large or chunked body goes unseen; URLs, headers, query strings and bodies
//     up to 128 KiB are inspected, which is where scanners and injection attempts
//     live.
//
//   - SecResponseBodyAccess Off: the recommended setting buffers text responses,
//     which has no place in front of arbitrary applications.
//
//   - SecAuditEngine Off: the recommended audit log records request headers and
//     bodies, which would put credentials into a log.
//
//   - SecDebugLogLevel 1: at the default, the engine writes several WARN lines per
//     request through the same logger the rule matches arrive on, for clean
//     requests too. Rule matches are not affected by it.
const wafDirectivesHead = `Include @coraza.conf-recommended
Include @crs-setup.conf.example
`

const wafDirectivesTail = `Include @owasp_crs/*.conf
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

// wafDirectives is the configuration with no exclusions: what an install that has
// never made one runs, byte for byte, so adding the feature is not a route diff.
var wafDirectives = renderWAFDirectives(nil, nil)

// renderWAFDirectives is the Coraza configuration for these exclusions and
// blocking hosts.
//
// ⚠️ The exclusion block sits BETWEEN the setup include and the rule set, and that
// is not a style choice: a ctl:ruleRemove* only affects rules that have not run
// yet, and a rule defined earlier runs earlier. Placed after the rule set it
// removed a phase-2 rule but left a phase-1 one firing (measured against Caddy
// v2.11.7 with coraza-caddy v2.6.2).
//
// SecRuleEngine DetectionOnly stays last, so nothing the includes say can switch
// the engine on at load. The exclusion rules can only REMOVE rules and targets
// from a transaction. The blocking rules are the one thing that promotes the
// engine, only for the hosts that opted in, and they sit in the same place as the
// exclusions for the same reason: the switch must be made before the phase-1
// rules it governs have run (measured: before or after the static directive, the
// per-host rule still wins at request time).
//
// ⚠️ That measurement does NOT license moving the block. Placed after the
// includes and the static directive, a ctl:ruleEngine=On rule also won for the
// anomaly verdict, because the switch only has to land before phase 2. That is a
// different question from the one the exclusions answered: a ctl placed after the
// includes left a PHASE-1 CRS rule (920350) firing. The position is not arbitrary
// and a control that also passes is not a reason to simplify it.
func renderWAFDirectives(exclusions []proxy.WAFExclusion, blockingHosts []string) string {
	return wafDirectivesHead + renderWAFExclusions(exclusions) + renderWAFBlocking(blockingHosts) + wafDirectivesTail
}

// buildWAFRoute renders the WAF route for these hosts.
//
// It has no `terminal`, so Caddy carries on to the application's own route once
// the module has looked at the request.
//
// The first handler writes "<host>_<uuid>" into a header and the module reads it as
// its transaction id (tx_id_req_header). Coraza logs that id as unique_id, so the
// host a request asked for arrives in the rule-match line itself: no second
// pipeline has to be joined to learn it. The header is SET, not appended, so a
// client cannot supply its own, and the last handler removes it so the application
// never sees it.
func buildWAFRoute(hosts []string, exclusions []proxy.WAFExclusion, blockingHosts []string) caddyRoute {
	return caddyRoute{
		ID:    wafRouteID,
		Match: []caddyMatch{{Host: hosts}},
		Handle: []caddyHandle{
			{
				"handler": "headers",
				"request": map[string]any{
					"set": map[string][]string{waf.TxHeader: {"{http.request.host}_{http.request.uuid}"}},
				},
			},
			{
				"handler":          "waf",
				"load_owasp_crs":   true,
				"directives":       renderWAFDirectives(exclusions, blockingHosts),
				"tx_id_req_header": waf.TxHeader,
			},
			{
				"handler": "headers",
				"request": map[string]any{"delete": []string{waf.TxHeader}},
			},
		},
	}
}

// SyncWAF makes the WAF route match cfg. It is called every reconcile pass, so it
// is a GET when nothing has changed and a write only on a real difference: a write
// is a config reload.
func (c *Client) SyncWAF(ctx context.Context, cfg proxy.WAFConfig) (err error) {
	defer func() { metrics.RecordCaddyCall("sync_waf", err) }()

	hosts := wafHosts(cfg.Hosts)
	current, found, err := c.fetchRouteByID(ctx, wafRouteID)
	if err != nil {
		return err
	}

	if !cfg.Enabled || len(hosts) == 0 {
		if !found {
			return nil
		}
		if err := c.deleteRouteByID(ctx, wafRouteID); err != nil {
			return err
		}
		slog.Info("caddy: WAF route removed")
		return nil
	}

	desired := buildWAFRoute(hosts, cfg.Exclusions, wafBlockingHosts(hosts, cfg.BlockingHosts))
	if found {
		same, err := routeJSONEqual(current, desired)
		if err != nil {
			return err
		}
		if same {
			return nil
		}
		// Replaced where it stands, so it stays first.
		if err := c.sendJSON(ctx, http.MethodPatch, "/id/"+wafRouteID, desired); err != nil {
			return fmt.Errorf("update WAF route: %w", err)
		}
		slog.Info("caddy: WAF route updated", "hosts", len(hosts))
		return nil
	}

	// Index 0: it has to run before any application route, because routes are
	// first-match-wins and the application's reverse_proxy ends the chain. Index 1
	// when the CrowdSec route holds the front: a banned address should be turned
	// away before the WAF spends anything looking at it.
	at := 0
	if _, behindCrowdSec, err := c.fetchRouteByID(ctx, crowdsecRouteID); err != nil {
		return err
	} else if behindCrowdSec {
		at = 1
	}
	if err := c.sendJSON(ctx, http.MethodPut, fmt.Sprintf("/config/apps/http/servers/srv0/routes/%d", at), desired); err != nil {
		return fmt.Errorf("add WAF route: %w", err)
	}
	slog.Info("caddy: WAF route added", "hosts", len(hosts))
	return nil
}

// wafBlockingHosts keeps only the blocking hosts the route also inspects, in the
// route's own form. Belt and braces over the reconciler's narrowing: a host the
// WAF does not match can never be enforced on, whoever built the config.
func wafBlockingHosts(hosts, blocking []string) []string {
	inRoute := make(map[string]struct{}, len(hosts))
	for _, h := range hosts {
		inRoute[h] = struct{}{}
	}
	out := make([]string, 0, len(blocking))
	for _, h := range wafHosts(blocking) {
		if _, ok := inRoute[h]; ok {
			out = append(out, h)
		}
	}
	return out
}

// wafHosts is the host list as the route carries it: deduplicated, lower-cased and
// sorted, because Caddy's matcher is case-insensitive and the reconciler compares
// what it wrote against what is there. A list in a different order each pass would
// look like drift for ever and reload Caddy every 30 seconds.
func wafHosts(hosts []string) []string {
	seen := make(map[string]struct{}, len(hosts))
	out := make([]string, 0, len(hosts))
	for _, h := range hosts {
		h = strings.ToLower(strings.TrimSpace(h))
		if h == "" {
			continue
		}
		if _, dup := seen[h]; dup {
			continue
		}
		seen[h] = struct{}{}
		out = append(out, h)
	}
	sort.Strings(out)
	return out
}

// sendJSON writes one config value and reports a refusal as an error. Unlike
// doConfig it does not treat an unreachable Caddy or an HTTP error as ignorable:
// Caddy rejects the whole write when a module is absent or the directives do not
// load, and the caller must hear about it.
func (c *Client) sendJSON(ctx context.Context, method, path string, v any) error {
	body, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.adminURL+path, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		if isTransportError(err) {
			return fmt.Errorf("%w: %v", ErrCaddyUnreachable, err)
		}
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(respBody)))
	}
	return nil
}
