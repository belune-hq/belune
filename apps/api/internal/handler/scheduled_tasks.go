package handler

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/weiliang79/belune/internal/pkg/cronspec"
	"github.com/weiliang79/belune/internal/service"
	"github.com/weiliang79/belune/internal/store/generated"
)

const (
	defaultTaskTimeoutSeconds = 300
	defaultTaskKeepRuns       = 20
	maxTaskNameLen            = 100
	maxTaskCommandLen         = 8192
)

// timezoneOr returns requested when set, else fallback. Create passes UTC; a
// full-row update passes the stored zone, so a PUT that omits it cannot
// silently move when the schedule fires.
func timezoneOr(requested, fallback string) string {
	if v := strings.TrimSpace(requested); v != "" {
		return v
	}
	return fallback
}

type scheduledTaskRunResponse struct {
	ID         string     `json:"id"`
	Trigger    string     `json:"trigger"`
	Status     string     `json:"status"`
	ExitCode   *int32     `json:"exit_code,omitempty"`
	Output     string     `json:"output,omitempty"`
	SkipReason string     `json:"skip_reason,omitempty"`
	StartedAt  time.Time  `json:"started_at"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
}

// toScheduledTaskRunResponse maps a run; withOutput is false for the list view,
// where up to 64 KB per task would only be thrown away by the badge.
func toScheduledTaskRunResponse(r generated.ApplicationScheduledTaskRun, withOutput bool) scheduledTaskRunResponse {
	resp := scheduledTaskRunResponse{
		ID:        uuidToString(r.ID),
		Trigger:   r.Trigger,
		Status:    r.Status,
		StartedAt: r.StartedAt.Time,
	}
	if withOutput {
		resp.Output = r.Output
	}
	if r.ExitCode.Valid {
		v := r.ExitCode.Int32
		resp.ExitCode = &v
	}
	if r.SkipReason.Valid {
		resp.SkipReason = r.SkipReason.String
	}
	if r.FinishedAt.Valid {
		t := r.FinishedAt.Time
		resp.FinishedAt = &t
	}
	return resp
}

type scheduledTaskResponse struct {
	ID             string     `json:"id"`
	ApplicationID  string     `json:"application_id"`
	Name           string     `json:"name"`
	Command        string     `json:"command"`
	Cron           string     `json:"cron"`
	Timezone       string     `json:"timezone"`
	Enabled        bool       `json:"enabled"`
	TimeoutSeconds int32      `json:"timeout_seconds"`
	KeepRuns       int32      `json:"keep_runs"`
	LastRunAt      *time.Time `json:"last_run_at,omitempty"`
	// NextRunAt is rendered in the task's own timezone, so the offset in the
	// string is the quickest check that the cron means what its author thinks.
	NextRunAt *time.Time                `json:"next_run_at,omitempty"`
	LatestRun *scheduledTaskRunResponse `json:"latest_run,omitempty"`
	CreatedAt time.Time                 `json:"created_at"`
	UpdatedAt time.Time                 `json:"updated_at"`
}

func toScheduledTaskResponse(t generated.ApplicationScheduledTask, latest *generated.ApplicationScheduledTaskRun) scheduledTaskResponse {
	resp := scheduledTaskResponse{
		ID:             uuidToString(t.ID),
		ApplicationID:  uuidToString(t.ApplicationID),
		Name:           t.Name,
		Command:        t.Command,
		Cron:           t.Cron,
		Timezone:       t.Timezone,
		Enabled:        t.Enabled,
		TimeoutSeconds: int32(t.TimeoutSeconds),
		KeepRuns:       int32(t.KeepRuns),
		CreatedAt:      t.CreatedAt.Time,
		UpdatedAt:      t.UpdatedAt.Time,
	}
	if t.LastRunAt.Valid {
		v := t.LastRunAt.Time
		resp.LastRunAt = &v
	}
	if t.Enabled {
		if sched, err := cronspec.Parse(t.Cron, t.Timezone); err == nil {
			if loc, lerr := time.LoadLocation(t.Timezone); lerr == nil {
				next := sched.Next(time.Now()).In(loc)
				resp.NextRunAt = &next
			}
		}
	}
	if latest != nil {
		r := toScheduledTaskRunResponse(*latest, false)
		resp.LatestRun = &r
	}
	return resp
}

// resolveScheduledTask parses the application and task path params and checks
// access and ownership. On failure it writes the response and returns ok=false.
func (h *Handler) resolveScheduledTask(w http.ResponseWriter, r *http.Request) (generated.ApplicationScheduledTask, bool) {
	var appUUID, taskUUID pgtype.UUID
	if err := appUUID.Scan(chi.URLParam(r, "applicationId")); err != nil {
		writeError(w, http.StatusBadRequest, "invalid application id")
		return generated.ApplicationScheduledTask{}, false
	}
	if err := taskUUID.Scan(chi.URLParam(r, "taskId")); err != nil {
		writeError(w, http.StatusBadRequest, "invalid task id")
		return generated.ApplicationScheduledTask{}, false
	}
	if !h.canAccessApplication(r, appUUID) {
		writeError(w, http.StatusForbidden, "access denied")
		return generated.ApplicationScheduledTask{}, false
	}
	task, err := h.queries.GetScheduledTask(r.Context(), taskUUID)
	if err != nil || task.ApplicationID != appUUID {
		writeError(w, http.StatusNotFound, "scheduled task not found")
		return generated.ApplicationScheduledTask{}, false
	}
	return task, true
}

func (h *Handler) scheduledTaskDispatcher() *service.ScheduledTaskDispatcher {
	max := 0
	if h.cfg != nil {
		max = h.cfg.ScheduledTaskMaxConcurrent
	}
	return service.NewScheduledTaskDispatcher(h.queries, h.asynq, max)
}

// ListScheduledTasks lists an application's scheduled tasks with each one's
// next run time and latest run outcome.
//
//apidoc:tag applications/scheduled-tasks
//apidoc:title Get Tasks
//apidoc:order 1
func (h *Handler) ListScheduledTasks(w http.ResponseWriter, r *http.Request) {
	var appUUID pgtype.UUID
	if err := appUUID.Scan(chi.URLParam(r, "applicationId")); err != nil {
		writeError(w, http.StatusBadRequest, "invalid application id")
		return
	}
	if !h.canAccessApplication(r, appUUID) {
		writeError(w, http.StatusForbidden, "access denied")
		return
	}
	tasks, err := h.queries.ListScheduledTasksForApplication(r.Context(), appUUID)
	if err != nil {
		writeServerError(w, r, "failed to list scheduled tasks", err)
		return
	}
	latestRows, err := h.queries.LatestScheduledTaskRuns(r.Context(), appUUID)
	if err != nil {
		writeServerError(w, r, "failed to list scheduled tasks", err)
		return
	}
	latest := make(map[pgtype.UUID]generated.ApplicationScheduledTaskRun, len(latestRows))
	for _, run := range latestRows {
		latest[run.TaskID] = run
	}
	out := make([]scheduledTaskResponse, 0, len(tasks))
	for _, t := range tasks {
		var lr *generated.ApplicationScheduledTaskRun
		if run, ok := latest[t.ID]; ok {
			lr = &run
		}
		out = append(out, toScheduledTaskResponse(t, lr))
	}
	writeJSON(w, http.StatusOK, out)
}

type scheduledTaskRequest struct {
	Name           *string `json:"name"`
	Command        *string `json:"command"`
	Cron           *string `json:"cron"`
	Timezone       *string `json:"timezone"`
	Enabled        *bool   `json:"enabled"`
	TimeoutSeconds *int32  `json:"timeout_seconds"`
	KeepRuns       *int32  `json:"keep_runs"`
}

// scheduledTaskFields are a task's writable fields after defaults/fallbacks.
type scheduledTaskFields struct {
	name, command, cron, timezone string
	enabled                       bool
	timeoutSeconds, keepRuns      int32
}

// mergeScheduledTask overlays req on base. Update calls it with the stored task
// because UpdateScheduledTask writes every column: a PUT that omits a field must
// keep it, not clear it.
func mergeScheduledTask(base scheduledTaskFields, req scheduledTaskRequest) scheduledTaskFields {
	if req.Name != nil {
		base.name = strings.TrimSpace(*req.Name)
	}
	if req.Command != nil {
		base.command = strings.TrimSpace(*req.Command)
	}
	if req.Cron != nil {
		base.cron = strings.TrimSpace(*req.Cron)
	}
	if req.Timezone != nil {
		base.timezone = strings.TrimSpace(*req.Timezone)
	}
	if req.Enabled != nil {
		base.enabled = *req.Enabled
	}
	if req.TimeoutSeconds != nil {
		base.timeoutSeconds = *req.TimeoutSeconds
	}
	if req.KeepRuns != nil {
		base.keepRuns = *req.KeepRuns
	}
	return base
}

func validateScheduledTask(f scheduledTaskFields) string {
	switch {
	case f.name == "":
		return "name is required"
	case len(f.name) > maxTaskNameLen:
		return "name is too long"
	case f.command == "":
		return "command is required"
	case len(f.command) > maxTaskCommandLen:
		return "command is too long"
	case f.cron == "":
		return "cron is required"
	case f.timeoutSeconds < 1 || f.timeoutSeconds > 86400:
		return "timeout_seconds must be between 1 and 86400"
	case f.keepRuns < 1 || f.keepRuns > 1000:
		return "keep_runs must be between 1 and 1000"
	}
	if err := cronspec.ValidateTimezone(f.timezone); err != nil {
		return err.Error()
	}
	if _, err := cronspec.ParseMinimumMinute(f.cron, f.timezone); err != nil {
		return "invalid cron schedule: " + err.Error()
	}
	return ""
}

// CreateScheduledTask creates a task. The command is stored as plaintext and is
// kept out of the audit log: secrets belong in environment variables, referenced
// as $VAR, and auditing the body would copy any pasted one into a second place.
//
//apidoc:tag applications/scheduled-tasks
//apidoc:title Create Task
//apidoc:order 2
func (h *Handler) CreateScheduledTask(w http.ResponseWriter, r *http.Request) {
	var appUUID pgtype.UUID
	if err := appUUID.Scan(chi.URLParam(r, "applicationId")); err != nil {
		writeError(w, http.StatusBadRequest, "invalid application id")
		return
	}
	if !h.canAccessApplication(r, appUUID) {
		writeError(w, http.StatusForbidden, "access denied")
		return
	}
	var req scheduledTaskRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	f := mergeScheduledTask(scheduledTaskFields{
		timezone: cronspec.DefaultTimezone, enabled: true,
		timeoutSeconds: defaultTaskTimeoutSeconds, keepRuns: defaultTaskKeepRuns,
	}, req)
	if msg := validateScheduledTask(f); msg != "" {
		writeError(w, http.StatusBadRequest, msg)
		return
	}
	task, err := h.queries.CreateScheduledTask(r.Context(), generated.CreateScheduledTaskParams{
		ApplicationID: appUUID, Name: f.name, Command: f.command, Cron: f.cron, Timezone: f.timezone,
		Enabled: f.enabled, TimeoutSeconds: f.timeoutSeconds, KeepRuns: f.keepRuns,
	})
	if err != nil {
		writeServerError(w, r, "failed to create scheduled task", err)
		return
	}
	h.audit(r, "create_scheduled_task", "application_scheduled_task", uuidToString(task.ID), map[string]any{
		"application_id": uuidToString(appUUID), "name": task.Name,
	})
	writeJSON(w, http.StatusCreated, toScheduledTaskResponse(task, nil))
}

//apidoc:tag applications/scheduled-tasks
//apidoc:title Update Task
//apidoc:order 3
func (h *Handler) UpdateScheduledTask(w http.ResponseWriter, r *http.Request) {
	task, ok := h.resolveScheduledTask(w, r)
	if !ok {
		return
	}
	var req scheduledTaskRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	f := mergeScheduledTask(scheduledTaskFields{
		name: task.Name, command: task.Command, cron: task.Cron, timezone: task.Timezone,
		enabled: task.Enabled, timeoutSeconds: int32(task.TimeoutSeconds), keepRuns: int32(task.KeepRuns),
	}, req)
	if msg := validateScheduledTask(f); msg != "" {
		writeError(w, http.StatusBadRequest, msg)
		return
	}
	updated, err := h.queries.UpdateScheduledTask(r.Context(), generated.UpdateScheduledTaskParams{
		ID: task.ID, Name: f.name, Command: f.command, Cron: f.cron, Timezone: f.timezone,
		Enabled: f.enabled, TimeoutSeconds: f.timeoutSeconds, KeepRuns: f.keepRuns,
	})
	if err != nil {
		writeServerError(w, r, "failed to update scheduled task", err)
		return
	}
	h.audit(r, "update_scheduled_task", "application_scheduled_task", uuidToString(task.ID), map[string]any{
		"application_id": uuidToString(task.ApplicationID), "name": updated.Name,
	})
	writeJSON(w, http.StatusOK, toScheduledTaskResponse(updated, nil))
}

//apidoc:tag applications/scheduled-tasks
//apidoc:title Delete Task
//apidoc:order 4
func (h *Handler) DeleteScheduledTask(w http.ResponseWriter, r *http.Request) {
	task, ok := h.resolveScheduledTask(w, r)
	if !ok {
		return
	}
	if err := h.queries.DeleteScheduledTask(r.Context(), task.ID); err != nil {
		writeServerError(w, r, "failed to delete scheduled task", err)
		return
	}
	h.audit(r, "delete_scheduled_task", "application_scheduled_task", uuidToString(task.ID), map[string]any{
		"application_id": uuidToString(task.ApplicationID), "name": task.Name,
	})
	w.WriteHeader(http.StatusNoContent)
}

// RunScheduledTask runs a task now. A manual run never shifts the schedule.
//
//apidoc:tag applications/scheduled-tasks
//apidoc:title Run Task
//apidoc:order 5
func (h *Handler) RunScheduledTask(w http.ResponseWriter, r *http.Request) {
	task, ok := h.resolveScheduledTask(w, r)
	if !ok {
		return
	}
	run, _, err := h.scheduledTaskDispatcher().Dispatch(r.Context(), task, service.RunTriggerManual)
	if err != nil {
		if nr, ok := service.IsNotRunnable(err); ok {
			writeError(w, http.StatusConflict, nr.Reason)
			return
		}
		writeServerError(w, r, "failed to start scheduled task", err)
		return
	}
	h.audit(r, "run_scheduled_task", "application_scheduled_task", uuidToString(task.ID), map[string]any{
		"application_id": uuidToString(task.ApplicationID), "run_id": uuidToString(run.ID),
	})
	writeJSON(w, http.StatusAccepted, toScheduledTaskRunResponse(run, false))
}

// ListScheduledTaskRuns returns a task's run history, newest first, with output.
//
//apidoc:tag applications/scheduled-tasks
//apidoc:title Get Task Runs
//apidoc:order 6
func (h *Handler) ListScheduledTaskRuns(w http.ResponseWriter, r *http.Request) {
	task, ok := h.resolveScheduledTask(w, r)
	if !ok {
		return
	}
	// The prune keeps keep_runs of finished runs AND keep_runs of skips, so the
	// table legitimately holds twice that; a single keep_runs limit let newer
	// skips push every real run out of the list.
	limit := int32(task.KeepRuns) * 2
	if v, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && v > 0 && int32(v) < limit {
		limit = int32(v)
	}
	runs, err := h.queries.ListScheduledTaskRuns(r.Context(), generated.ListScheduledTaskRunsParams{TaskID: task.ID, Limit: limit})
	if err != nil {
		writeServerError(w, r, "failed to list runs", err)
		return
	}
	out := make([]scheduledTaskRunResponse, 0, len(runs))
	for _, run := range runs {
		out = append(out, toScheduledTaskRunResponse(run, true))
	}
	writeJSON(w, http.StatusOK, out)
}
