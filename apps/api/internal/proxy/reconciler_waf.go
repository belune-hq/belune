package proxy

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/jackc/pgx/v5"
)

// WAFState is what the reconciler last did about the WAF, for the panel to report.
type WAFState struct {
	// Active is whether the WAF route is in Caddy now.
	Active bool `json:"active"`
	// Hosts is how many application hostnames it inspects.
	Hosts int `json:"hosts"`
	// Reason says why the WAF is not active although the operator turned it on:
	// ReasonModuleAbsent, ReasonModulesUnknown or ReasonNoApplications. Empty when
	// there is nothing to explain.
	Reason string `json:"reason,omitempty"`
	// Error is the last refusal when Caddy would not take the route.
	Error string `json:"error,omitempty"`
}

// ReasonNoApplications means the switch is on and there is nothing to inspect yet.
const ReasonNoApplications = "no_applications"

// WAF reports the state the last pass left the WAF in.
func (r *Reconciler) WAF() WAFState {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.waf
}

func (r *Reconciler) setWAF(s WAFState) {
	r.mu.Lock()
	r.waf = s
	r.mu.Unlock()
}

// wafWanted reads the operator's switch. Only "true" is on. A missing row is off;
// any other error is returned, because reading the switch wrong in the direction
// of off would remove a working route over a transient database error.
func (r *Reconciler) wafWanted(ctx context.Context) (bool, error) {
	if r.wafSwitch != nil {
		return r.wafSwitch(ctx)
	}
	s, err := r.queries.GetSetting(ctx, SettingWAFEnabled)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("reading %s: %w", SettingWAFEnabled, err)
	}
	return strings.TrimSpace(s.Value) == "true", nil
}

// wafExclusions reads the operator's carve-outs with the hosts they apply to now.
// An error is returned and nothing is pushed: a route rendered without the
// exclusions would quietly start detecting what the operator had silenced, and
// then flip back on the next good read.
func (r *Reconciler) wafExclusions(ctx context.Context) ([]WAFExclusion, error) {
	if r.wafExclusionList != nil {
		return r.wafExclusionList(ctx)
	}
	rows, err := r.queries.ListWAFExclusions(ctx)
	if err != nil {
		return nil, fmt.Errorf("reading the WAF exclusions: %w", err)
	}
	out := make([]WAFExclusion, 0, len(rows))
	for _, row := range rows {
		out = append(out, WAFExclusion{
			ID: row.ID,
			WAFExclusionSpec: WAFExclusionSpec{
				RuleID:      int(row.RuleID),
				Collection:  row.Collection.String,
				TargetName:  row.TargetName.String,
				PathPattern: row.PathPattern.String,
			},
			// Install-wide ONLY when no application is named. An application whose
			// domains are gone has Valid set and no hostnames, and must stay inert.
			Global: !row.ApplicationID.Valid,
			Hosts:  row.Hostnames,
		})
	}
	return out, nil
}

// wafBlockingCandidates reads the hostnames of applications that opted in to
// blocking. Like the exclusions, an error is returned and nothing is pushed: a
// pass that read the list as empty would quietly demote every blocking host to
// detection-only and then promote it again on the next good read.
func (r *Reconciler) wafBlockingCandidates(ctx context.Context) ([]string, error) {
	if r.wafBlockingList != nil {
		return r.wafBlockingList(ctx)
	}
	hosts, err := r.queries.ListWAFBlockingHostnames(ctx)
	if err != nil {
		return nil, fmt.Errorf("reading the WAF blocking hosts: %w", err)
	}
	return hosts, nil
}

