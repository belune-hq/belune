package proxy

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5/pgtype"
	"sync"
	"time"

	"github.com/belune-hq/belune/internal/naming"
	"github.com/belune-hq/belune/internal/proxy/capability"
	"github.com/belune-hq/belune/internal/status"
	"github.com/belune-hq/belune/internal/store/generated"
)

// Reconciler periodically compares DB-declared routes against Caddy's live
// route table and reconciles any drift by adding missing routes and removing
// stale ones. It replaces the one-shot recoverProxyRoutes startup call.
type Reconciler struct {
	queries  *generated.Queries
	proxy    ProxyManager
	dec      Decryptor
	interval time.Duration

	// net re-joins the proxy container to each project's network. Optional: nil
	// when the proxy is not a container we manage (Caddy on the host), in which
	// case the attachment is somebody else's problem.
	net            NetworkAttacher
	proxyContainer string

	// caps reports which optional Caddy modules exist. Nil means "do not gate",
	// which is the behaviour before the probe was wired; once set, the
	// Provider itself fails closed.
	caps capability.Provider

	// runMu serialises whole reconcile passes so a manual ReconcileNow can't
	// overlap the periodic tick (reconcile is diff-based/idempotent but issues
	// concurrent Caddy add/remove calls otherwise).
	runMu sync.Mutex

	mu     sync.RWMutex
	status ReconcilerStatus

	// lastExpected is how many routes the latest pass wanted Caddy to hold, and
	// probe is what the last cheap probe saw. Both feed probeLostConfig.
	lastExpected int
	probe        caddyState

	// waf is what the last pass did about the WAF; see syncWAF.
	waf WAFState
	// wafSwitch replaces the database read of the operator's switch. Tests only.
	wafSwitch func(ctx context.Context) (bool, error)
	// wafExclusionList replaces the database read of the exclusions. Tests only.
	wafExclusionList func(ctx context.Context) ([]WAFExclusion, error)

	// crowdsecSrc supplies CrowdSec's wishes each pass; nil means the feature is
	// not wired and the step does nothing. crowdsec is what the last pass did.
	crowdsecSrc CrowdSecSource
	crowdsec    CrowdSecState
}

// caddyProbeInterval is how often Run checks whether Caddy still holds our
// routes. A restarted Caddy comes back with only its static Caddyfile, so every
// domain answers with the dashboard until a pass re-adds the routes; waiting for
// the 30s tick made that window up to 30s. A const on purpose: the probe is one
// admin GET, and nothing about it is worth an operator's attention.
const caddyProbeInterval = 3 * time.Second

// caddyState is what one probe concluded about Caddy.
type caddyState int

const (
	caddyUnknown     caddyState = iota // not probed yet
	caddyUnreachable                   // the admin API did not answer
	caddyHealthy                       // answered, and holds what we expect
	caddyLostConfig                    // answered, and holds none of our routes
)

// String is the wire name; the zero value reads "unknown", which is also what a
// nil reconciler reports, so "not asked yet" never renders as "healthy".
func (s caddyState) String() string {
	switch s {
	case caddyUnreachable:
		return "unreachable"
	case caddyHealthy:
		return "healthy"
	case caddyLostConfig:
		return "lost_config"
	}
	return "unknown"
}

// ReconcilerStatus is a snapshot of the reconciler's recent activity. It is
// exposed over the admin API so operators can tell at a glance whether the
// background loop is still healthy and whether Caddy has been drifting.
type ReconcilerStatus struct {
	IntervalSeconds int       `json:"interval_seconds"`
	LastRunAt       time.Time `json:"last_run_at"`
	LastDurationMs  int64     `json:"last_duration_ms"`
	LastAdded       int       `json:"last_added"`
	// LastUpdated counts routes that existed but did not match the database, and
	// had to be rewritten — drift a presence check would never have seen.
	LastUpdated int    `json:"last_updated"`
	LastRemoved int    `json:"last_removed"`
	LastError   string `json:"last_error,omitempty"`
	RunCount    int64  `json:"run_count"`
	// TotalDrift counts every route that had to be added or removed across
	// all runs. Stable value over time means Caddy is in sync with the DB.
	TotalDrift int64 `json:"total_drift"`
	// Degraded lists the features served WITHOUT because the running Caddy lacks
	// their module. Derived afresh every pass rather than stored, so it clears
	// itself the moment the module appears or the feature is removed.
	Degraded []Degradation `json:"degraded,omitempty"`
	// AdminState is what the 3s probe last concluded about Caddy's admin API:
	// unknown (not probed yet), unreachable, healthy or lost_config. It is the
	// state the reconciler ACTS on, exposed rather than re-derived, so the
	// dashboard cannot hold a second opinion that disagrees with it.
	AdminState string `json:"admin_state"`
}

