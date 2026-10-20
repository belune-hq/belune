package handler

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/belune-hq/belune/internal/proxy"
)

// GetProxyReconcilerStatus returns the most recent reconciliation pass state.
// Useful for operators tracking whether Caddy drifts from the DB-declared
// routes between deploys.
// GET /api/maintenance/proxy (admin only)
//
//apidoc:tag platform/maintenance
//apidoc:title Get Proxy Status
//apidoc:order 6
func (h *Handler) GetProxyReconcilerStatus(w http.ResponseWriter, r *http.Request) {
	if h.reconciler == nil {
		writeJSON(w, http.StatusOK, proxy.ReconcilerStatus{})
		return
	}
	writeJSON(w, http.StatusOK, h.reconciler.Status())
}

// ReconcileProxy triggers an on-demand reconciliation of Caddy's routes against
// the DB-declared state (fixing any drift) and returns the resulting status.
// POST /api/maintenance/proxy/reconcile (admin only)
//
//apidoc:tag platform/maintenance
//apidoc:title Reconcile Proxy
//apidoc:order 7
func (h *Handler) ReconcileProxy(w http.ResponseWriter, r *http.Request) {
	if h.reconciler == nil {
		writeError(w, http.StatusServiceUnavailable, "reconciler unavailable")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()

	if err := h.reconciler.ReconcileNow(ctx); err != nil {
		// A pass ran but hit route errors — surface it, still return the status.
		slog.Warn("manual proxy reconcile reported errors", "error", err)
	}
	h.audit(r, "reconcile_proxy", "proxy", "", nil)
	writeJSON(w, http.StatusOK, h.reconciler.Status())
}
