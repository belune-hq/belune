package handler

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/hibiken/asynq"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/belune-hq/belune/internal/instances"
	"github.com/belune-hq/belune/internal/service"
	"github.com/belune-hq/belune/internal/store/generated"
	"github.com/belune-hq/belune/internal/worker"
)

type processInstanceResponse struct {
	ID            string  `json:"id"`
	Idx           int32   `json:"idx"`
	ContainerName string  `json:"container_name"`
	ContainerID   *string `json:"container_id"`
	Status        string  `json:"status"`
}

// processTypeResponse carries replicas and the type's own limits. CPULimit and
// MemoryLimit are null when the type inherits the application's.
type processTypeResponse struct {
	ID                 string   `json:"id"`
	Name               string   `json:"name"`
	Command            string   `json:"command"`
	Routable           bool     `json:"routable"`
	Replicas           int32    `json:"replicas"`
	CPULimit           *float64 `json:"cpu_limit"`
	MemoryLimit        *int64   `json:"memory_limit"`
	HealthCheckType    *string  `json:"health_check_type"`
	HealthCheckCommand *string  `json:"health_check_command"`
	// ResolvedHealth is what the type ACTUALLY runs with, computed by the one
	// server-side rule (instances.ResolveHealth). The UI renders from it rather than
	// mirroring the rule, because a copy that drifts would tell the operator the wrong
	// thing about what is running.
	ResolvedHealth resolvedHealthResponse    `json:"resolved_health"`
	Instances      []processInstanceResponse `json:"instances"`
	CreatedAt      string                    `json:"created_at"`
	UpdatedAt      string                    `json:"updated_at"`
}

type resolvedHealthResponse struct {
	Kind   string `json:"kind"`   // http | command | none
	Source string `json:"source"` // set | inherited | default
}

func toProcessTypeResponse(app generated.Application, pt generated.ApplicationProcessType, insts []generated.ApplicationInstance) processTypeResponse {
	rh := instances.ResolveHealth(app, pt)
	out := processTypeResponse{
		ResolvedHealth: resolvedHealthResponse{Kind: rh.Type, Source: rh.Source},
		ID:             uuidToString(pt.ID),
		Name:           pt.Name,
		Command:        pt.Command,
		Routable:       pt.Routable,
		Replicas:       pt.Replicas,
		Instances:      make([]processInstanceResponse, 0, len(insts)),
		CreatedAt:      pt.CreatedAt.Time.Format("2006-01-02T15:04:05Z07:00"),
		UpdatedAt:      pt.UpdatedAt.Time.Format("2006-01-02T15:04:05Z07:00"),
	}
	if pt.CpuLimit.Valid {
		v := pt.CpuLimit.Float64
		out.CPULimit = &v
	}
	if pt.MemoryLimit.Valid {
		v := pt.MemoryLimit.Int64
		out.MemoryLimit = &v
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

type createProcessTypeRequest struct {
	Name               string `json:"name"`
	Command            string `json:"command"`
	Routable           bool   `json:"routable"`
	HealthCheckType    string `json:"health_check_type"`
	HealthCheckCommand string `json:"health_check_command"`
	// Replicas absent = 1. CPULimit / MemoryLimit absent or 0 = inherit.
	Replicas    *int32   `json:"replicas"`
	CPULimit    *float64 `json:"cpu_limit"`
	MemoryLimit *int64   `json:"memory_limit"`
}

// updateProcessTypeRequest has no name and no routable: the name is an identity and
// moving the routable flag moves the proxy's upstream. Sent, they are 400s via
// DisallowUnknownFields. Absent fields keep their stored value.
type updateProcessTypeRequest struct {
	Command            *string `json:"command"`
	HealthCheckType    *string `json:"health_check_type"`
	HealthCheckCommand *string `json:"health_check_command"`
	// Replicas absent keeps the stored count. For the limits, absent keeps and an
	// explicit 0 clears back to inheriting the application's.
	Replicas    *int32   `json:"replicas"`
	CPULimit    *float64 `json:"cpu_limit"`
	MemoryLimit *int64   `json:"memory_limit"`
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

//apidoc:tag applications/process-types
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
	app, err := h.queries.GetApplication(r.Context(), appUUID)
	if err != nil {
		writeError(w, http.StatusNotFound, "application not found")
		return
	}
	out := make([]processTypeResponse, 0, len(types))
	for _, pt := range types {
		out = append(out, toProcessTypeResponse(app, pt, byType[pt.ID]))
	}
	writeJSON(w, http.StatusOK, out)
}

// CreateProcessType creates the type and its instance row. It deploys nothing: the
// container appears on the next deploy.
//
//apidoc:tag applications/process-types
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

	pt, insts, plan, err := h.appService.CreateProcessType(r.Context(), appUUID, service.ProcessTypeInput{
		Name: req.Name, Command: req.Command, Routable: req.Routable,
		HealthCheckType: req.HealthCheckType, HealthCheckCommand: req.HealthCheckCommand,
		Replicas: req.Replicas, CPULimit: req.CPULimit, MemoryLimit: req.MemoryLimit,
	})
	if err != nil {
		h.writeProcessTypeError(w, r, "failed to create process type", err)
		return
	}

	app, err := h.queries.GetApplication(r.Context(), appUUID)
	if err != nil {
		writeError(w, http.StatusNotFound, "application not found")
		return
	}
	h.audit(r, "create_process_type", "process_type", uuidToString(pt.ID), map[string]any{
		"application_id": uuidToString(appUUID),
		"name":           pt.Name,
		"routable":       pt.Routable,
		"replicas":       pt.Replicas,
	})
	// A new type's containers appear on the next deploy, exactly as before; a type
	// created with replicas > 1 on an already-deployed application gets them now.
	h.enqueueScale(plan)
	writeJSON(w, http.StatusCreated, toProcessTypeResponse(app, pt, insts))
}

//apidoc:tag applications/process-types
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
	pt, insts, plan, err := h.appService.UpdateProcessType(r.Context(), appUUID, typeUUID, service.ProcessTypePatch{
		Command: req.Command, HealthCheckType: req.HealthCheckType, HealthCheckCommand: req.HealthCheckCommand,
		Replicas: req.Replicas, CPULimit: req.CPULimit, MemoryLimit: req.MemoryLimit,
	})
	if err != nil {
		h.writeProcessTypeError(w, r, "failed to update process type", err)
		return
	}

	app, err := h.queries.GetApplication(r.Context(), appUUID)
	if err != nil {
		writeError(w, http.StatusNotFound, "application not found")
		return
	}
	h.audit(r, "update_process_type", "process_type", uuidToString(pt.ID), map[string]any{
		"application_id": uuidToString(appUUID),
		"name":           pt.Name,
		"replicas":       pt.Replicas,
	})
	h.enqueueScale(plan)
	writeJSON(w, http.StatusOK, toProcessTypeResponse(app, pt, insts))
}

