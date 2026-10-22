package handler

import (
	"context"
	"net/http"

	"github.com/belune-hq/belune/internal/logtailer"
	"github.com/belune-hq/belune/internal/proxy"
	"github.com/belune-hq/belune/internal/proxy/caddyinfo"
	"github.com/belune-hq/belune/internal/proxy/capability"
)

// AccessLogStatusProvider is the slice of the log tailer the panel reads.
type AccessLogStatusProvider interface {
	Status() logtailer.Status
}

// SetAccessLog wires the request-log tailer's status into the proxy panel.
func (h *Handler) SetAccessLog(p AccessLogStatusProvider) {
	h.accessLog = p
}

// proxyStatusResponse is the reconciler's status plus a description of Caddy
// itself. The reconciler fields stay where they were, so nothing existing
// changes shape.
type proxyStatusResponse struct {
	proxy.ReconcilerStatus
	Caddy caddyPanel `json:"caddy"`
}

type caddyPanel struct {
	caddyinfo.Report
	// Features is the per-feature view of Report.Modules, with the same
	// available / unavailable / unknown distinction as /api/proxy/capabilities.
	Features map[string]featureCapability `json:"features"`
	// OptionalModules is the same three-state view for modules no route feature
	// uses yet (CrowdSec, WAF, layer 4). The page decides what to show from it, so
	// the module IDs live here and not in the frontend.
	OptionalModules map[string]featureCapability `json:"optional_modules"`
	// AccessLog is state "unknown" when no tailer is wired.
	AccessLog logtailer.Status `json:"access_log"`
}

// caddyPanel takes the admin state from the caller's one reconciler snapshot: reading
// Status() again here lets the 3s probe land between the two reads, and a single
// response would then hold two opinions about the same Caddy.
func (h *Handler) caddyPanel(r *http.Request, adminState string) caddyPanel {
	h.caddyInspectorOnce.Do(func() {
		// ⚠️ Local cannot fail for the single-server runtime, so a failure is not
		// retried: Once memoises it and the panel would stay unknown until restart.
		// Revisit when Runtimes can be fallible (multi-server) — cache successes
		// only, as the capability prober does.
		var rt capability.Runtime
		if h.runtimes != nil {
			if lr, err := h.runtimes.Local(context.Background()); err == nil {
				rt = lr
			}
		}
		h.caddyInspector = caddyinfo.New(h.cfg, rt, h.capabilities)
	})

	report := h.caddyInspector.Inspect(r.Context(), adminState)

	// Rebuilt from the report's module list so this and /api/proxy/capabilities
	// share featureState, and an empty list stays "unknown", not "unavailable".
	set := capability.NewSet(report.Modules.IDs...)
	features := make(map[string]featureCapability)
	for _, t := range proxy.ModuleBackedFeatures() {
		module := proxy.RequiredModule(t)
		features[t] = featureCapability{Module: module, State: featureState(set, module)}
	}

	optional := map[string]featureCapability{
		"crowdsec": {Module: capability.ModuleCrowdSec, State: featureState(set, capability.ModuleCrowdSec)},
		"waf":      {Module: capability.ModuleWAF, State: featureState(set, capability.ModuleWAF)},
		"layer4":   {Module: capability.ModuleLayer4, State: featureState(set, capability.ModuleLayer4)},
	}

	panel := caddyPanel{Report: report, Features: features, OptionalModules: optional, AccessLog: logtailer.Status{State: "unknown"}}
	if h.accessLog != nil {
		panel.AccessLog = h.accessLog.Status()
	}
	return panel
}
