package handler

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/belune-hq/belune/internal/service"
)

type wafBlockingApplication struct {
	ApplicationID   string `json:"application_id"`
	ApplicationName string `json:"application_name"`
	ProjectName     string `json:"project_name"`
	// Blocking is whether the application has opted in to WAF blocking.
	Blocking bool `json:"blocking"`
	// Hosts are the application's domains now: what turning blocking on affects.
	Hosts []string `json:"hosts"`
	// SharedHosts are hosts another application that has not opted in also serves.
	// The WAF sees the whole host, so these stay detection-only whatever this
	// application is set to.
	SharedHosts []string `json:"shared_hosts"`
}

type wafBlockingResponse struct {
	Applications []wafBlockingApplication `json:"applications"`
}

type setWAFBlockingRequest struct {
	Enabled *bool `json:"enabled"`
}

func wafBlockingBody(v service.WAFBlockingView) wafBlockingApplication {
	out := wafBlockingApplication{
		ApplicationID: v.ApplicationID, ApplicationName: v.ApplicationName, ProjectName: v.ProjectName,
		Blocking: v.Blocking, Hosts: v.Hosts, SharedHosts: v.SharedHosts,
	}
	if out.Hosts == nil {
		out.Hosts = []string{}
	}
	if out.SharedHosts == nil {
		out.SharedHosts = []string{}
	}
	return out
}

// ListWAFBlocking lists the applications the WAF can enforce on, and which have
// opted in.
// GET /api/maintenance/proxy/waf/blocking (admin only)
//
//apidoc:tag platform/maintenance
//apidoc:title List WAF Blocking
//apidoc:order 16
func (h *Handler) ListWAFBlocking(w http.ResponseWriter, r *http.Request) {
	list, err := h.wafBlocking.List(r.Context())
	if err != nil {
		writeServerError(w, r, "failed to list the applications WAF blocking applies to", err)
		return
	}
	out := wafBlockingResponse{Applications: make([]wafBlockingApplication, 0, len(list))}
	for _, v := range list {
		out.Applications = append(out.Applications, wafBlockingBody(v))
	}
	writeJSON(w, http.StatusOK, out)
}

// SetWAFBlocking turns blocking on or off for one application. On means requests
// to its hosts that the rule set scores over the threshold are refused with a
// 403; off returns them to detection-only within one reconcile pass. Turning it on
// is audited with the hosts it affects, as creating an exclusion is.
// PUT /api/maintenance/proxy/waf/blocking/{application_id} (admin only)
//
//apidoc:tag platform/maintenance
//apidoc:title Set WAF Blocking
//apidoc:order 17
func (h *Handler) SetWAFBlocking(w http.ResponseWriter, r *http.Request) {
	var appID pgtype.UUID
	if err := appID.Scan(chi.URLParam(r, "application_id")); err != nil {
		writeError(w, http.StatusBadRequest, "invalid application id")
		return
	}
	var req setWAFBlockingRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Enabled == nil {
		writeError(w, http.StatusBadRequest, "enabled (true or false) is required")
		return
	}

	was, now, err := h.wafBlocking.Set(r.Context(), appID, *req.Enabled)
	switch {
	case errors.Is(err, service.ErrWAFBlockingApplication):
		writeError(w, http.StatusNotFound, "that application does not exist or serves no domain, so there is nothing for the WAF to enforce on")
		return
	case err != nil:
		writeServerError(w, r, "failed to save the WAF mode", err)
		return
	}

	action := "disable_waf_blocking"
	if *req.Enabled {
		action = "enable_waf_blocking"
	}
	h.audit(r, action, "application", now.ApplicationID, map[string]any{
		"application_name": now.ApplicationName, "hosts": now.Hosts, "shared_hosts": now.SharedHosts,
		"was_blocking": was.Blocking,
	})
	h.reconcileAfterChange(r.Context(), "WAF blocking")
	writeJSON(w, http.StatusOK, wafBlockingBody(now))
}