//apidoc:tag applications/process-types
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

// instanceRestartWait is how long the request waits for a restart before answering
// 202. It is shorter than the 15s handler timeout on purpose: past that, the
// TimeoutHandler answers 503 for work that is still succeeding, and the UI never
// shows its result. The restart itself is not bounded by it.
var instanceRestartWait = 10 * time.Second

// RestartInstance restarts one container of an application, not the others.
// POST /api/projects/{projectId}/applications/{applicationId}/instances/{instanceId}/restart
//
// 200 when it finished inside the wait, 202 when it is still going (it completes
// regardless). The application-level restart is unchanged.
//
//apidoc:tag applications/process-types
//apidoc:title Restart Instance
//apidoc:order 5
func (h *Handler) RestartInstance(w http.ResponseWriter, r *http.Request) {
	var appUUID, instUUID pgtype.UUID
	if err := appUUID.Scan(chi.URLParam(r, "applicationId")); err != nil {
		writeError(w, http.StatusBadRequest, "invalid application id")
		return
	}
	if err := instUUID.Scan(chi.URLParam(r, "instanceId")); err != nil {
		writeError(w, http.StatusBadRequest, "invalid instance id")
		return
	}
	if !h.canAccessApplication(r, appUUID) {
		writeError(w, http.StatusForbidden, "access denied")
		return
	}
	inst, err := h.queries.GetInstance(r.Context(), instUUID)
	if err != nil || inst.ApplicationID != appUUID {
		writeError(w, http.StatusNotFound, service.ErrInstanceNotFound.Error())
		return
	}

	// Audited BEFORE the work, as a choice: the operator's intent is the fact worth
	// recording, and the outcome (which may be a 202 that finishes later) is logged by
	// the goroutine below rather than known here.
	h.audit(r, "restart_instance", "application", uuidToString(appUUID), map[string]any{
		"instance_id":    uuidToString(instUUID),
		"container_name": inst.ContainerName,
	})

	done := make(chan error, 1)
	go func() {
		_, err := h.appService.RestartInstance(context.WithoutCancel(r.Context()), appUUID, instUUID)
		if err != nil {
			slog.Error("restart instance failed", "instance_id", uuidToString(instUUID), "container", inst.ContainerName, "error", err)
		}
		done <- err
	}()

	select {
	case err := <-done:
		switch {
		case err == nil:
			writeJSON(w, http.StatusOK, map[string]string{"status": "restarted"})
		case errors.Is(err, service.ErrInstanceContainerGone):
			writeError(w, http.StatusConflict, err.Error())
		case errors.Is(err, service.ErrServerUnreachable):
			writeError(w, http.StatusBadGateway, "failed to reach the application's server")
		default:
			writeError(w, http.StatusInternalServerError, "failed to restart the instance")
		}
	case <-time.After(instanceRestartWait):
		writeJSON(w, http.StatusAccepted, map[string]string{"status": "restarting"})
	case <-r.Context().Done():
		// Nothing to write to: the client or the timeout handler has gone. The
		// restart carries on in its own context.
	}
}

// enqueueScale hands the container half of a scale change to the worker. The rows are
// already committed; if the queue is unavailable the next deploy still converges the
// containers on the rows, so this is logged, not failed.
func (h *Handler) enqueueScale(plan service.ScalePlan) {
	if plan.Empty() || h.asynq == nil {
		return
	}
	task, err := worker.NewScaleTask(plan)
	if err != nil {
		slog.Error("scale: could not build the container task", "error", err)
		return
	}
	if _, err := h.asynq.Enqueue(task, asynq.MaxRetry(0)); err != nil {
		slog.Error("scale: could not queue the container changes; the next deploy will converge them",
			"application_id", uuidToString(plan.ApplicationID), "error", err)
	}
}