// blockingHosts narrows the opted-in hostnames to the ones the WAF may enforce
// on. The result is always a subset of hosts, so the dashboard (which never
// reaches hosts) can never be enforced on, and a host with no route is not named.
//
// ⛔ Two locks keep the dashboard out, and neither is redundant. The first is
// that the dashboard never reaches hosts, so it cannot be named. That does not
// stop a WILDCARD from covering it: "*.example.com" is a valid application
// domain, Caddy's host matcher glob-matches it, and the WAF route (first in the
// table, not terminal) then sees "panel.example.com" and the scoping pattern
// matches it too. The dashboard takes commands and credentials as ordinary input,
// which a rule set reads as attacks, and its recovery path is the dashboard, so
// the second lock drops any candidate whose pattern covers dashboardHost.
//
// A wildcard is dropped when it overlaps a host that did NOT opt in: the rule
// that scopes it is a pattern over the request's host, and "*.example.com" would
// also cover a sibling application's "shop.example.com" and start refusing its
// visitors unasked. Dropping it costs the wildcard's own hosts their enforcement,
// which is the cheaper way to be wrong.
func blockingHosts(hosts, candidates []string, dashboardHost string) []string {
	inScope := make(map[string]struct{}, len(hosts))
	for _, h := range hosts {
		inScope[h] = struct{}{}
	}
	want := make(map[string]struct{}, len(candidates))
	for _, c := range candidates {
		c = strings.ToLower(strings.TrimSpace(c))
		if _, ok := inScope[c]; ok {
			want[c] = struct{}{}
		}
	}

	out := make([]string, 0, len(want))
	for _, h := range hosts {
		if _, ok := want[h]; !ok {
			continue
		}
		if dashboardHost != "" && hostPatternsOverlap(h, strings.ToLower(dashboardHost)) {
			slog.Warn("WAF blocking: leaving a host detection-only because it would cover the dashboard", "host", h, "dashboard", dashboardHost)
			continue
		}
		if strings.Contains(h, "*") {
			overlaps := ""
			for _, other := range hosts {
				if _, optedIn := want[other]; !optedIn && hostPatternsOverlap(h, other) {
					overlaps = other
					break
				}
			}
			if overlaps != "" {
				slog.Warn("WAF blocking: leaving a wildcard host detection-only because it overlaps a host that has not opted in", "host", h, "overlaps", overlaps)
				continue
			}
		}
		out = append(out, h)
	}
	return out
}

// hostPatternsOverlap reports whether some hostname could match both patterns,
// reading "*" as any one label, as Caddy's host matcher does.
func hostPatternsOverlap(a, b string) bool {
	al, bl := strings.Split(a, "."), strings.Split(b, ".")
	if len(al) != len(bl) {
		return false
	}
	for i := range al {
		if al[i] != bl[i] && al[i] != "*" && bl[i] != "*" {
			return false
		}
	}
	return true
}

// syncWAF brings the WAF route in line with the switch, the module and the
// applications that exist. Called last in a pass, once the application routes are
// in place, so the host list it carries is the one they have.
//
// The three capability answers are three different things and are kept apart:
//   - the module is present: the route is written;
//   - the module is absent: the route is removed, because Caddy would refuse a
//     route that names it;
//   - Belune could not ask (restarting, no exec access): nothing is touched. A
//     failed probe is not evidence the module is gone, and acting on it would
//     remove a working WAF and put it back on the next pass.
func (r *Reconciler) syncWAF(ctx context.Context, expected []RouteConfig, dashboardHost string) error {
	wanted, err := r.wafWanted(ctx)
	if err != nil {
		return err
	}
	exclusions, err := r.wafExclusions(ctx)
	if err != nil {
		return err
	}
	candidates, err := r.wafBlockingCandidates(ctx)
	if err != nil {
		return err
	}

	hosts := newHostSet()
	for _, cfg := range expected {
		if cfg.Hostname != "" && cfg.Hostname != dashboardHost {
			hosts.add(cfg.Hostname)
		}
	}
	sorted := hosts.sorted()

	prev := r.WAF()
	state := WAFState{Hosts: len(sorted)}
	if wanted {
		var known, present bool
		if r.caps != nil {
			set := r.caps.Capabilities(ctx)
			known, present = set.Known(), set.WAF()
		}
		switch {
		case !known:
			// Leave the route as it is, and say what is true of it.
			state.Active, state.Reason = prev.Active, ReasonModulesUnknown
			r.setWAF(state)
			return nil
		case !present:
			wanted, state.Reason = false, ReasonModuleAbsent
		case len(sorted) == 0:
			state.Reason = ReasonNoApplications
		}
	}

	if err := r.proxy.SyncWAF(ctx, WAFConfig{Enabled: wanted, Hosts: sorted, BlockingHosts: blockingHosts(sorted, candidates, dashboardHost), Exclusions: exclusions}); err != nil {
		state.Active, state.Error = prev.Active, err.Error()
		r.setWAF(state)
		return fmt.Errorf("syncing the WAF route: %w", err)
	}
	state.Active = wanted && len(sorted) > 0
	r.setWAF(state)
	return nil
}
