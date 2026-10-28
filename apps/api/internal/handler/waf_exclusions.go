package handler

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/belune-hq/belune/internal/proxy"
	"github.com/belune-hq/belune/internal/server/middleware"
	"github.com/belune-hq/belune/internal/service"
)

type wafExclusionResponse struct {
	ID     int64 `json:"id"`
	RuleID int   `json:"rule_id"`
	// Shape is "target" (one named request field), "path" (one path) or "rule"
	// (the whole rule).
	Shape       string `json:"shape"`
	Collection  string `json:"collection"`
	TargetName  string `json:"target_name"`
	PathPattern string `json:"path_pattern"`
	// Scope is "install" for every application, or "application".
	Scope           string  `json:"scope"`
	ApplicationID   *string `json:"application_id"`
	ApplicationName string  `json:"application_name"`
	// Hosts are the application's domains now; the exclusion is not stored with them.
	Hosts []string `json:"hosts"`
	// Inert: scoped to an application that has no domains, so it applies to nothing.
	Inert     bool      `json:"inert"`
	Note      string    `json:"note"`
	CreatedAt time.Time `json:"created_at"`
}

type wafExclusionsResponse struct {
	Exclusions []wafExclusionResponse `json:"exclusions"`
	// Max is the most Belune will render.
	Max int `json:"max"`
	// Collections are the request fields a target exclusion may name.
	Collections []string `json:"collections"`
}

type createWAFExclusionRequest struct {
	RuleID      int    `json:"rule_id"`
	Collection  string `json:"collection"`
	TargetName  string `json:"target_name"`
	PathPattern string `json:"path_pattern"`
	// ApplicationID scopes it to one application; empty or absent is install-wide.
	ApplicationID string `json:"application_id"`
	// Note is required when the whole rule is turned off.
	Note string `json:"note"`
}

func wafExclusionBody(v service.WAFExclusionView) wafExclusionResponse {
	out := wafExclusionResponse{
		ID: v.ID, RuleID: v.RuleID, Shape: v.Shape,
		Collection: v.Collection, TargetName: v.TargetName, PathPattern: v.PathPattern,
		Scope: "install", ApplicationName: v.ApplicationName,
		Hosts: v.Hosts, Inert: v.Inert, Note: v.Note, CreatedAt: v.CreatedAt,
	}
	if out.Hosts == nil {
		out.Hosts = []string{}
	}
	if v.ApplicationID != "" {
		id := v.ApplicationID
		out.Scope, out.ApplicationID = "application", &id
	}
	return out
}

// wafExclusionAudit is what an audit row says about an exclusion: enough to answer
// "who turned this off, where, and why" after the person who did is long gone.
func wafExclusionAudit(v service.WAFExclusionView) map[string]any {
	return map[string]any{
		"rule_id": v.RuleID, "shape": v.Shape,
		"collection": v.Collection, "target_name": v.TargetName, "path_pattern": v.PathPattern,
		"scope":          map[bool]string{true: "application", false: "install"}[v.ApplicationID != ""],
		"application_id": v.ApplicationID, "application_name": v.ApplicationName,
		"hosts": v.Hosts, "note": v.Note,
	}
}

// ListWAFExclusions returns the places the rule set has been told to stand down.
// GET /api/maintenance/proxy/waf/exclusions (admin only)
//
//apidoc:tag platform/maintenance
//apidoc:title List WAF Exclusions
//apidoc:order 13
func (h *Handler) ListWAFExclusions(w http.ResponseWriter, r *http.Request) {
	list, err := h.wafExclusions.List(r.Context())
	if err != nil {
		writeServerError(w, r, "failed to list the WAF exclusions", err)
		return
	}
	out := wafExclusionsResponse{
		Exclusions:  make([]wafExclusionResponse, 0, len(list)),
		Max:         proxy.MaxWAFExclusions,
		Collections: proxy.WAFExclusionCollections(),
	}
	for _, v := range list {
		out.Exclusions = append(out.Exclusions, wafExclusionBody(v))
	}
	writeJSON(w, http.StatusOK, out)
}

// CreateWAFExclusion tells the rule set to stop looking at something: a named field,
// a path, or a whole rule. It is a security downgrade, so it is audited with its
// reason, and the rules that evaluate the anomaly score are refused outright.
// POST /api/maintenance/proxy/waf/exclusions (admin only)
//
//apidoc:tag platform/maintenance
//apidoc:title Create WAF Exclusion
//apidoc:order 14
func (h *Handler) CreateWAFExclusion(w http.ResponseWriter, r *http.Request) {
	var req createWAFExclusionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	v, err := h.wafExclusions.Create(r.Context(), service.WAFExclusionInput{
		WAFExclusionSpec: proxy.WAFExclusionSpec{
			RuleID: req.RuleID, Collection: req.Collection,
			TargetName: req.TargetName, PathPattern: req.PathPattern,
		},
		ApplicationID: req.ApplicationID,
		Note:          req.Note,
		CreatedBy:     middleware.UserIDFromContext(r.Context()),
	})
	var invalid *service.WAFExclusionInvalid
	switch {
	case errors.As(err, &invalid):
		writeError(w, http.StatusBadRequest, invalid.Reason)
		return
	case errors.Is(err, service.ErrWAFExclusionApplication):
		writeError(w, http.StatusNotFound, err.Error())
		return
	case errors.Is(err, service.ErrWAFExclusionDuplicate), errors.Is(err, service.ErrWAFExclusionLimit):
		writeError(w, http.StatusConflict, err.Error())
		return
	case err != nil:
		writeServerError(w, r, "failed to save the WAF exclusion", err)
		return
	}

	h.audit(r, "create_waf_exclusion", "proxy", strconv.FormatInt(v.ID, 10), wafExclusionAudit(v))
	h.reconcileNow(r, "WAF exclusion")
	writeJSON(w, http.StatusCreated, wafExclusionBody(v))
}

// DeleteWAFExclusion puts a rule back in force. Reversing a downgrade is never gated.
// DELETE /api/maintenance/proxy/waf/exclusions/{id} (admin only)
//
//apidoc:tag platform/maintenance
//apidoc:title Delete WAF Exclusion
//apidoc:order 15
func (h *Handler) DeleteWAFExclusion(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil || id <= 0 {
		writeError(w, http.StatusBadRequest, "invalid exclusion id")
		return
	}
	was, err := h.wafExclusions.Delete(r.Context(), id)
	switch {
	case errors.Is(err, service.ErrWAFExclusionNotFound):
		writeError(w, http.StatusNotFound, err.Error())
		return
	case err != nil:
		writeServerError(w, r, "failed to remove the WAF exclusion", err)
		return
	}
	h.audit(r, "delete_waf_exclusion", "proxy", strconv.FormatInt(id, 10), wafExclusionAudit(was))
	h.reconcileNow(r, "WAF exclusion")
	w.WriteHeader(http.StatusNoContent)
}

// reconcileNow applies a saved change now rather than on the next 30-second pass,
// so the answer is the state the proxy is actually in. A failed pass is not a
// failed request: the change is saved and the next pass retries. Bounded below the
// route's 15-second ceiling.
func (h *Handler) reconcileNow(r *http.Request, what string) {
	if h.reconciler == nil {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	if err := h.reconciler.ReconcileNow(ctx); err != nil {
		slog.Warn("proxy reconcile after a change reported errors", "change", what, "error", err)
	}
}
