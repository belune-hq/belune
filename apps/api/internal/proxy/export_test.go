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
	if r.wafExclusionList == nil {
		r.wafExclusionList = func(context.Context) ([]WAFExclusion, error) { return nil, nil }
	}
	if r.wafBlockingList == nil {
		r.wafBlockingList = func(context.Context) ([]string, error) { return nil, nil }
	}
	return r.syncWAF(ctx, expected, dashboardHost)
}

// WithWAFBlockingHosts supplies the opted-in hostnames a pass reads.
func (r *Reconciler) WithWAFBlockingHosts(list func(context.Context) ([]string, error)) *Reconciler {
	r.wafBlockingList = list
	return r
}

// BlockingHosts exposes the narrowing of opted-in hostnames to enforceable ones.
func BlockingHosts(hosts, candidates []string, dashboardHost string) []string {
	return blockingHosts(hosts, candidates, dashboardHost)
}

// WithWAFExclusions supplies the exclusions a pass reads, so a test needs no database.
func (r *Reconciler) WithWAFExclusions(list func(context.Context) ([]WAFExclusion, error)) *Reconciler {
	r.wafExclusionList = list
	return r
}

// SyncCrowdSec exposes the CrowdSec step of a pass.
func (r *Reconciler) SyncCrowdSec(ctx context.Context, expected []RouteConfig, dashboardHost string) error {
	return r.syncCrowdSec(ctx, expected, dashboardHost)
}

// SyncTrustedProxies exposes the trusted-proxies step of a pass, with the stored
// setting supplied by the test, so it can be exercised without a database.
func (r *Reconciler) SyncTrustedProxies(ctx context.Context, setting func(context.Context) (string, error)) error {
	r.trustedSetting = setting
	return r.syncTrustedProxies(ctx)
}
