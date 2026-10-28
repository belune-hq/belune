package handler

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/belune-hq/belune/internal/proxy"
	"github.com/belune-hq/belune/internal/service"
)

type trustedProxyOverlapResponse struct {
	CIDR    string `json:"cidr"`
	Network string `json:"network"`
	Subnet  string `json:"subnet"`
}

type trustedProxyNetworkResponse struct {
	Name   string `json:"name"`
	Subnet string `json:"subnet"`
}

type trustedProxyPresetResponse struct {
	ID        string   `json:"id"`
	Name      string   `json:"name"`
	CIDRs     []string `json:"cidrs"`
	Source    string   `json:"source"`
	CheckedAt string   `json:"checked_at"`
}

type trustedProxiesResponse struct {
	// CIDRs are the proxies in front of Caddy whose word about the visitor is taken.
	CIDRs []string `json:"cidrs"`
	// Overlaps are trusted ranges that overlap a Docker network Caddy is attached
	// to, which makes every container there a proxy that can claim any address.
	Overlaps []trustedProxyOverlapResponse `json:"overlaps"`
	// Networks are the address ranges of the Docker networks Caddy is attached to:
	// what a trusted range must not cover. They let a form say whether a range being
	// typed would overlap one before it is saved.
	Networks []trustedProxyNetworkResponse `json:"networks"`
	// OverlapsChecked is false when the networks could not be listed.
	OverlapsChecked bool `json:"overlaps_checked"`
	// Presets are published ranges to adopt; none is ever applied by default.
	Presets []trustedProxyPresetResponse `json:"presets"`
}

type updateTrustedProxiesRequest struct {
	CIDRs []string `json:"cidrs"`
}

func trustedProxiesBody(tp service.TrustedProxies) trustedProxiesResponse {
	out := trustedProxiesResponse{
		CIDRs:           tp.CIDRs,
		Overlaps:        make([]trustedProxyOverlapResponse, 0, len(tp.Overlaps)),
		Networks:        make([]trustedProxyNetworkResponse, 0, len(tp.Networks)),
		OverlapsChecked: tp.OverlapsChecked,
		Presets:         []trustedProxyPresetResponse{},
	}
	for _, o := range tp.Overlaps {
		out.Overlaps = append(out.Overlaps, trustedProxyOverlapResponse(o))
	}
	for _, n := range tp.Networks {
		out.Networks = append(out.Networks, trustedProxyNetworkResponse(n))
	}
	for _, p := range proxy.TrustedProxyPresets() {
		out.Presets = append(out.Presets, trustedProxyPresetResponse(p))
	}
	return out
}

// GetTrustedProxies returns the proxies Caddy is told sit in front of it.
// GET /api/maintenance/proxy/trusted-proxies (admin only)
//
//apidoc:tag platform/maintenance
//apidoc:title Get Trusted Proxies
//apidoc:order 20
func (h *Handler) GetTrustedProxies(w http.ResponseWriter, r *http.Request) {
	tp, err := h.trustedProxies.Get(r.Context())
	if err != nil {
		writeServerError(w, r, "failed to read the trusted proxies", err)
		return
	}
	writeJSON(w, http.StatusOK, trustedProxiesBody(tp))
}

// UpdateTrustedProxies replaces the list of CDNs or load balancers in front of
// Caddy. With none listed (the default) the visitor is whoever connects to Caddy;
// with a CDN listed, Caddy reads the visitor from the X-Forwarded-For the CDN sent.
//
// ⚠️ A listed range may claim any client address. Never list a proxy you do not
// control. The response says when a range overlaps a Docker network Caddy is on,
// which would let a deployed container do exactly that.
// PUT /api/maintenance/proxy/trusted-proxies (admin only)
//
//apidoc:tag platform/maintenance
//apidoc:title Update Trusted Proxies
//apidoc:order 21
func (h *Handler) UpdateTrustedProxies(w http.ResponseWriter, r *http.Request) {
	var req updateTrustedProxiesRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.CIDRs == nil {
		writeError(w, http.StatusBadRequest, "cidrs (a list of IP addresses or CIDR ranges, possibly empty) is required")
		return
	}

	before, tp, err := h.trustedProxies.Set(r.Context(), req.CIDRs)
	var invalid *service.TrustedProxyValidationError
	switch {
	case errors.As(err, &invalid):
		writeError(w, http.StatusBadRequest, invalid.Reason)
		return
	case err != nil:
		writeServerError(w, r, "failed to save the trusted proxies", err)
		return
	}

	// A security downgrade is audited with what it replaced and with what is wrong
	// with it, so the row alone answers "who made a Docker network a proxy".
	h.audit(r, "update_trusted_proxies", "proxy", "", map[string]any{
		"before":   before,
		"after":    tp.CIDRs,
		"overlaps": tp.Overlaps,
	})

	h.reconcileAfterChange(r.Context(), "trusted proxies")
	writeJSON(w, http.StatusOK, trustedProxiesBody(tp))
}

// reconcileAfterChange applies a saved setting now rather than on the next
// 30-second pass. A failed pass is not a failed request: the setting is saved and
// the next pass retries. Bounded below the route's 15-second ceiling.
func (h *Handler) reconcileAfterChange(ctx context.Context, what string) {
	if h.reconciler == nil {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := h.reconciler.ReconcileNow(ctx); err != nil {
		slog.Warn("proxy reconcile after a settings change reported errors", "setting", what, "error", err)
	}
}
