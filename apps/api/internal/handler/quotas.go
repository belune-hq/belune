package handler

import (
	"encoding/json"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/weiliang79/belune/internal/quota"
	"github.com/weiliang79/belune/internal/server/middleware"
	"github.com/weiliang79/belune/internal/store/generated"
)

// quotaView is the wire shape returned by the admin quota API. Limits may have
// nil fields (represented as JSON null) to indicate no cap on that dimension.
type quotaView struct {
	Scope   string         `json:"scope"`
	ScopeID string         `json:"scope_id"`
	Limits  quota.Limits   `json:"limits"`
	Usage   quota.Usage    `json:"usage"`
	Meta    map[string]any `json:"meta,omitempty"`
}

//apidoc:tag platform/quotas
//apidoc:title Get Quotas
//apidoc:order 1
func (h *Handler) ListQuotas(w http.ResponseWriter, r *http.Request) {
	if h.quotaSvc == nil {
		writeError(w, http.StatusServiceUnavailable, "quotas unavailable")
		return
	}
	// Narrowed to the pin in SQL — see the query. An unpinned caller passes
	// NULL and sees everything.
	pinnedIDs, err := pinnedProjectUUIDs(r.Context())
	if err != nil {
		writeServerError(w, r, "invalid pinned project", err)
		return
	}
	rows, err := h.queries.ListQuotas(r.Context(), pinnedIDs)
	if err != nil {
		writeServerError(w, r, "failed to list quotas", err)
		return
	}

	views := make([]quotaView, 0, len(rows))
	for _, row := range rows {
		v, err := h.buildQuotaView(r, row.Scope, row.ScopeID, quota.LimitsFromRow(row))
		if err != nil {
			writeServerError(w, r, "failed to compute quota usage", err)
			return
		}
		views = append(views, v)
	}
	writeJSON(w, http.StatusOK, views)
}

//apidoc:tag platform/quotas
//apidoc:title Get Quota
//apidoc:order 2
func (h *Handler) GetQuota(w http.ResponseWriter, r *http.Request) {
	if h.quotaSvc == nil {
		writeError(w, http.StatusServiceUnavailable, "quotas unavailable")
		return
	}
	scope := chi.URLParam(r, "scope")
	scopeID, ok := parseScopeID(w, chi.URLParam(r, "scopeId"))
	if !ok {
		return
	}
	if !isValidScope(scope) {
		writeError(w, http.StatusBadRequest, "scope must be 'user' or 'project'")
		return
	}
	if !h.quotaScopeAllowed(w, r, scope, scopeID) {
		return
	}
	limits, err := h.quotaSvc.LimitsFor(r.Context(), scope, scopeID)
	if err != nil {
		writeServerError(w, r, "failed to get quota", err)
		return
	}
	view, err := h.buildQuotaView(r, scope, scopeID, limits)
	if err != nil {
		writeServerError(w, r, "failed to compute quota usage", err)
		return
	}
	writeJSON(w, http.StatusOK, view)
}

type upsertQuotaRequest struct {
	MaxApplications *int32   `json:"max_applications"`
	MaxCPU          *float64 `json:"max_cpu"`
	MaxMemoryMB     *int64   `json:"max_memory_mb"`
}

//apidoc:tag platform/quotas
//apidoc:title Upsert Quota
//apidoc:order 3
func (h *Handler) UpsertQuota(w http.ResponseWriter, r *http.Request) {
	if h.quotaSvc == nil {
		writeError(w, http.StatusServiceUnavailable, "quotas unavailable")
		return
	}
	scope := chi.URLParam(r, "scope")
	scopeID, ok := parseScopeID(w, chi.URLParam(r, "scopeId"))
	if !ok {
		return
	}
	if !isValidScope(scope) {
		writeError(w, http.StatusBadRequest, "scope must be 'user' or 'project'")
		return
	}
	if !h.quotaScopeAllowed(w, r, scope, scopeID) {
		return
	}

	var req upsertQuotaRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	limits := quota.Limits{
		MaxApplications: req.MaxApplications,
		MaxCPU:          req.MaxCPU,
		MaxMemoryMB:     req.MaxMemoryMB,
	}
	apps, cpu, mem := quota.LimitsToRow(limits)

	// Capture the previous limits so the audit row records the diff. A
	// missing prior row means this upsert is creating the quota for the
	// first time; we record it as nil-valued in 'old'.
	oldLimits, _ := h.quotaSvc.LimitsFor(r.Context(), scope, scopeID)

	row, err := h.queries.UpsertQuota(r.Context(), generated.UpsertQuotaParams{
		Scope:           scope,
		ScopeID:         scopeID,
		MaxApplications: apps,
		MaxCpu:          cpu,
		MaxMemoryMb:     mem,
	})
	if err != nil {
		writeServerError(w, r, "failed to save quota", err)
		return
	}
	h.audit(r, "upsert_quota", "quota", uuidToString(row.ID), map[string]any{
		"scope":    scope,
		"scope_id": uuidToString(scopeID),
		"old":      auditLimits(oldLimits),
		"new":      auditLimits(limits),
	})

	view, err := h.buildQuotaView(r, scope, scopeID, quota.LimitsFromRow(row))
	if err != nil {
		writeServerError(w, r, "failed to compute quota usage", err)
		return
	}
	writeJSON(w, http.StatusOK, view)
}