// Degradation records one feature a route is being served without.
type Degradation struct {
	Hostname string `json:"hostname"`
	Path     string `json:"path"`
	Feature  string `json:"feature"`
	// Module is the Caddy module the feature needs and the running Caddy lacks.
	Module string `json:"module"`
	// Reason says how we came to withhold it, because the two cases need different
	// advice: ReasonModuleAbsent ("use the Belune Caddy image") versus
	// ReasonModulesUnknown ("Belune cannot reach the proxy to ask").
	Reason string `json:"reason"`
	// Since is when this exact degradation first appeared, carried across passes.
	// It exists so a consumer can tell a standing condition from a transient one
	// (Caddy is unprobeable during every update) by TIME rather than by counting
	// passes: two passes can land seconds apart during a Caddy restart.
	Since time.Time `json:"since"`
}

// Why a feature was withheld; see Degradation.Reason.
const (
	ReasonModuleAbsent   = "module_absent"
	ReasonModulesUnknown = "modules_unknown"
)

// carrySince returns next with each entry's Since taken from the identical
// hostname+path+feature in prev, or from now when it is new. The previous value
// is copied whole: slices.Equal in reconcile compares time.Time with ==, so a
// value rebuilt from the same instant but a different monotonic reading would
// look like a change and re-log a standing degradation every pass.
// degradationsChanged reports whether this pass's degradation set differs from
// the last one's, which is what keeps a standing condition out of the log on
// every tick. Pure, like nextCaddyState and probeShouldReconcile, because it is
// paired with carrySince and only the pair is the real behaviour: the
// comparison is by value, so a Since that was re-stamped rather than carried
// over reads as a change and re-logs for ever. A reconcile-level test can show
// that too, but it needs a database; this can be checked anywhere.
func degradationsChanged(prev, next []Degradation) bool {
	return !slices.Equal(prev, next)
}

func carrySince(prev, next []Degradation, now time.Time) []Degradation {
	for i := range next {
		next[i].Since = now
		for _, p := range prev {
			if p.Hostname == next[i].Hostname && p.Path == next[i].Path && p.Feature == next[i].Feature {
				next[i].Since = p.Since
				break
			}
		}
	}
	return next
}

// NewReconciler creates a Reconciler with the given tick interval. dec unwraps
// the stored certificate PEM for domains serving an uploaded certificate.
func NewReconciler(queries *generated.Queries, proxy ProxyManager, dec Decryptor, interval time.Duration) *Reconciler {
	return &Reconciler{
		queries:  queries,
		proxy:    proxy,
		dec:      dec,
		interval: interval,
		status:   ReconcilerStatus{IntervalSeconds: int(interval / time.Second)},
	}
}

// WithNetworkAttacher tells the reconciler to keep the proxy container joined to
// every project network it needs, naming the proxy container to attach.
func (r *Reconciler) WithNetworkAttacher(net NetworkAttacher, proxyContainer string) *Reconciler {
	r.net = net
	r.proxyContainer = proxyContainer
	return r
}

// WithCapabilities makes the reconciler serve a route without any feature whose
// Caddy module is absent, instead of pushing a config Caddy will reject.
func (r *Reconciler) WithCapabilities(caps capability.Provider) *Reconciler {
	r.caps = caps
	return r
}

