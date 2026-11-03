package handler

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/belune-hq/belune/internal/service"
	"github.com/belune-hq/belune/internal/store/generated"
)

type processInstanceResponse struct {
	ID            string  `json:"id"`
	Idx           int32   `json:"idx"`
	ContainerName string  `json:"container_name"`
	ContainerID   *string `json:"container_id"`
	Status        string  `json:"status"`
}

// processTypeResponse deliberately omits replicas, cpu_limit and memory_limit:
// nothing reads them in 0.3.0, so showing them would imply a control that does not
// exist.
type processTypeResponse struct {
	ID                 string                    `json:"id"`
	Name               string                    `json:"name"`
	Command            string                    `json:"command"`
	Routable           bool                      `json:"routable"`
	HealthCheckType    *string                   `json:"health_check_type"`
	HealthCheckCommand *string                   `json:"health_check_command"`
	Instances          []processInstanceResponse `json:"instances"`
	CreatedAt          string                    `json:"created_at"`
	UpdatedAt          string                    `json:"updated_at"`
}

func toProcessTypeResponse(pt generated.ApplicationProcessType, insts []generated.ApplicationInstance) processTypeResponse {
	out := processTypeResponse{
		ID:        uuidToString(pt.ID),
		Name:      pt.Name,
		Command:   pt.Command,
		Routable:  pt.Routable,
		Instances: make([]processInstanceResponse, 0, len(insts)),
		CreatedAt: pt.CreatedAt.Time.Format("2006-01-02T15:04:05Z07:00"),
		UpdatedAt: pt.UpdatedAt.Time.Format("2006-01-02T15:04:05Z07:00"),
	}
	if pt.HealthCheckType.Valid {
		v := pt.HealthCheckType.String
		out.HealthCheckType = &v
	}
	if pt.HealthCheckCommand.Valid {
		v := pt.HealthCheckCommand.String
		out.HealthCheckCommand = &v
	}
	for _, in := range insts {
		r := processInstanceResponse{ID: uuidToString(in.ID), Idx: in.Idx, ContainerName: in.ContainerName, Status: in.Status}
		if in.ContainerID.Valid {
			v := in.ContainerID.String
			r.ContainerID = &v
		}
		out.Instances = append(out.Instances, r)
	}
	return out
}

// notConfigurable holds the three columns that exist but that nothing honours yet.
// They are accepted in no request body: a value stored and silently ignored is worse
// than a field that is not there. Any non-null value is a 400.
type notConfigurable struct {
	Replicas    *json.RawMessage `json:"replicas"`
	CPULimit    *json.RawMessage `json:"cpu_limit"`
	MemoryLimit *json.RawMessage `json:"memory_limit"`
}

func (n notConfigurable) rejected() string {
	switch {
	case n.Replicas != nil:
		return "replicas is not configurable in this release: every process type runs one instance"
	case n.CPULimit != nil, n.MemoryLimit != nil:
		return "per-process-type cpu_limit and memory_limit are not supported: the application's limits apply to every process type"
	}
	return ""
}

type createProcessTypeRequest struct {
	Name               string `json:"name"`
	Command            string `json:"command"`
	Routable           bool   `json:"routable"`
	HealthCheckType    string `json:"health_check_type"`
	HealthCheckCommand string `json:"health_check_command"`
	notConfigurable
}

// updateProcessTypeRequest has no name and no routable: the name is an identity and
// moving the routable flag moves the proxy's upstream. Sent, they are 400s via
// DisallowUnknownFields. Absent fields keep their stored value.
type updateProcessTypeRequest struct {
	Command            *string `json:"command"`
	HealthCheckType    *string `json:"health_check_type"`
	HealthCheckCommand *string `json:"health_check_command"`
	notConfigurable
}

func decodeStrict(r *http.Request, v any) error {
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	return dec.Decode(v)
}

// writeProcessTypeError maps the service's errors onto statuses. The two uniqueness
// conflicts are different messages on purpose.
func (h *Handler) writeProcessTypeError(w http.ResponseWriter, r *http.Request, msg string, err error) {
	var verr *service.ProcessTypeValidationError
	switch {
	case errors.As(err, &verr):
		writeError(w, http.StatusBadRequest, verr.Msg)
	case errors.Is(err, service.ErrProcessTypeNotFound):
		writeError(w, http.StatusNotFound, err.Error())
	case errors.Is(err, service.ErrProcessTypeNameTaken),
		errors.Is(err, service.ErrRoutableTypeExists),
		errors.Is(err, service.ErrRoutableTypeProtected):
		writeError(w, http.StatusConflict, err.Error())
	default:
		writeServerError(w, r, msg, err)
	}
}

