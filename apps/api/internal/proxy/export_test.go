package proxy

import "context"

// Degrade exposes the capability filter so it can be tested without a database.
func (r *Reconciler) Degrade(ctx context.Context, expected []RouteConfig) ([]RouteConfig, []Degradation) {
	return r.degrade(ctx, expected)
}
