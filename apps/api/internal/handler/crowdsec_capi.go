package handler

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/belune-hq/belune/internal/crowdsec"
	"github.com/belune-hq/belune/internal/pkg/redact"
	"github.com/belune-hq/belune/internal/worker"
	"github.com/hibiken/asynq"
)

func (h *Handler) SetCAPI(manager *crowdsec.CAPIManager) { h.capi = manager }

// GetCrowdSecCAPI reads local Central API configuration and the last deliberate check.
//
//apidoc:tag platform/maintenance
//apidoc:title Get Central API Status
//apidoc:order 15
func (h *Handler) GetCrowdSecCAPI(w http.ResponseWriter, r *http.Request) {
	if h.capi == nil {
		writeError(w, http.StatusServiceUnavailable, "Central API management is unavailable")
		return
	}
	state, err := h.capi.Status(r.Context())
	if err != nil {
		writeServerError(w, r, "failed to read Central API state", err)
		return
	}
	writeJSON(w, http.StatusOK, state)
}

// UpdateCrowdSecCAPI enqueues the restart and health wait outside the API timeout.
//
//apidoc:tag platform/maintenance
//apidoc:title Change Central API Settings
//apidoc:order 16
func (h *Handler) UpdateCrowdSecCAPI(w http.ResponseWriter, r *http.Request) {
	if h.capi == nil {
		writeError(w, http.StatusServiceUnavailable, "Central API management is unavailable")
		return
	}
	var change crowdsec.CAPIChange
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&change); err != nil {
		writeError(w, http.StatusBadRequest, "invalid Central API settings")
		return
	}
	if change.Enabled == nil && change.Sharing == nil && !change.RestartConsole {
		writeError(w, http.StatusBadRequest, "an enabled state or sharing preferences are required")
		return
	}
	if change.RestartConsole && (change.Enabled != nil || change.Sharing != nil) {
		writeError(w, http.StatusBadRequest, "restart console enrollment separately from changing Central API settings")
		return
	}
	err := h.capi.Queue(r.Context(), change, func(payload []byte) error {
		_, err := h.asynq.Enqueue(asynq.NewTask(worker.TypeCrowdSecCAPI, payload), asynq.TaskID(worker.TypeCrowdSecCAPI), asynq.MaxRetry(2), asynq.Timeout(10*time.Minute))
		return err
	})
	if err != nil {
		if errors.Is(err, crowdsec.ErrCAPIBusy) || errors.Is(err, asynq.ErrTaskIDConflict) {
			writeError(w, http.StatusConflict, "a Central API change is already in progress")
		} else if errors.Is(err, crowdsec.ErrCAPIRateLimited) {
			writeError(w, http.StatusTooManyRequests, err.Error())
		} else {
			writeServerError(w, r, "could not queue Central API change", err)
		}
		return
	}
	details := map[string]any{"restart_console": change.RestartConsole}
	if change.Enabled != nil {
		details["enabled"] = *change.Enabled
	}
	if change.Sharing != nil {
		details["sharing"] = change.Sharing
	}
	h.audit(r, "update_crowdsec_capi", "crowdsec", "default", details)
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "queued"})
}

// CheckCrowdSecCAPI deliberately authenticates once; status polling never does.
//
//apidoc:tag platform/maintenance
//apidoc:title Check Central API Connection
//apidoc:order 17
func (h *Handler) CheckCrowdSecCAPI(w http.ResponseWriter, r *http.Request) {
	if h.capi == nil {
		writeError(w, http.StatusServiceUnavailable, "Central API management is unavailable")
		return
	}
	result, err := h.capi.CheckConnection(r.Context())
	if err != nil {
		if errors.Is(err, crowdsec.ErrCAPIBusy) {
			writeError(w, http.StatusConflict, "a Central API change or connection check is already in progress")
		} else {
			writeServerError(w, r, "could not check Central API connection", err)
		}
		return
	}
	h.audit(r, "check_crowdsec_capi", "crowdsec", "default", map[string]any{"state": result.State})
	writeJSON(w, http.StatusOK, result)
}

//apidoc:tag platform/maintenance
//apidoc:title Enroll In CrowdSec Console
//apidoc:order 18
func (h *Handler) EnrollCrowdSecConsole(w http.ResponseWriter, r *http.Request) {
	if h.capi == nil {
		writeError(w, http.StatusServiceUnavailable, "Central API management is unavailable")
		return
	}
	var request crowdsec.CAPIEnrollRequest
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8192))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&request) != nil {
		writeError(w, http.StatusBadRequest, "invalid console enrollment request")
		return
	}
	result, err := h.capi.Enroll(r.Context(), request)
	if err != nil {
		writeConsoleActionError(w, err)
		return
	}
	h.audit(r, "enroll_crowdsec_console", "crowdsec", "default", map[string]any{"mode": request.Mode, "overwrite": request.Overwrite, "state": result.State, "outcome": result.Outcome})
	writeJSON(w, http.StatusOK, result)
}

//apidoc:tag platform/maintenance
//apidoc:title Check CrowdSec Console Enrollment
//apidoc:order 19
func (h *Handler) CheckCrowdSecConsole(w http.ResponseWriter, r *http.Request) {
	if h.capi == nil {
		writeError(w, http.StatusServiceUnavailable, "Central API management is unavailable")
		return
	}
	result, err := h.capi.CheckEnrollment(r.Context())
	if err != nil {
		writeConsoleActionError(w, err)
		return
	}
	h.audit(r, "check_crowdsec_console", "crowdsec", "default", map[string]any{"state": result.State})
	writeJSON(w, http.StatusOK, result)
}
func writeConsoleActionError(w http.ResponseWriter, err error) {
	code := http.StatusBadRequest
	if errors.Is(err, crowdsec.ErrCAPIBusy) {
		code = http.StatusConflict
	}
	if errors.Is(err, crowdsec.ErrCAPIRateLimited) {
		code = http.StatusTooManyRequests
	}
	// The service returns actionable, redacted errors, never arbitrary CLI output.
	writeError(w, code, redact.Error(err.Error()))
}
