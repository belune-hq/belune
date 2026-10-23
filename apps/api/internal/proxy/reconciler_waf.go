package proxy

import (
	"context"
	"errors"
	"fmt"
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

	if err := r.proxy.SyncWAF(ctx, WAFConfig{Enabled: wanted, Hosts: sorted}); err != nil {
		state.Active, state.Error = prev.Active, err.Error()
		r.setWAF(state)
		return fmt.Errorf("syncing the WAF route: %w", err)
	}
	state.Active = wanted && len(sorted) > 0
	r.setWAF(state)
	return nil
}
