package proxy

import (
	"context"
	"time"
)

// Degrade exposes the capability filter so it can be tested without a database.
func (r *Reconciler) Degrade(ctx context.Context, expected []RouteConfig) ([]RouteConfig, []Degradation) {
	return r.degrade(ctx, expected)
}

// ProbeLostConfig exposes one probe so a test can drive the transitions itself.
func (r *Reconciler) ProbeLostConfig(ctx context.Context) { r.probeLostConfig(ctx) }

// CaddyProbeInterval lets a test wait in units of the real probe cadence.
const CaddyProbeInterval = caddyProbeInterval

// States and the two pure probe decisions, for a table test that needs no Docker.
const (
	CaddyUnknown     = caddyUnknown
	CaddyUnreachable = caddyUnreachable
	CaddyHealthy     = caddyHealthy
	CaddyLostConfig  = caddyLostConfig
)

type CaddyState = caddyState

func NextCaddyState(routes []RouteConfig, err error, lastExpected int) CaddyState {
	return nextCaddyState(routes, err, lastExpected)
}

func ProbeShouldReconcile(prev, next CaddyState, lastPassErrored bool) bool {
	return probeShouldReconcile(prev, next, lastPassErrored)
}

// CarrySince exposes the Since bookkeeping so it can be tested without a database.
func CarrySince(prev, next []Degradation, now time.Time) []Degradation {
	return carrySince(prev, next, now)
}

// DegradationsChanged exposes the log-once decision so it can be tested without
// a database.
func DegradationsChanged(prev, next []Degradation) bool {
	return degradationsChanged(prev, next)
}

// SyncWAF exposes the WAF step of a pass, with the operator's switch supplied by
// the test, so it can be exercised without a database.
func (r *Reconciler) SyncWAF(ctx context.Context, expected []RouteConfig, dashboardHost string, wanted func(context.Context) (bool, error)) error {
	r.wafSwitch = wanted
	return r.syncWAF(ctx, expected, dashboardHost)
}