//apidoc:tag platform/quotas
//apidoc:title Delete Quota
//apidoc:order 4
func (h *Handler) DeleteQuota(w http.ResponseWriter, r *http.Request) {
	if h.quotaSvc == nil {
		writeError(w, http.StatusServiceUnavailable, "quotas unavailable")
		return
	}
	scope := chi.URLParam(r, "scope")
	scopeID, ok := parseScopeID(w, chi.URLParam(r, "scopeId"))
	if !ok {
		return
	}
	if !isValidScope(scope) {
		writeError(w, http.StatusBadRequest, "scope must be 'user' or 'project'")
		return
	}
	if !h.quotaScopeAllowed(w, r, scope, scopeID) {
		return
	}
	// Capture limits before deletion so the audit row preserves what was
	// removed (the row itself is gone from the DB after this call).
	prior, _ := h.quotaSvc.LimitsFor(r.Context(), scope, scopeID)

	if err := h.queries.DeleteQuota(r.Context(), generated.DeleteQuotaParams{Scope: scope, ScopeID: scopeID}); err != nil {
		writeServerError(w, r, "failed to delete quota", err)
		return
	}
	h.audit(r, "delete_quota", "quota", uuidToString(scopeID), map[string]any{
		"scope": scope,
		"old":   auditLimits(prior),
	})
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

// auditLimits flattens quota.Limits into a JSON-friendly map. Nil pointers
// become nil values so the audit row clearly distinguishes "no cap" (null)
// from "cap of 0" (the actual integer).
func auditLimits(l quota.Limits) map[string]any {
	out := map[string]any{
		"max_applications": nil,
		"max_cpu":          nil,
		"max_memory_mb":    nil,
	}
	if l.MaxApplications != nil {
		out["max_applications"] = *l.MaxApplications
	}
	if l.MaxCPU != nil {
		out["max_cpu"] = *l.MaxCPU
	}
	if l.MaxMemoryMB != nil {
		out["max_memory_mb"] = *l.MaxMemoryMB
	}
	return out
}

func (h *Handler) buildQuotaView(r *http.Request, scope string, scopeID pgtype.UUID, limits quota.Limits) (quotaView, error) {
	var usage quota.Usage
	var err error
	meta := map[string]any{}
	switch scope {
	case quota.ScopeUser:
		usage, err = h.quotaSvc.UsageForUser(r.Context(), scopeID)
		if err == nil {
			if u, lookupErr := h.queries.GetUserByID(r.Context(), scopeID); lookupErr == nil {
				meta["email"] = u.Email
				meta["username"] = u.Username
			}
		}
	case quota.ScopeProject:
		usage, err = h.quotaSvc.UsageForProject(r.Context(), scopeID)
		if err == nil {
			if p, lookupErr := h.queries.GetProject(r.Context(), scopeID); lookupErr == nil {
				meta["name"] = p.Name
				meta["slug"] = p.Slug
			}
		}
	}
	if err != nil {
		return quotaView{}, err
	}
	return quotaView{
		Scope:   scope,
		ScopeID: uuidToString(scopeID),
		Limits:  limits,
		Usage:   usage,
		Meta:    meta,
	}, nil
}

func isValidScope(s string) bool { return s == quota.ScopeUser || s == quota.ScopeProject }

// quotaScopeAllowed reports whether the caller may address this quota scope,
// writing the refusal itself when not.
//
// ⚠️ These routes are admin-gated but the project arrives as {scopeId}, not
// {projectId}, so middleware.RequirePinnedProjectMatch never fires on them and
// nothing else narrowed them: a project-pinned token could read, and WRITE,
// any project's limits — capping another project at one application and
// blocking its deploys. Found in review, verified by doing it.
//
// A pinned token is refused the USER scope outright rather than compared to
// the pin. The pin is expressed in projects, so it has nothing to say about a
// user-scoped quota — and a user quota governs every project that user owns,
// so honouring it would reach past the pin by another route.
func (h *Handler) quotaScopeAllowed(w http.ResponseWriter, r *http.Request, scope string, scopeID pgtype.UUID) bool {
	if !middleware.TokenPinnedFromContext(r.Context()) {
		return true
	}
	if scope != quota.ScopeProject {
		writeError(w, http.StatusForbidden,
			"a project-pinned token cannot address user-scoped quotas")
		return false
	}
	if !middleware.PinAllows(r.Context(), uuidToString(scopeID)) {
		writeError(w, http.StatusForbidden, "token is pinned to a different project")
		return false
	}
	return true
}

func parseScopeID(w http.ResponseWriter, s string) (pgtype.UUID, bool) {
	var u pgtype.UUID
	if err := u.Scan(s); err != nil {
		writeError(w, http.StatusBadRequest, "invalid scope id")
		return pgtype.UUID{}, false
	}
	return u, true
}