// Status returns a snapshot of the reconciler's latest run state.
func (r *Reconciler) Status() ReconcilerStatus {
	r.mu.RLock()
	defer r.mu.RUnlock()
	st := r.status
	st.AdminState = r.probe.String()
	return st
}

// ReconcileNow runs a reconciliation pass on demand (the admin "reconcile
// routes" action) and returns the pass's first error, if any. It shares the run
// mutex with the periodic loop, so a manual trigger cannot overlap a tick.
func (r *Reconciler) ReconcileNow(ctx context.Context) error {
	r.reconcile(ctx)
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.status.LastError != "" {
		return errors.New(r.status.LastError)
	}
	return nil
}

// Run starts the reconcile loop. It reconciles immediately on entry, then on
// every tick. Returns when ctx is cancelled.
func (r *Reconciler) Run(ctx context.Context) {
	r.reconcile(ctx)

	ticker := time.NewTicker(r.interval)
	defer ticker.Stop()
	probe := time.NewTicker(caddyProbeInterval)
	defer probe.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			r.reconcile(ctx)
		case <-probe.C:
			r.probeLostConfig(ctx)
		}
	}
}

// probeLostConfig notices a Caddy that restarted and reconciles at once instead
// of waiting for the tick.
//
// ListRoutes returns only host-matched routes, so neither the catch-all nor the
// Caddyfile's default site count: a restarted Caddy reports zero on an install
// that has domains. It fires on a TRANSITION into that state, never on the state
// itself — if a pass cannot restore the routes the condition stays true, and a
// level trigger would then reconcile every few seconds for ever. A still-broken
// install falls back to the ordinary tick. It also fires when Caddy becomes
// reachable again, which covers an API that booted before Caddy was answering
// (InitCatchAll warns once and does not retry).
//
// Caddy can live outside Docker, so unreachable is a contemplated topology and
// is logged once per transition, not once per probe.
func (r *Reconciler) probeLostConfig(ctx context.Context) {
	current, err := r.proxy.ListRoutes(ctx)

	r.mu.Lock()
	next := nextCaddyState(current, err, r.lastExpected)
	prev := r.probe
	r.probe = next
	fire := probeShouldReconcile(prev, next, r.status.LastError != "")
	r.mu.Unlock()

	switch {
	case next == caddyUnreachable && prev != caddyUnreachable:
		slog.Warn("proxy reconciler: caddy admin API is not answering", "error", err)
	case fire && next == caddyLostConfig:
		slog.Warn("proxy reconciler: caddy has lost its routes (restarted?); reconciling now")
	case fire:
		slog.Info("proxy reconciler: caddy is reachable again; reconciling now")
	}
	if fire {
		r.reconcile(ctx)
	}
}

// nextCaddyState classifies one ListRoutes answer. Pure, so the decision that
// is most likely to regress can be tested without a database or a Caddy.
func nextCaddyState(routes []RouteConfig, err error, lastExpected int) caddyState {
	switch {
	case err != nil:
		return caddyUnreachable
	case len(routes) == 0 && lastExpected > 0:
		return caddyLostConfig
	}
	return caddyHealthy
}

// probeShouldReconcile is the edge trigger: entering lost-config, or leaving
// unreachable. Never the state itself. Unknown -> healthy only counts when the
// opening pass failed; otherwise the first probe would reconcile a Caddy that
// was never in trouble.
func probeShouldReconcile(prev, next caddyState, lastPassErrored bool) bool {
	switch next {
	case caddyLostConfig:
		return prev != caddyLostConfig
	case caddyHealthy:
		return prev == caddyUnreachable || (prev == caddyUnknown && lastPassErrored)
	}
	return false
}

