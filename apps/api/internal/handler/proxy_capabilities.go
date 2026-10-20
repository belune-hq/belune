package handler

import (
	"net/http"

	"github.com/belune-hq/belune/internal/proxy"
	"github.com/belune-hq/belune/internal/proxy/capability"
)

// SetCapabilities wires the Caddy capability probe. It is the SAME instance the
// reconciler holds: two probers would mean two caches and two execs, and they
// could disagree about the same Caddy.
func (h *Handler) SetCapabilities(p capability.Provider) {
	h.capabilities = p
}

// capabilitySet is the probe's current answer, or an empty (not-Known) Set when
// no provider is wired.
func (h *Handler) capabilitySet(r *http.Request) capability.Set {
	if h.capabilities == nil {
		return capability.Set{}
	}
	return h.capabilities.Capabilities(r.Context())
}

// Feature availability states. "unknown" is a real state, not a fallback: when
// the probe could not ask Caddy (mid-restart, no exec access) the honest answer
// is that we do not know, and callers must not claim the module is missing.
const (
	featureAvailable   = "available"
	featureUnavailable = "unavailable"
	featureUnknown     = "unknown"
)

type featureCapability struct {
	// Module is the Caddy module the feature needs.
	Module string `json:"module"`
	// State is one of available, unavailable, unknown.
	State string `json:"state"`
}

type proxyCapabilitiesResponse struct {
	// Known is false when the probe could not read the running Caddy's modules.
	Known bool `json:"known"`
	// Features covers only the feature types that need an optional module; every
	// other type works on any Caddy and is absent here.
	Features map[string]featureCapability `json:"features"`
}

func featureState(set capability.Set, module string) string {
	switch {
	case !set.Known():
		return featureUnknown
	case set.Has(module):
		return featureAvailable
	default:
		return featureUnavailable
	}
}

// GetProxyCapabilities reports which optional route features the running Caddy
// can serve. It describes the install's Caddy, so it is not scoped to a project.
// GET /api/proxy/capabilities
//
//apidoc:tag applications/domains
//apidoc:title Get Proxy Capabilities
//apidoc:order 20
func (h *Handler) GetProxyCapabilities(w http.ResponseWriter, r *http.Request) {
	set := h.capabilitySet(r)

	features := make(map[string]featureCapability)
	for _, t := range proxy.ModuleBackedFeatures() {
		module := proxy.RequiredModule(t)
		features[t] = featureCapability{Module: module, State: featureState(set, module)}
	}
	writeJSON(w, http.StatusOK, proxyCapabilitiesResponse{Known: set.Known(), Features: features})
}
