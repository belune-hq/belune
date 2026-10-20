package handler

import (
	"encoding/json"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/belune-hq/belune/internal/store/generated"
)

type setResourcesRequest struct {
	// CPU cores; 0 = unlimited (e.g. 0.5 = half a core).
	CPULimit float64 `json:"cpu_limit"`
	// Memory in bytes; 0 = unlimited.
	MemoryLimit int64 `json:"memory_limit"`
}

// SetResources updates an application's CPU and memory limits. Its own endpoint
// rather than part of the general update: the limits are edited from a separate
// Resources card, and a card that reused the general PUT would have to echo
// every source field back correctly or the update would reject the mismatch.
//
// Limits are applied when the container is next created, so this stamps the
// config-changed marker and the badge points at Reload.
//
//apidoc:tag applications/resources
//apidoc:title Set Resources
//apidoc:order 1
func (h *Handler) SetResources(w http.ResponseWriter, r *http.Request) {
	applicationID := chi.URLParam(r, "applicationId")
	var applicationUUID pgtype.UUID
	if err := applicationUUID.Scan(applicationID); err != nil {
		writeError(w, http.StatusBadRequest, "invalid application id")
		return
	}
	if !h.canAccessApplication(r, applicationUUID) {
		writeError(w, http.StatusForbidden, "access denied")
		return
	}

	var req setResourcesRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.CPULimit < 0 || req.MemoryLimit < 0 {
		writeError(w, http.StatusBadRequest, "limits cannot be negative")
		return
	}

	app, err := h.queries.SetApplicationResources(r.Context(), generated.SetApplicationResourcesParams{
		ID:          applicationUUID,
		CpuLimit:    req.CPULimit,
		MemoryLimit: req.MemoryLimit,
	})
	if err != nil {
		writeServerError(w, r, "failed to update resources", err)
		return
	}

	h.markConfigChanged(r.Context(), applicationUUID)
	h.audit(r, "set_resources", "application", applicationID, map[string]any{
		"cpu_limit": req.CPULimit, "memory_limit": req.MemoryLimit,
	})

	writeJSON(w, http.StatusOK, toApplicationResponse(app))
}