// reconcile performs one reconciliation pass. Errors are logged and retried on
// the next tick — reconciliation is best-effort and must not block startup.
// The outcome (added/removed counts, first error) is recorded on the
// Reconciler so it can be surfaced via Status().
func (r *Reconciler) reconcile(ctx context.Context) {
	r.runMu.Lock()
	defer r.runMu.Unlock()

	start := time.Now()
	var added, removed, updated int
	var degraded []Degradation
	var firstErr error
	recordErr := func(err error) {
		if firstErr == nil {
			firstErr = err
		}
	}

	defer func() {
		r.mu.Lock()
		r.status.LastRunAt = start
		r.status.LastDurationMs = time.Since(start).Milliseconds()
		r.status.LastAdded = added
		r.status.LastUpdated = updated
		r.status.LastRemoved = removed
		r.status.RunCount++
		r.status.TotalDrift += int64(added + removed + updated)
		// Only on change: this runs every pass, and a standing condition would
		// otherwise repeat itself in the log every 30 seconds for ever.
		if degradationsChanged(r.status.Degraded, degraded) {
			for _, d := range degraded {
				slog.Warn("proxy reconciler: serving route without a feature its Caddy lacks the module for",
					"hostname", d.Hostname, "path", d.Path, "feature", d.Feature, "module", d.Module)
			}
		}
		r.status.Degraded = degraded
		if firstErr != nil {
			r.status.LastError = firstErr.Error()
		} else {
			r.status.LastError = ""
		}
		r.mu.Unlock()
	}()

	expected, networks, err := r.buildExpected(ctx)
	if err != nil {
		slog.Warn("proxy reconciler: failed to build expected routes", "error", err)
		recordErr(err)
		return
	}

	r.mu.Lock()
	r.lastExpected = len(expected)
	r.mu.Unlock()

	// Degrade the feature, never drop the route. Skip and custom-cert sets below
	// are computed from `expected`, so a route that stays in it stays in them —
	// which is what keeps "a hostname is in Caddy's route table iff it is in the
	// skip computation" true. Removing a route here would have to remove it from
	// both, or an ssl_mode=off host would start attracting certificate attempts.
	expected, degraded = r.degrade(ctx, expected)

	// DNS-01 policies come from the same pass as the routes, for the same reason:
	// a policy for a name with no route would attract issuance nothing serves.
	// A domain that cannot have its policy (module absent, no provider) is a
	// Degradation, and its route stays.
	dnsPlan, dnsDegraded, dnsErr := PlanDNSPolicies(ctx, r.queries, r.dec, r.caps)
	if dnsErr != nil {
		slog.Warn("proxy reconciler: dns challenge planning was incomplete", "error", dnsErr)
		recordErr(dnsErr)
	}
	if len(dnsDegraded) > 0 {
		degraded = append(degraded, dnsDegraded...)
		sortDegradations(degraded)
	}
	r.mu.RLock()
	degraded = carrySince(r.status.Degraded, degraded, start)
	r.mu.RUnlock()

	// Before the routes: a route pointing at a container the proxy cannot resolve
	// is a 502, and a recreated proxy container has lost every project network it
	// ever joined.
	r.ensureNetworks(ctx, networks)

	// Re-assert the server-level config (HTTPS listener, auto-HTTPS policy, skip
	// list) before touching routes. A restarted Caddy comes back up with only its
	// static Caddyfile, so this is what restores the :443 listener — and doing it
	// first means a route re-added below cannot briefly trigger a doomed
	// certificate attempt for an ssl_mode=off hostname.
	// The dashboard has no domains row, so its TLS choice has to be folded into
	// these lists by hand. Miss it out and the mode is decorative: an off
	// dashboard would still have a certificate obtained for it, and a custom one
	// would be served Caddy's internal cert in preference to the uploaded one.
	dashboardHost, dashboardMode, dashboardCertID := r.dashboardTLS(ctx)

	// Deduplicated by hostname. These lists are about certificates, and a
	// certificate belongs to a name — Caddy selects it by SNI, which knows nothing
	// about paths. A host split across three paths appears three times in
	// `expected`, so listing it per row would hand Caddy the same hostname three
	// times over in its automation policies.
	//
	// A host whose rows disagree about ssl_mode is a contradiction — one
	// certificate cannot be both absent and uploaded — so say so rather than
	// letting whichever row was iterated last quietly decide.
	skip := newHostSet()
	skipCerts := newHostSet()
	modeByHost := make(map[string]string, len(expected))
	for _, cfg := range expected {
		if prev, seen := modeByHost[cfg.Hostname]; seen && prev != cfg.SSLMode {
			slog.Warn("proxy reconciler: conflicting ssl_mode for one hostname; using the first",
				"hostname", cfg.Hostname, "using", prev, "ignoring", cfg.SSLMode)
			continue
		}
		modeByHost[cfg.Hostname] = cfg.SSLMode

		switch cfg.SSLMode {
		case SSLModeOff:
			skip.add(cfg.Hostname)
		case SSLModeCustom:
			skipCerts.add(cfg.Hostname)
		}
	}
	if dashboardHost != "" {
		switch dashboardMode {
		case SSLModeOff:
			skip.add(dashboardHost)
		case SSLModeCustom:
			skipCerts.add(dashboardHost)
		}
	}
	// Before the routes, like the skip lists: a route that lands first starts an
	// HTTP-01 attempt for a name that can only be validated over DNS. An empty
	// plan from a failed read is NOT pushed, or one bad query would strip every
	// policy and send every DNS-01 domain back to HTTP-01.
	if dnsErr == nil || len(dnsPlan) > 0 {
		if err := r.proxy.SyncDNSPolicies(ctx, dnsPlan); err != nil {
			slog.Warn("proxy reconciler: failed to sync dns challenge policies", "error", err)
			recordErr(err)
		}
	}

	if err := r.proxy.SyncAutoHTTPS(ctx, skip.sorted(), skipCerts.sorted()); err != nil {
		slog.Warn("proxy reconciler: failed to sync auto-HTTPS config", "error", err)
		recordErr(err)
	}

	// Uploaded certificates are held in Caddy's memory, so a restart drops them
	// just as it drops routes. Re-assert the whole set from the DB each pass.
	if err := r.syncCertificates(ctx, dashboardHost, dashboardMode, dashboardCertID); err != nil {
		slog.Warn("proxy reconciler: failed to sync certificates", "error", err)
		recordErr(err)
	}

	// The dashboard's own route, likewise. It is not backed by a domains row, so
	// it is both re-asserted here and exempted from the stale-route sweep below —
	// otherwise the sweep would see a host-matched route with no matching domain
	// and delete the dashboard within one interval.
	if err := r.proxy.SetDashboardRoute(ctx, dashboardHost, dashboardMode); err != nil {
		slog.Warn("proxy reconciler: failed to sync dashboard route", "error", err)
		recordErr(err)
	}

	current, err := r.proxy.ListRoutes(ctx)
	if err != nil {
		slog.Warn("proxy reconciler: failed to list current routes", "error", err)
		recordErr(err)
		return
	}

	// Index current routes by host *and* path. Keying on hostname alone would
	// collapse shop.com/ and shop.com/api into one entry, so the reconciler would
	// see a single route where there are two: it would treat whichever it kept as
	// covering both, and delete the other as stale on the next pass.
	currentSet := make(map[routeKey]struct{}, len(current))
	for _, c := range current {
		currentSet[keyFor(c)] = struct{}{}
	}

	// Index expected routes by host + path, for the same reason.
	expectedSet := make(map[routeKey]RouteConfig, len(expected))
	for _, e := range expected {
		expectedSet[keyFor(e)] = e
	}

	// Make every expected route match the database — not merely exist.
	//
	// This used to add only the routes Caddy was missing, which quietly assumed a
	// route with the right hostname had the right configuration. It need not: a
	// domain update writes the database and then pushes the route, and if that push
	// fails the handler returns 200 anyway and leaves the old route behind. The
	// hostname is still present, so a presence check finds nothing wrong and the
	// domain serves stale config for ever.
	for key, cfg := range expectedSet {
		_, existed := currentSet[key]
		changed, err := r.proxy.EnsureRoute(ctx, cfg)
		switch {
		case err != nil:
			slog.Warn("proxy reconciler: failed to ensure route", "hostname", key.hostname, "path", key.path, "error", err)
			recordErr(err)
		case changed && !existed:
			slog.Info("proxy reconciler: restored missing route", "hostname", key.hostname, "path", key.path)
			added++
		case changed:
			slog.Info("proxy reconciler: repaired drifted route", "hostname", key.hostname, "path", key.path)
			updated++
		}
	}

	// Remove routes in Caddy that no longer exist in DB.
	for key := range currentSet {
		if key.hostname == dashboardHost && dashboardHost != "" {
			continue // Belune's own dashboard: no domains row backs it.
		}
		if _, exists := expectedSet[key]; !exists {
			if err := r.proxy.RemoveRoute(ctx, key.hostname, key.path); err != nil {
				slog.Warn("proxy reconciler: failed to remove stale route", "hostname", key.hostname, "path", key.path, "error", err)
				recordErr(err)
			} else {
				slog.Info("proxy reconciler: removed stale route", "hostname", key.hostname, "path", key.path)
				removed++
			}
		}
	}

	// Last, so the host list the WAF route carries is the one the application
	// routes above now have.
	if err := r.syncWAF(ctx, expected, dashboardHost); err != nil {
		slog.Warn("proxy reconciler: failed to sync the WAF route", "error", err)
		recordErr(err)
	}
	// After the WAF, so a pass that creates both writes this one second and it
	// ends up in front: a banned address should be turned away before the WAF has
	// spent anything looking at it.
	if err := r.syncCrowdSec(ctx, expected, dashboardHost); err != nil {
		slog.Warn("proxy reconciler: failed to sync the CrowdSec route", "error", err)
		recordErr(err)
	}

	if added > 0 || removed > 0 {
		slog.Info("proxy reconciler: reconciliation complete", "added", added, "removed", removed)
	}
}

