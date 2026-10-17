package handler

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/weiliang79/belune/internal/service"
	"github.com/weiliang79/belune/internal/store/generated"
)

type backupDestinationResponse struct {
	ID        string    `json:"id"`
	ProjectID string    `json:"project_id"`
	Name      string    `json:"name"`
	Provider  string    `json:"provider"`
	Endpoint  string    `json:"endpoint"`
	Region    string    `json:"region"`
	Bucket    string    `json:"bucket"`
	Prefix    string    `json:"prefix"`
	UseSSL    bool      `json:"use_ssl"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

func toDestinationListResponse(d generated.ListBackupDestinationsByProjectRow) backupDestinationResponse {
	return backupDestinationResponse{
		ID:        uuidToString(d.ID),
		ProjectID: uuidToString(d.ProjectID),
		Name:      d.Name,
		Provider:  d.Provider,
		Endpoint:  d.Endpoint,
		Region:    d.Region,
		Bucket:    d.Bucket,
		Prefix:    d.Prefix,
		UseSSL:    d.UseSsl,
		CreatedAt: d.CreatedAt.Time,
		UpdatedAt: d.UpdatedAt.Time,
	}
}

func toDestinationResponse(d generated.BackupDestination) backupDestinationResponse {
	return backupDestinationResponse{
		ID:        uuidToString(d.ID),
		ProjectID: uuidToString(d.ProjectID),
		Name:      d.Name,
		Provider:  d.Provider,
		Endpoint:  d.Endpoint,
		Region:    d.Region,
		Bucket:    d.Bucket,
		Prefix:    d.Prefix,
		UseSSL:    d.UseSsl,
		CreatedAt: d.CreatedAt.Time,
		UpdatedAt: d.UpdatedAt.Time,
	}
}

type backupDestinationRequest struct {
	// ID is only used by the ad-hoc test endpoint to fall back to a saved
	// destination's stored credentials when the form secret is left blank.
	ID        string `json:"id"`
	Name      string `json:"name"`
	Provider  string `json:"provider"`
	Endpoint  string `json:"endpoint"`
	Region    string `json:"region"`
	Bucket    string `json:"bucket"`
	Prefix    string `json:"prefix"`
	UseSSL    *bool  `json:"use_ssl"`
	AccessKey string `json:"access_key"`
	SecretKey string `json:"secret_key"`
}

var validBackupProviders = map[string]bool{
	"s3": true, "r2": true, "b2": true, "wasabi": true, "minio": true, "other": true, "local": true,
}

// toSaveParams validates the request and builds save params. On update, empty
// credentials preserve the stored secret (creds left nil).
//
// provider == "local" means "keep the archive on-host, don't upload
// anywhere" — it has no endpoint/bucket/region/prefix/credentials, so those
// fields are validated and stored as their zero values regardless of what the
// request sent (a stale form value from switching providers must not leak
// into a saved local destination).
func (req *backupDestinationRequest) toSaveParams(projectID pgtype.UUID, requireCreds bool) (service.SaveBackupDestinationParams, string) {
	name := strings.TrimSpace(req.Name)
	if name == "" {
		return service.SaveBackupDestinationParams{}, "name is required"
	}
	provider := req.Provider
	if provider == "" {
		provider = "s3"
	}
	if !validBackupProviders[provider] {
		return service.SaveBackupDestinationParams{}, "invalid provider"
	}

	if provider == "local" {
		return service.SaveBackupDestinationParams{
			ProjectID: projectID,
			Name:      name,
			Provider:  provider,
		}, ""
	}

	bucket := strings.TrimSpace(req.Bucket)
	if bucket == "" {
		return service.SaveBackupDestinationParams{}, "bucket is required"
	}
	region := strings.TrimSpace(req.Region)
	if region == "" {
		region = "us-east-1"
	}
	useSSL := true
	if req.UseSSL != nil {
		useSSL = *req.UseSSL
	}

	p := service.SaveBackupDestinationParams{
		ProjectID: projectID,
		Name:      name,
		Provider:  provider,
		Endpoint:  strings.TrimSpace(req.Endpoint),
		Region:    region,
		Bucket:    bucket,
		Prefix:    strings.Trim(strings.TrimSpace(req.Prefix), "/"),
		UseSSL:    useSSL,
	}

	if req.AccessKey != "" || req.SecretKey != "" {
		if req.AccessKey == "" || req.SecretKey == "" {
			return service.SaveBackupDestinationParams{}, "access_key and secret_key must be provided together"
		}
		p.Credentials = &service.DestinationCredentials{AccessKey: req.AccessKey, SecretKey: req.SecretKey}
	} else if requireCreds {
		return service.SaveBackupDestinationParams{}, "access_key and secret_key are required"
	}
	return p, ""
}

// projectFromPath parses and authorizes the {projectId} path param.
func (h *Handler) projectFromPath(w http.ResponseWriter, r *http.Request) (pgtype.UUID, bool) {
	var projectUUID pgtype.UUID
	if err := projectUUID.Scan(chi.URLParam(r, "projectId")); err != nil {
		writeError(w, http.StatusBadRequest, "invalid project id")
		return projectUUID, false
	}
	if !h.canAccessProject(r, projectUUID) {
		writeError(w, http.StatusForbidden, "access denied")
		return projectUUID, false
	}
	return projectUUID, true
}

// destinationInProject parses {destId}, loads it, and verifies it belongs to the
// path project. Returns the row and ok.
func (h *Handler) destinationInProject(w http.ResponseWriter, r *http.Request, projectID pgtype.UUID) (generated.BackupDestination, bool) {
	var destUUID pgtype.UUID
	if err := destUUID.Scan(chi.URLParam(r, "destId")); err != nil {
		writeError(w, http.StatusBadRequest, "invalid destination id")
		return generated.BackupDestination{}, false
	}
	dest, err := h.backupDestSvc.Get(r.Context(), destUUID)
	if err != nil {
		writeError(w, http.StatusNotFound, "destination not found")
		return generated.BackupDestination{}, false
	}
	if dest.ProjectID != projectID {
		writeError(w, http.StatusNotFound, "destination not found")
		return generated.BackupDestination{}, false
	}
	return dest, true
}

// ListBackupDestinations returns a project's backup destinations (no secrets).
//
//apidoc:tag projects/backup-destinations
//apidoc:title Get Destinations
//apidoc:order 1
func (h *Handler) ListBackupDestinations(w http.ResponseWriter, r *http.Request) {
	projectUUID, ok := h.projectFromPath(w, r)
	if !ok {
		return
	}
	rows, err := h.backupDestSvc.ListByProject(r.Context(), projectUUID)
	if err != nil {
		writeServerError(w, r, "failed to list destinations", err)
		return
	}
	resp := make([]backupDestinationResponse, 0, len(rows))
	for _, d := range rows {
		resp = append(resp, toDestinationListResponse(d))
	}
	writeJSON(w, http.StatusOK, resp)
}

// CreateBackupDestination creates a project backup destination.
//
//apidoc:tag projects/backup-destinations
//apidoc:title Create Destination
//apidoc:order 2
func (h *Handler) CreateBackupDestination(w http.ResponseWriter, r *http.Request) {
	projectUUID, ok := h.projectFromPath(w, r)
	if !ok {
		return
	}
	var req backupDestinationRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	params, msg := req.toSaveParams(projectUUID, true)
	if msg != "" {
		writeError(w, http.StatusBadRequest, msg)
		return
	}
	dest, err := h.backupDestSvc.Create(r.Context(), params)
	if err != nil {
		writeServerError(w, r, "failed to create destination", err)
		return
	}
	h.audit(r, "create_backup_destination", "project", uuidToString(projectUUID), map[string]any{"destination_id": uuidToString(dest.ID)})
	writeJSON(w, http.StatusCreated, toDestinationResponse(dest))
}

// UpdateBackupDestination updates a destination; empty creds preserve the secret.
//
//apidoc:tag projects/backup-destinations
//apidoc:title Update Destination
//apidoc:order 3
func (h *Handler) UpdateBackupDestination(w http.ResponseWriter, r *http.Request) {
	projectUUID, ok := h.projectFromPath(w, r)
	if !ok {
		return
	}
	dest, ok := h.destinationInProject(w, r, projectUUID)
	if !ok {
		return
	}
	var req backupDestinationRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	params, msg := req.toSaveParams(projectUUID, false)
	if msg != "" {
		writeError(w, http.StatusBadRequest, msg)
		return
	}
	updated, err := h.backupDestSvc.Update(r.Context(), dest.ID, params)
	if err != nil {
		// Name the count: without it a refusal to save reads as a bug rather
		// than as protection for backups already written here.
		var locked service.ErrDestinationIdentityLocked
		if errors.As(err, &locked) {
			writeError(w, http.StatusConflict, fmt.Sprintf(
				"%d backup(s) are stored in this destination, so the settings that decide where its objects live can no longer be changed. Credentials can still be rotated, or create a new destination for the new location.",
				locked.BackupCount))
			return
		}
		writeServerError(w, r, "failed to update destination", err)
		return
	}
	h.audit(r, "update_backup_destination", "project", uuidToString(projectUUID), map[string]any{"destination_id": uuidToString(dest.ID)})
	writeJSON(w, http.StatusOK, toDestinationResponse(updated))
}

// DeleteBackupDestination removes a destination. The DB refuses while a backup
// config references it (ON DELETE RESTRICT) or while any backup is recorded as
// living in it (backup_locations) — surfaced as 409 naming whichever it is.
//
//apidoc:tag projects/backup-destinations
//apidoc:title Delete Destination
//apidoc:order 4
func (h *Handler) DeleteBackupDestination(w http.ResponseWriter, r *http.Request) {
	projectUUID, ok := h.projectFromPath(w, r)
	if !ok {
		return
	}
	dest, ok := h.destinationInProject(w, r, projectUUID)
	if !ok {
		return
	}
	if err := h.backupDestSvc.Delete(r.Context(), dest.ID); err != nil {
		// Recorded backups are now the more common cause, and "in use by a
		// backup configuration" would send the operator to delete a schedule
		// that is not the thing holding the destination.
		msg := "destination is in use by a backup configuration"
		if n, cErr := h.backupDestSvc.CountBackupsStored(r.Context(), dest.ID); cErr == nil && n > 0 {
			msg = fmt.Sprintf(
				"%d backup(s) are stored in this destination. Delete those backups (or the resources they belong to) before removing it.", n)
		}
		writeError(w, http.StatusConflict, msg)
		return
	}
	h.audit(r, "delete_backup_destination", "project", uuidToString(projectUUID), map[string]any{"destination_id": uuidToString(dest.ID)})
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

// TestBackupDestination verifies connectivity + bucket access for a destination.
//
//apidoc:tag projects/backup-destinations
//apidoc:title Test Destination
//apidoc:order 5
func (h *Handler) TestBackupDestination(w http.ResponseWriter, r *http.Request) {
	projectUUID, ok := h.projectFromPath(w, r)
	if !ok {
		return
	}
	dest, ok := h.destinationInProject(w, r, projectUUID)
	if !ok {
		return
	}
	if err := h.backupDestSvc.Test(r.Context(), dest.ID); err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// TestBackupDestinationParams tests ad-hoc connection params from the create/edit
// form (before saving). On edit, a blank secret falls back to the stored one via
// the optional body id (verified to belong to this project).
//
//apidoc:tag projects/backup-destinations
//apidoc:title Test Configs
//apidoc:order 6
func (h *Handler) TestBackupDestinationParams(w http.ResponseWriter, r *http.Request) {
	projectUUID, ok := h.projectFromPath(w, r)
	if !ok {
		return
	}
	var req backupDestinationRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Provider != "local" && strings.TrimSpace(req.Bucket) == "" {
		writeError(w, http.StatusBadRequest, "bucket is required")
		return
	}

	params := service.SaveBackupDestinationParams{
		Provider: req.Provider,
		Endpoint: strings.TrimSpace(req.Endpoint),
		Region:   strings.TrimSpace(req.Region),
		Bucket:   strings.TrimSpace(req.Bucket),
		Prefix:   strings.Trim(strings.TrimSpace(req.Prefix), "/"),
	}
	if req.UseSSL != nil {
		params.UseSSL = *req.UseSSL
	} else {
		params.UseSSL = true
	}
	if req.AccessKey != "" || req.SecretKey != "" {
		params.Credentials = &service.DestinationCredentials{
			AccessKey: req.AccessKey,
			SecretKey: req.SecretKey,
		}
	}

	// Optional fallback to a saved destination's stored credentials.
	fallbackID := pgtype.UUID{}
	if req.ID != "" {
		if err := fallbackID.Scan(req.ID); err == nil {
			if existing, err := h.backupDestSvc.Get(r.Context(), fallbackID); err != nil || existing.ProjectID != projectUUID {
				fallbackID = pgtype.UUID{} // not ours — ignore
			}
		} else {
			fallbackID = pgtype.UUID{}
		}
	}

	if err := h.backupDestSvc.TestConnection(r.Context(), params, fallbackID); err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}
