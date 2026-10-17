package handler

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/weiliang79/belune/internal/server/middleware"
	"github.com/weiliang79/belune/internal/store/generated"
)

type alertPreferencesResponse struct {
	DeployFailures        bool  `json:"deploy_failures"`
	DeploySuccess         bool  `json:"deploy_success"`
	BuildFailures         bool  `json:"build_failures"`
	QuotaThreshold        bool  `json:"quota_threshold"`
	QuotaThresholdPercent int32 `json:"quota_threshold_percent"`
}

func defaultAlertPreferences() alertPreferencesResponse {
	return alertPreferencesResponse{
		DeployFailures:        true,
		DeploySuccess:         true,
		BuildFailures:         true,
		QuotaThreshold:        true,
		QuotaThresholdPercent: 80,
	}
}

// GetAlertPreferences returns the current user's alert preferences,
// falling back to defaults when no row exists.
//
//apidoc:tag account/profile
//apidoc:title Get Alert Preferences
//apidoc:order 2
func (h *Handler) GetAlertPreferences(w http.ResponseWriter, r *http.Request) {
	userID := middleware.UserIDFromContext(r.Context())
	var uid pgtype.UUID
	if err := uid.Scan(userID); err != nil {
		writeError(w, http.StatusUnauthorized, "invalid user id")
		return
	}

	prefs, err := h.queries.GetAlertPreferences(r.Context(), uid)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeJSON(w, http.StatusOK, defaultAlertPreferences())
			return
		}
		writeServerError(w, r, "failed to fetch alert preferences", err)
		return
	}

	writeJSON(w, http.StatusOK, alertPreferencesResponse{
		DeployFailures:        prefs.DeployFailures,
		DeploySuccess:         prefs.DeploySuccess,
		BuildFailures:         prefs.BuildFailures,
		QuotaThreshold:        prefs.QuotaThreshold,
		QuotaThresholdPercent: prefs.QuotaThresholdPercent,
	})
}

type updateAlertPreferencesRequest struct {
	DeployFailures        bool  `json:"deploy_failures"`
	DeploySuccess         bool  `json:"deploy_success"`
	BuildFailures         bool  `json:"build_failures"`
	QuotaThreshold        bool  `json:"quota_threshold"`
	QuotaThresholdPercent int32 `json:"quota_threshold_percent"`
}

// UpdateAlertPreferences upserts the current user's alert preferences.
//
//apidoc:tag account/profile
//apidoc:title Update Alert Preferences
//apidoc:order 3
func (h *Handler) UpdateAlertPreferences(w http.ResponseWriter, r *http.Request) {
	userID := middleware.UserIDFromContext(r.Context())
	var uid pgtype.UUID
	if err := uid.Scan(userID); err != nil {
		writeError(w, http.StatusUnauthorized, "invalid user id")
		return
	}

	var req updateAlertPreferencesRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	if req.QuotaThresholdPercent < 1 || req.QuotaThresholdPercent > 100 {
		writeError(w, http.StatusBadRequest, "quota_threshold_percent must be between 1 and 100")
		return
	}

	prefs, err := h.queries.UpsertAlertPreferences(r.Context(), generated.UpsertAlertPreferencesParams{
		UserID:                uid,
		DeployFailures:        req.DeployFailures,
		DeploySuccess:         req.DeploySuccess,
		BuildFailures:         req.BuildFailures,
		QuotaThreshold:        req.QuotaThreshold,
		QuotaThresholdPercent: req.QuotaThresholdPercent,
	})
	if err != nil {
		writeServerError(w, r, "failed to update alert preferences", err)
		return
	}

	writeJSON(w, http.StatusOK, alertPreferencesResponse{
		DeployFailures:        prefs.DeployFailures,
		DeploySuccess:         prefs.DeploySuccess,
		BuildFailures:         prefs.BuildFailures,
		QuotaThreshold:        prefs.QuotaThreshold,
		QuotaThresholdPercent: prefs.QuotaThresholdPercent,
	})
}