// buildExpected returns the full set of RouteConfigs that should exist in
// Caddy, derived from all running applications and their domains in the DB.
// buildExpected returns the routes the proxy should be serving, and the project
// networks the proxy must be joined to in order to reach them.
// hostSet is a deduplicated, deterministically ordered set of hostnames.
//
// Deterministic ordering is not cosmetic: these lists are written into Caddy's
// config, and the reconciler compares what it wrote against what is there. A set
// that came out in a different order each pass would look like drift for ever and
// rewrite the server config every minute.
type hostSet map[string]struct{}

func newHostSet() hostSet { return make(hostSet) }

func (s hostSet) add(host string) { s[host] = struct{}{} }

func (s hostSet) sorted() []string {
	if len(s) == 0 {
		return nil
	}
	out := make([]string, 0, len(s))
	for h := range s {
		out = append(out, h)
	}
	sort.Strings(out)
	return out
}

// routeKey identifies a route the way Caddy does now: a hostname no longer names
// one route, only a (hostname, path) pair does.
type routeKey struct {
	hostname string
	path     string
}

// keyFor normalises the empty path to "/", so a config that predates paths and
// one that explicitly says "root" are the same key rather than two.
func keyFor(cfg RouteConfig) routeKey {
	path := cfg.Path
	if path == "" {
		path = "/"
	}
	return routeKey{hostname: cfg.Hostname, path: path}
}

