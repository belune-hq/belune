package instances

import (
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/belune-hq/belune/internal/store/generated"
)

// Health sources: where a resolved health check came from.
const (
	HealthSourceSet       = "set"       // the process type chose it
	HealthSourceInherited = "inherited" // the application's, because the type is routable and set none
	HealthSourceDefault   = "default"   // the rule's fallback for a non-routable type
)

// Health is the health check a process type actually runs with.
type Health struct {
	Type    string // "http", "command" or "none"
	Command pgtype.Text
	Source  string
	// Inert means the check resolves to something that can never run: an http check
	// with no path. Type stays "http" -- the operator chose it, they just have not
	// said WHERE to probe -- so this is a separate fact, not a flattening to "none".
	// The deploy gate and the UI both read it, so neither can disagree about whether
	// a deploy is actually verified. Reason says why, for display.
	Inert  bool
	Reason string
}

// HealthInertNoPath is Reason for an http check with no path.
const HealthInertNoPath = "no health check path is set, so nothing is probed"

// withInert is the ONE definition of "configured but checks nothing". It is applied to
// every return of ResolveHealth so the gate (worker.verifyRoutableHealth) and the API
// read the same answer. A rolling deploy's safety is this gate: without a check the
// new generation is never verified before the old one is removed.
func withInert(app generated.Application, h Health) Health {
	if h.Type == "http" && (!app.HealthCheckPath.Valid || app.HealthCheckPath.String == "") {
		h.Inert, h.Reason = true, HealthInertNoPath
	}
	return h
}

// ResolveHealth is the ONE place the health-check rule lives. The deploy worker
// (the Docker HEALTHCHECK and the deploy gate) and the API (what the operator is TOLD
// is running) both read it, because two copies of a three-line rule is how the
// http-inheritance bug ships later.
//
//	process type sets a type -> use it
//	else routable            -> inherit the application's
//	else                     -> 'none'
//
// ⚠️ The last line is load-bearing: a non-routable type must NEVER inherit 'http'.
// http is a one-shot readiness gate the control plane runs over the network against
// a port, and a worker listens on nothing, so inheriting it would make every worker
// permanently unhealthy. 'command' is the mechanism meant for workers, and it must be
// set on the type explicitly.
//
// A type's own health_check_command, when NULL, inherits the application's, which is
// what the column means. HTTP knobs (path, container_port, expect_status) and the
// Docker timing knobs stay application-level and are not resolved here.
func ResolveHealth(app generated.Application, pt generated.ApplicationProcessType) Health {
	if pt.HealthCheckType.Valid && pt.HealthCheckType.String != "" {
		cmd := app.HealthCheckCommand
		if pt.HealthCheckCommand.Valid {
			cmd = pt.HealthCheckCommand
		}
		return withInert(app, Health{Type: pt.HealthCheckType.String, Command: cmd, Source: HealthSourceSet})
	}
	if pt.Routable {
		return withInert(app, Health{Type: app.HealthCheckType, Command: app.HealthCheckCommand, Source: HealthSourceInherited})
	}
	return Health{Type: "none", Source: HealthSourceDefault}
}
