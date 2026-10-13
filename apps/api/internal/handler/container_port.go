package handler

import (
	"encoding/json"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/weiliang79/belune/internal/service"
	"github.com/weiliang79/belune/internal/store/generated"
)

// A pointer, not an int, so an absent or null key is distinguishable from the
// explicit 0 that clears the port. The handler rejects the former: a body that
// forgot the field must not silently reset the port.
type setContainerPortRequest struct {
	// Port the container listens on, 1-65535. 0 clears it, so the port is
	// resolved from the application's first domain and then 8080. Required.
	ContainerPort *int `json:"container_port"`
}

// SetContainerPort sets the port an application's container listens on. Its own
// endpoint rather than a field on the general update: that update rewrites every
// column, and a nullable port there could not tell "left out" from "clear it".
//
// ⚠️ Saved, not applied. The health-check URL and the proxy route both take the
// port at deploy time, so the running container is unaffected until the next
// deploy; the pending-change marker is stamped so the badge points at Reload.
// Without this a domain-less app listening on anything but 8080 fails its
// health probe on every deploy and cannot be corrected.
//
//apidoc:tag applications/resources
//apidoc:title Set Container Port
//apidoc:order 4
func (h *Handler) SetContainerPort(w http.ResponseWriter, r *http.Request) {
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

	var req setContainerPortRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.ContainerPort == nil {
		writeError(w, http.StatusBadRequest, "container_port is required; send 0 to clear it")
		return
	}
	port, err := service.ValidateContainerPort(*req.ContainerPort)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	if err := h.queries.SetApplicationContainerPort(r.Context(), generated.SetApplicationContainerPortParams{
		ID:            applicationUUID,
		ContainerPort: port,
	}); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to update container port")
		return
	}

	h.markConfigChanged(r.Context(), applicationUUID)
	h.audit(r, "set_container_port", "application", applicationID, map[string]any{
		"container_port": *req.ContainerPort,
	})

	// Re-read: the query is :exec, and the response should say what the row
	// holds rather than what was asked for.
	app, err := h.queries.GetApplication(r.Context(), applicationUUID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to read the updated application")
		return
	}
	writeJSON(w, http.StatusOK, toApplicationResponse(app))
}