func (r *Reconciler) buildExpected(ctx context.Context) ([]RouteConfig, []string, error) {
	apps, err := r.queries.ListAllApplications(ctx)
	if err != nil {
		return nil, nil, err
	}

	var configs []RouteConfig
	networks := make(map[string]struct{})
	for _, app := range apps {
		if app.Status != status.ApplicationRunning {
			continue
		}

		row, err := r.queries.GetApplicationWithProjectSlug(ctx, app.ID)
		if err != nil {
			slog.Warn("proxy reconciler: failed to get application slug", "error", err)
			continue
		}

		// naming.ContainerName returns appSlug directly (the DB slug is the full container name).
		containerName := naming.ContainerName(row.ProjectSlug, row.Slug, "")

		domains, err := r.queries.ListDomainsByApplication(ctx, app.ID)
		if err != nil {
			slog.Warn("proxy reconciler: failed to list domains", "error", err)
			continue
		}

		for _, domain := range domains {
			cfg, err := BuildRouteConfigFromDB(ctx, r.queries, r.dec, domain, containerName, app.HealthCheckPath.String)
			if err != nil {
				slog.Warn("proxy reconciler: failed to build route config", "hostname", domain.Hostname, "error", err)
				continue
			}
			configs = append(configs, cfg)
			// Only apps that actually have a domain need the proxy on their network.
			networks[naming.ProjectNetworkName(row.ProjectSlug)] = struct{}{}
		}
	}

	names := make([]string, 0, len(networks))
	for n := range networks {
		names = append(names, n)
	}
	return configs, names, nil
}

