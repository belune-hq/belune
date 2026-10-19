package proxy

import "context"

// Degrade exposes the capability filter so it can be tested without a database.
func (r *Reconciler) Degrade(ctx context.Context, expected []RouteConfig) ([]RouteConfig, []Degradation) {
	return r.degrade(ctx, expected)
}

// ProbeLostConfig exposes one probe so a test can drive the transitions itself.
func (r *Reconciler) ProbeLostConfig(ctx context.Context) { r.probeLostConfig(ctx) }

// CaddyProbeInterval lets a test wait in units of the real probe cadence.
const CaddyProbeInterval = caddyProbeInterval
