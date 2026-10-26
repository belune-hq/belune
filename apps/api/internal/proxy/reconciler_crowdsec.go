package proxy

import (
	"context"
	"fmt"
)

// ReasonCrowdSecUnavailable means the switch is on, the module is there, and the
// CrowdSec local API is not usable: the container is absent or down, or its
// bouncer could not be registered.
const ReasonCrowdSecUnavailable = "lapi_unavailable"

// CrowdSecDesire is what the CrowdSec side of Belune wants the proxy to carry.
type CrowdSecDesire struct {
	// Enabled is the operator's switch.
	Enabled bool
	// Ready is whether the local API answers and the bouncer is registered, i.e.
	// whether a route that talks to it would work. Not ready is not the same as
	// off: a route already in Caddy keeps enforcing from memory through a blip.
	Ready bool
	// Reason says why it is not ready.
	Reason string
	APIURL string
	APIKey string
}

// String keeps the key out of any log line that formats the struct whole.
func (d CrowdSecDesire) String() string {
	return fmt.Sprintf("CrowdSecDesire{enabled=%t ready=%t reason=%q api_key=[REDACTED]}", d.Enabled, d.Ready, d.Reason)
}

// CrowdSecSource supplies the CrowdSec side's wishes each pass. It also checks on
// the local API and registers the bouncer if need be, which is why the reconciler
// asks it rather than reading the switch from the database itself.
type CrowdSecSource interface {
	Prepare(ctx context.Context) (CrowdSecDesire, error)
}

// CrowdSecState is what the reconciler last did about CrowdSec, for the panel.
type CrowdSecState struct {
	// Active is whether the bouncer route is in Caddy now.
	Active bool `json:"active"`
	// Hosts is how many application hostnames it covers.
	Hosts int `json:"hosts"`
	// Reason says why it is not active although the operator turned it on:
	// ReasonModuleAbsent, ReasonModulesUnknown, ReasonNoApplications or
	// ReasonCrowdSecUnavailable. Empty when there is nothing to explain.
	Reason string `json:"reason,omitempty"`
	// Error is the last refusal when Caddy would not take the config.
	Error string `json:"error,omitempty"`
}

// CrowdSec reports the state the last pass left the bouncer in.
func (r *Reconciler) CrowdSec() CrowdSecState {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.crowdsec
}

func (r *Reconciler) setCrowdSec(s CrowdSecState) {
	r.mu.Lock()
	r.crowdsec = s
	r.mu.Unlock()
}

// WithCrowdSec makes the reconciler carry the CrowdSec bouncer for src.
func (r *Reconciler) WithCrowdSec(src CrowdSecSource) *Reconciler {
	r.crowdsecSrc = src
	return r
}

// syncCrowdSec brings the bouncer in line with the switch, the module, the local
// API and the applications that exist. Called after the WAF step, so on a first
// pass the bouncer's route is written second and lands in front of the WAF's.
//
// As with the WAF, the three answers about the module are kept apart: present
// means write, absent means remove (Caddy would refuse a route naming it), and
// "could not ask" means touch nothing. A fourth answer is specific to this
// feature: a local API that is down is not a reason to remove a bouncer that is
// working, since the module goes on enforcing the bans it already holds, so the
// route is left exactly as it is.
func (r *Reconciler) syncCrowdSec(ctx context.Context, expected []RouteConfig, dashboardHost string) error {
	if r.crowdsecSrc == nil {
		return nil
	}
	prev := r.CrowdSec()

	desire, err := r.crowdsecSrc.Prepare(ctx)
	if err != nil {
		// The switch could not be read, or registration failed. Nothing is changed
		// on a guess in either direction.
		prev.Error, prev.Reason = err.Error(), ReasonCrowdSecUnavailable
		r.setCrowdSec(prev)
		return fmt.Errorf("preparing CrowdSec: %w", err)
	}

	hosts := newHostSet()
	for _, cfg := range expected {
		if cfg.Hostname != "" && cfg.Hostname != dashboardHost {
			hosts.add(cfg.Hostname)
		}
	}
	sorted := hosts.sorted()

	state := CrowdSecState{Hosts: len(sorted)}
	wanted := desire.Enabled
	if wanted {
		var known, present bool
		if r.caps != nil {
			set := r.caps.Capabilities(ctx)
			known, present = set.Known(), set.CrowdSec()
		}
		switch {
		case !known:
			state.Active, state.Reason = prev.Active, ReasonModulesUnknown
			r.setCrowdSec(state)
			return nil
		case !present:
			wanted, state.Reason = false, ReasonModuleAbsent
		case !desire.Ready:
			state.Active, state.Reason = prev.Active, desire.Reason
			if state.Reason == "" {
				state.Reason = ReasonCrowdSecUnavailable
			}
			r.setCrowdSec(state)
			return nil
		case len(sorted) == 0:
			state.Reason = ReasonNoApplications
		}
	}

	cfg := CrowdSecConfig{Enabled: wanted, APIURL: desire.APIURL, APIKey: desire.APIKey, Hosts: sorted}
	if dashboardHost != "" {
		cfg.Exempt = []string{dashboardHost}
	}
	if err := r.proxy.SyncCrowdSec(ctx, cfg); err != nil {
		state.Active, state.Error = prev.Active, err.Error()
		r.setCrowdSec(state)
		return fmt.Errorf("syncing the CrowdSec route: %w", err)
	}
	state.Active = wanted && len(sorted) > 0
	r.setCrowdSec(state)
	return nil
}