// degrade removes, from the configs it returns, each enabled feature whose Caddy
// module the running Caddy lacks, and reports what it removed.
//
// Left in, such a feature makes Caddy reject the route's POST (measured on stock
// caddy:2-alpine: HTTP 500, "unknown module: http.handlers.rate_limit"). That
// leaves the config untouched, so other routes are unaffected — but the domain
// itself never gets a route and errors every pass for ever. Worse, AddRoute
// removes any existing route first, so a domain that was serving loses its route
// when an update names a module Caddy lacks. Serving without the feature, and
// saying so, beats both.
//
// The database is not touched: the feature stays configured, and starts working
// again when the module appears.
func (r *Reconciler) degrade(ctx context.Context, expected []RouteConfig) ([]RouteConfig, []Degradation) {
	if r.caps == nil {
		return expected, nil
	}

	// Probe only when something needs it: an exec every pass would be wasted on
	// installs using no module-backed feature.
	needsProbe := false
	for _, cfg := range expected {
		for _, f := range cfg.Features {
			if f.Enabled && RequiredModule(f.Type) != "" {
				needsProbe = true
			}
		}
	}
	if !needsProbe {
		return expected, nil
	}
	caps := r.caps.Capabilities(ctx)
	reason := ReasonModuleAbsent
	if !caps.Known() {
		reason = ReasonModulesUnknown
	}

	var degraded []Degradation
	out := make([]RouteConfig, len(expected))
	copy(out, expected)
	for i, cfg := range out {
		// Copy-on-write: Features may share a backing array with the caller.
		var kept []RouteFeature
		changed := false
		for j, f := range cfg.Features {
			module := RequiredModule(f.Type)
			if f.Enabled && module != "" && !caps.Has(module) {
				if !changed {
					kept = append(kept, cfg.Features[:j]...)
					changed = true
				}
				degraded = append(degraded, Degradation{
					Hostname: cfg.Hostname,
					Path:     keyFor(cfg).path,
					Feature:  f.Type,
					Module:   module,
					Reason:   reason,
				})
				continue
			}
			if changed {
				kept = append(kept, f)
			}
		}
		if changed {
			out[i].Features = kept
		}
	}

	sortDegradations(degraded)
	return out, degraded
}

// sortDegradations orders by hostname, path, feature, so the set compares equal
// pass to pass — degradationsChanged is a value comparison.
func sortDegradations(d []Degradation) {
	sort.Slice(d, func(a, b int) bool {
		x, y := d[a], d[b]
		if x.Hostname != y.Hostname {
			return x.Hostname < y.Hostname
		}
		if x.Path != y.Path {
			return x.Path < y.Path
		}
		return x.Feature < y.Feature
	})
}

// ensureNetworks re-joins the proxy container to every project network it needs.
//
// The deploy worker does this when an app is deployed, and that was the only
// place it happened — so a proxy container that got *recreated* (any compose up,
// an upgrade) came back with none of them, and every app domain answered 502
// until each app was redeployed. Docker treats an existing attachment as a
// no-op, so this is cheap to re-assert on every pass.
func (r *Reconciler) ensureNetworks(ctx context.Context, networks []string) {
	if r.net == nil || r.proxyContainer == "" {
		return
	}
	for _, network := range networks {
		if err := r.net.ConnectContainerToNetwork(ctx, r.proxyContainer, network); err != nil {
			// The proxy may legitimately live outside Docker, so this is a warning,
			// not a failed pass.
			slog.Warn("proxy reconciler: failed to attach proxy to project network",
				"proxy", r.proxyContainer, "network", network, "error", err)
		}
	}
}

