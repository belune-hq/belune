package handler

import (
	"encoding/json"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/weiliang79/belune/internal/service"
	"github.com/weiliang79/belune/internal/store"
	"github.com/weiliang79/belune/internal/store/generated"
)

// The health check has two mechanisms behind one type field, so it is written
// through its own endpoint rather than the general update: the fields that
// belong to the unselected mechanism must be cleared as a unit, or a later
// deploy would apply a stale command it can no longer see in the UI.
//
//	http    — control-plane probe of an HTTP path after deploy (existing)
//	command — native Docker HEALTHCHECK, run in the container, continuous, and
//	          feeding the application's status via the eventwatcher
//	none    — no check
type healthCheckRequest struct {
	Type string `json:"type"`

	// http
	Path         string `json:"path"`
	ExpectStatus int    `json:"expect_status"`

	// command
	Command            string `json:"command"`
	IntervalSeconds    int    `json:"interval_seconds"`
	RetriesCount       int    `json:"retries"`
	StartPeriodSeconds int    `json:"start_period_seconds"`

	// shared
	TimeoutSeconds int `json:"timeout_seconds"`

	// ContainerPort is the one pointer in this struct, on purpose. Every field
	// above is one internal-consistency unit written whole; this is not part of
	// it. It is also the proxy's upstream fallback (ResolveUpstreamPort: domain,
	// then app, then 8080), so a caller that PUTs a health check without
	// mentioning it must not silently wipe a routing default — the same
	// full-row field-wipe as the ssl_mode bug. Absent or null leaves the stored
	// port alone, 0 clears it to NULL, 1-65535 sets it.
	ContainerPort *int `json:"container_port"`
}

// validateHealthCheck converts the REST wire shape into the shared input and
// delegates. The wire struct stays here, with its own json tags, so the
// published request schema is unaffected by the validation moving — the
// generator resolves the handler's type, not the service's.
func validateHealthCheck(id pgtype.UUID, req healthCheckRequest) (generated.SetApplicationHealthCheckParams, error) {
	return service.ValidateHealthCheck(id, service.HealthCheckInput{
		Type:               req.Type,
		Path:               req.Path,
		ExpectStatus:       req.ExpectStatus,
		Command:            req.Command,
		IntervalSeconds:    req.IntervalSeconds,
		RetriesCount:       req.RetriesCount,
		StartPeriodSeconds: req.StartPeriodSeconds,
		TimeoutSeconds:     req.TimeoutSeconds,
	})
}

// SetHealthCheck configures how an application's health is checked. A change
// takes effect on the next deploy — the check is baked into the container at
// create time (command) or run by the deploy (http) — so the pending-change
// marker is stamped, and the change is applied when the user next deploys.
//
//apidoc:tag applications/resources
//apidoc:title Set Health Check
//apidoc:order 3
func (h *Handler) SetHealthCheck(w http.ResponseWriter, r *http.Request) {
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

	var req healthCheckRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	params, err := validateHealthCheck(applicationUUID, req)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	var port pgtype.Int4
	if req.ContainerPort != nil {
		if port, err = service.ValidateContainerPort(*req.ContainerPort); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
	}

	if err := store.WithTx(r.Context(), h.db, func(q *generated.Queries) error {
		if _, err := q.SetApplicationHealthCheck(r.Context(), params); err != nil {
			return err
		}
		if req.ContainerPort == nil {
			return nil
		}
		return q.SetApplicationContainerPort(r.Context(), generated.SetApplicationContainerPortParams{
			ID:            applicationUUID,
			ContainerPort: port,
		})
	}); err != nil {
		writeServerError(w, r, "failed to update health check", err)
		return
	}

	// A command check is baked into the container image config, and the HTTP
	// probe runs during deploy — either way the running container does not
	// reflect the change until the next deploy.
	h.markConfigChanged(r.Context(), applicationUUID)

	details := map[string]any{"type": req.Type}
	if req.ContainerPort != nil {
		details["container_port"] = *req.ContainerPort
	}
	h.audit(r, "set_health_check", "application", applicationID, details)

	// Re-read: the health-check statement's returned row predates the port write.
	app, err := h.queries.GetApplication(r.Context(), applicationUUID)
	if err != nil {
		writeServerError(w, r, "failed to read the updated application", err)
		return
	}
	writeJSON(w, http.StatusOK, toApplicationResponse(app))
}
