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
	// AccessLog is state "unknown" when no tailer is wired.
	AccessLog logtailer.Status `json:"access_log"`
}

func (h *Handler) caddyPanel(r *http.Request) caddyPanel {
	h.caddyInspectorOnce.Do(func() {
		// Local is infallible for the single-server runtime and Caddy is the
		// control plane's own; if it ever errors the inspector reports unknown.
		var rt capability.Runtime
		if h.runtimes != nil {
			if lr, err := h.runtimes.Local(context.Background()); err == nil {
				rt = lr
			}
		}
		h.caddyInspector = caddyinfo.New(h.cfg, rt, h.capabilities)
	})

	adminState := ""
	if h.reconciler != nil {
		adminState = h.reconciler.Status().AdminState
	}
	report := h.caddyInspector.Inspect(r.Context(), adminState)

	// Rebuilt from the report's module list so this and /api/proxy/capabilities
	// share featureState, and an empty list stays "unknown", not "unavailable".
	set := capability.NewSet(report.Modules.IDs...)
	features := make(map[string]featureCapability)
	for _, t := range proxy.ModuleBackedFeatures() {
		module := proxy.RequiredModule(t)
		features[t] = featureCapability{Module: module, State: featureState(set, module)}
	}

	panel := caddyPanel{Report: report, Features: features, AccessLog: logtailer.Status{State: "unknown"}}
	if h.accessLog != nil {
		panel.AccessLog = h.accessLog.Status()
	}
	return panel
}