// syncCertificates pushes the PEM pair of every ssl_mode=custom domain into the
// proxy's in-band certificate store — plus the dashboard's own, which has no
// domains row and so would otherwise be left with nothing to serve.
func (r *Reconciler) syncCertificates(ctx context.Context, dashboardHost, dashboardMode, dashboardCertID string) error {
	rows, err := r.queries.ListCustomCertDomains(ctx)
	if err != nil {
		return err
	}

	certs := make([]HostCertificate, 0, len(rows)+1)
	for _, row := range rows {
		certPEM, err := r.dec.Decrypt(row.CertPemEncrypted)
		if err != nil {
			slog.Warn("proxy reconciler: failed to decrypt certificate", "hostname", row.Hostname, "error", err)
			continue
		}
		keyPEM, err := r.dec.Decrypt(row.KeyPemEncrypted)
		if err != nil {
			slog.Warn("proxy reconciler: failed to decrypt certificate key", "hostname", row.Hostname, "error", err)
			continue
		}
		certs = append(certs, HostCertificate{
			Hostname: row.Hostname,
			CertPEM:  string(certPEM),
			KeyPEM:   string(keyPEM),
		})
	}

	if dashboardHost != "" && dashboardMode == SSLModeCustom && dashboardCertID != "" {
		if cert, ok := r.dashboardCertificate(ctx, dashboardHost, dashboardCertID); ok {
			certs = append(certs, cert)
		}
	}

	return r.proxy.SyncCertificates(ctx, certs)
}

// dashboardCertificate loads and decrypts the certificate the dashboard is
// configured to serve. A missing or undecryptable certificate is a warning, not
// a failure: the rest of the certificates must still reach Caddy.
func (r *Reconciler) dashboardCertificate(ctx context.Context, hostname, certID string) (HostCertificate, bool) {
	var id pgtype.UUID
	if err := id.Scan(certID); err != nil {
		slog.Warn("proxy reconciler: dashboard certificate id is not a uuid", "id", certID)
		return HostCertificate{}, false
	}
	row, err := r.queries.GetCertificate(ctx, id)
	if err != nil {
		slog.Warn("proxy reconciler: dashboard certificate not found", "id", certID, "error", err)
		return HostCertificate{}, false
	}
	certPEM, err := r.dec.Decrypt(row.CertPemEncrypted)
	if err != nil {
		slog.Warn("proxy reconciler: failed to decrypt dashboard certificate", "error", err)
		return HostCertificate{}, false
	}
	keyPEM, err := r.dec.Decrypt(row.KeyPemEncrypted)
	if err != nil {
		slog.Warn("proxy reconciler: failed to decrypt dashboard certificate key", "error", err)
		return HostCertificate{}, false
	}
	return HostCertificate{
		Hostname: hostname,
		CertPEM:  string(certPEM),
		KeyPEM:   string(keyPEM),
	}, true
}

// dashboardTLS reads how the operator wants the dashboard served. An unset or
// unreadable setting means "no dashboard route" rather than an error: the
// catch-all still serves the dashboard on any host over plain HTTP.
//
// An absent mode is automatic, which is what every install did before the mode
// existed — so an upgrade keeps the certificate it already has.
func (r *Reconciler) dashboardTLS(ctx context.Context) (hostname, sslMode, certID string) {
	setting, err := r.queries.GetSetting(ctx, SettingDashboardDomain)
	if err != nil {
		return "", "", ""
	}
	hostname = strings.TrimSpace(setting.Value)
	if hostname == "" {
		return "", "", ""
	}

	sslMode = SSLModeAutomatic
	if s, err := r.queries.GetSetting(ctx, SettingDashboardSSLMode); err == nil {
		if m := strings.TrimSpace(s.Value); ValidSSLMode(m) && m != "" {
			sslMode = m
		}
	}
	if s, err := r.queries.GetSetting(ctx, SettingDashboardCertificateID); err == nil {
		certID = strings.TrimSpace(s.Value)
	}
	return hostname, sslMode, certID
}
