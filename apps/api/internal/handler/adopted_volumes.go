package handler

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/belune-hq/belune/internal/naming"
	"github.com/belune-hq/belune/internal/service"
	"github.com/belune-hq/belune/internal/store"
	"github.com/belune-hq/belune/internal/store/generated"
)

type adoptVolumeRequest struct {
	Name         string `json:"name"`
	MountPath    string `json:"mount_path"`
	ExternalName string `json:"external_name"`
}

//apidoc:tag applications/volumes-backups
//apidoc:title Adopt Existing Volume
//apidoc:order 3
func (h *Handler) AdoptApplicationVolume(w http.ResponseWriter, r *http.Request) {
	var appID pgtype.UUID
	if err := appID.Scan(chi.URLParam(r, "applicationId")); err != nil {
		writeError(w, http.StatusBadRequest, "invalid application id")
		return
	}
	if !h.canAccessApplication(r, appID) {
		writeError(w, http.StatusForbidden, "access denied")
		return
	}
	var req adoptVolumeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	name := naming.Slugify(req.Name)
	if name == "" || len(name) > 40 {
		writeError(w, http.StatusBadRequest, "name must contain alphanumeric characters and be at most 40 characters")
		return
	}
	if err := validateMountPath(req.MountPath); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	vol, err := h.appService.AdoptVolume(r.Context(), generated.CreateApplicationVolumeParams{
		ApplicationID: appID, Name: name, MountPath: req.MountPath, ExternalName: pgtype.Text{String: req.ExternalName, Valid: true},
	})
	if err != nil {
		switch {
		case errors.Is(err, store.ErrVolumeClaimsBusy):
			writeError(w, http.StatusServiceUnavailable, store.ErrVolumeClaimsBusy.Error())
		case errors.Is(err, service.ErrExternalVolumeMissing):
			writeError(w, http.StatusNotFound, service.ErrExternalVolumeMissing.Error())
		case errors.Is(err, service.ErrExternalVolumeClaimed):
			writeError(w, http.StatusConflict, service.ErrExternalVolumeClaimed.Error())
		case errors.Is(err, service.ErrExternalVolumeConflict):
			writeError(w, http.StatusConflict, service.ErrExternalVolumeConflict.Error())
		case errors.Is(err, service.ErrExternalVolumeConsumer):
			writeError(w, http.StatusConflict, service.ErrExternalVolumeConsumer.Error())
		case errors.Is(err, service.ErrExternalVolumeInUse):
			writeError(w, http.StatusConflict, service.ErrExternalVolumeInUse.Error())
		case errors.Is(err, service.ErrExternalVolumeManaged):
			writeError(w, http.StatusBadRequest, service.ErrExternalVolumeManaged.Error())
		case errors.Is(err, service.ErrExternalVolumeLocal):
			writeError(w, http.StatusBadRequest, service.ErrExternalVolumeLocal.Error())
		case errors.Is(err, service.ErrExternalVolumeName):
			writeError(w, http.StatusBadRequest, service.ErrExternalVolumeName.Error())
		default:
			writeServerError(w, r, "failed to adopt volume", err)
		}
		return
	}
	h.markConfigChanged(r.Context(), appID)
	h.audit(r, "adopt_volume", "application_volume", uuidToString(vol.ID), map[string]any{"application_id": uuidToString(appID), "external_name": req.ExternalName, "mount_path": vol.MountPath})
	writeJSON(w, http.StatusCreated, applicationVolumeResponse{ID: uuidToString(vol.ID), Name: vol.Name, MountPath: vol.MountPath, ExternalName: nullableVolumeName(vol.ExternalName), CreatedAt: vol.CreatedAt.Time.Format("2006-01-02T15:04:05Z07:00")})
}