func (h *Handler) parseProcessTypeIDs(w http.ResponseWriter, r *http.Request) (app, pt pgtype.UUID, ok bool) {
	if err := app.Scan(chi.URLParam(r, "applicationId")); err != nil {
		writeError(w, http.StatusBadRequest, "invalid application id")
		return app, pt, false
	}
	if !h.canAccessApplication(r, app) {
		writeError(w, http.StatusForbidden, "access denied")
		return app, pt, false
	}
	if raw := chi.URLParam(r, "processTypeId"); raw != "" {
		if err := pt.Scan(raw); err != nil {
			writeError(w, http.StatusBadRequest, "invalid process type id")
			return app, pt, false
		}
	}
	return app, pt, true
}

//apidoc:tag applications/resources
//apidoc:title Get Process Types
func (h *Handler) ListProcessTypes(w http.ResponseWriter, r *http.Request) {
	appUUID, _, ok := h.parseProcessTypeIDs(w, r)
	if !ok {
		return
	}
	types, byType, err := h.appService.ListProcessTypes(r.Context(), appUUID)
	if err != nil {
		writeServerError(w, r, "failed to list process types", err)
		return
	}
	out := make([]processTypeResponse, 0, len(types))
	for _, pt := range types {
		out = append(out, toProcessTypeResponse(pt, byType[pt.ID]))
	}
	writeJSON(w, http.StatusOK, out)
}

// CreateProcessType creates the type and its instance row. It deploys nothing: the
// container appears on the next deploy.
//
//apidoc:tag applications/resources
//apidoc:title Create Process Type
func (h *Handler) CreateProcessType(w http.ResponseWriter, r *http.Request) {
	appUUID, _, ok := h.parseProcessTypeIDs(w, r)
	if !ok {
		return
	}
	var req createProcessTypeRequest
	if err := decodeStrict(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}
	if msg := req.rejected(); msg != "" {
		writeError(w, http.StatusBadRequest, msg)
		return
	}

	pt, inst, err := h.appService.CreateProcessType(r.Context(), appUUID, service.ProcessTypeInput{
		Name: req.Name, Command: req.Command, Routable: req.Routable,
		HealthCheckType: req.HealthCheckType, HealthCheckCommand: req.HealthCheckCommand,
	})
	if err != nil {
		h.writeProcessTypeError(w, r, "failed to create process type", err)
		return
	}

	h.audit(r, "create_process_type", "process_type", uuidToString(pt.ID), map[string]any{
		"application_id": uuidToString(appUUID),
		"name":           pt.Name,
		"routable":       pt.Routable,
	})
	writeJSON(w, http.StatusCreated, toProcessTypeResponse(pt, []generated.ApplicationInstance{inst}))
}

//apidoc:tag applications/resources
//apidoc:title Update Process Type
func (h *Handler) UpdateProcessType(w http.ResponseWriter, r *http.Request) {
	appUUID, typeUUID, ok := h.parseProcessTypeIDs(w, r)
	if !ok {
		return
	}
	var req updateProcessTypeRequest
	if err := decodeStrict(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}
	if msg := req.rejected(); msg != "" {
		writeError(w, http.StatusBadRequest, msg)
		return
	}

	pt, inst, err := h.appService.UpdateProcessType(r.Context(), appUUID, typeUUID, service.ProcessTypePatch{
		Command: req.Command, HealthCheckType: req.HealthCheckType, HealthCheckCommand: req.HealthCheckCommand,
	})
	if err != nil {
		h.writeProcessTypeError(w, r, "failed to update process type", err)
		return
	}

	h.audit(r, "update_process_type", "process_type", uuidToString(pt.ID), map[string]any{
		"application_id": uuidToString(appUUID),
		"name":           pt.Name,
	})
	writeJSON(w, http.StatusOK, toProcessTypeResponse(pt, []generated.ApplicationInstance{inst}))
}

//apidoc:tag applications/resources
//apidoc:title Delete Process Type
func (h *Handler) DeleteProcessType(w http.ResponseWriter, r *http.Request) {
	appUUID, typeUUID, ok := h.parseProcessTypeIDs(w, r)
	if !ok {
		return
	}
	name := ""
	if pt, err := h.queries.GetProcessType(r.Context(), typeUUID); err == nil {
		name = pt.Name
	}
	warning, err := h.appService.DeleteProcessType(r.Context(), appUUID, typeUUID)
	if err != nil {
		h.writeProcessTypeError(w, r, "failed to delete process type", err)
		return
	}

	h.audit(r, "delete_process_type", "process_type", uuidToString(typeUUID), map[string]any{
		"application_id": uuidToString(appUUID),
		"name":           name,
	})
	resp := map[string]string{"status": "deleted"}
	if warning != "" {
		resp["warning"] = warning
	}
	writeJSON(w, http.StatusOK, resp)
}
