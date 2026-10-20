package handler

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/belune-hq/belune/internal/store"
	"github.com/belune-hq/belune/internal/store/generated"
)

//apidoc:tag projects/env
//apidoc:title Get Variables
//apidoc:order 1
func (h *Handler) ListProjectEnvVars(w http.ResponseWriter, r *http.Request) {
	projectID := chi.URLParam(r, "projectId")
	var projectUUID pgtype.UUID
	if err := projectUUID.Scan(projectID); err != nil {
		writeError(w, http.StatusBadRequest, "invalid project id")
		return
	}

	if !h.canAccessProject(r, projectUUID) {
		writeError(w, http.StatusForbidden, "access denied")
		return
	}

	envVars, err := h.queries.ListProjectEnvVars(r.Context(), projectUUID)
	if err != nil {
		writeServerError(w, r, "failed to list project env vars", err)
		return
	}

	result := make([]envVarResponse, 0, len(envVars))
	for _, ev := range envVars {
		resp := envVarResponse{
			ID:        ev.ID,
			Key:       ev.Key,
			IsSecret:  ev.IsSecret,
			CreatedAt: ev.CreatedAt.Time.Format(time.RFC3339),
			UpdatedAt: ev.UpdatedAt.Time.Format(time.RFC3339),
		}

		// Decrypt and show non-secret values. A secret's value is never sent by
		// this endpoint, real or masked — the editor fetches it on demand via
		// RevealProjectEnvVar instead.
		if !ev.IsSecret {
			decrypted, err := h.cfg.Keyring.Decrypt(ev.ValueEncrypted)
			if err == nil {
				resp.Value = string(decrypted)
			}
		}

		result = append(result, resp)
	}

	writeJSON(w, http.StatusOK, result)
}

// RevealProjectEnvVar returns the decrypted value of a single project-level
// env var. Project counterpart of RevealEnvVar — an inherited secret revealed
// from an application's env page hits this endpoint, audited as a
// project-level reveal.
//
//apidoc:tag projects/env
func (h *Handler) RevealProjectEnvVar(w http.ResponseWriter, r *http.Request) {
	projectID := chi.URLParam(r, "projectId")
	var projectUUID pgtype.UUID
	if err := projectUUID.Scan(projectID); err != nil {
		writeError(w, http.StatusBadRequest, "invalid project id")
		return
	}

	envVarID := chi.URLParam(r, "envVarId")
	var envVarUUID pgtype.UUID
	if err := envVarUUID.Scan(envVarID); err != nil {
		writeError(w, http.StatusBadRequest, "invalid env var id")
		return
	}

	if !h.canAccessProject(r, projectUUID) {
		writeError(w, http.StatusForbidden, "access denied")
		return
	}

	ev, err := h.queries.GetProjectEnvVar(r.Context(), envVarUUID)
	if err != nil {
		writeError(w, http.StatusNotFound, "env var not found")
		return
	}
	if ev.ProjectID != projectUUID {
		writeError(w, http.StatusNotFound, "env var not found")
		return
	}

	decrypted, err := h.cfg.Keyring.Decrypt(ev.ValueEncrypted)
	if err != nil {
		writeServerError(w, r, "failed to decrypt value", err)
		return
	}

	h.audit(r, "reveal_env_var", "project_env_var", envVarID, map[string]any{
		"project_id": projectID,
		"key":        ev.Key,
	})

	writeJSON(w, http.StatusOK, map[string]string{"value": string(decrypted)})
}

//apidoc:tag projects/env
//apidoc:title Update Variables
//apidoc:order 2
func (h *Handler) UpdateProjectEnvVars(w http.ResponseWriter, r *http.Request) {
	projectID := chi.URLParam(r, "projectId")
	var projectUUID pgtype.UUID
	if err := projectUUID.Scan(projectID); err != nil {
		writeError(w, http.StatusBadRequest, "invalid project id")
		return
	}

	if !h.canAccessProject(r, projectUUID) {
		writeError(w, http.StatusForbidden, "access denied")
		return
	}

	var req updateEnvVarsRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	existingVars, err := h.queries.ListProjectEnvVars(r.Context(), projectUUID)
	if err != nil {
		writeServerError(w, r, "failed to load existing project env vars", err)
		return
	}
	existing := make(map[string][]byte, len(existingVars))
	for _, ev := range existingVars {
		existing[ev.Key] = ev.ValueEncrypted
	}

	prepared, errMsg := prepareEnvVars(req.Vars, existing, h.cfg.Keyring.Encrypt)
	if errMsg != "" {
		writeError(w, http.StatusBadRequest, errMsg)
		return
	}

	// This used to delete every variable first and validate afterwards, so a
	// single bad key — or a failure anywhere in the loop — returned an error
	// having already wiped the project's variables, with no transaction to undo
	// it. Validation and encryption now happen before any write, and the
	// replacement runs in one transaction.
	if err := store.WithTx(r.Context(), h.db, func(q *generated.Queries) error {
		for _, p := range prepared {
			if _, err := q.UpsertProjectEnvVar(r.Context(), generated.UpsertProjectEnvVarParams{
				ProjectID:      projectUUID,
				Key:            p.key,
				ValueEncrypted: p.encrypted,
				IsSecret:       p.isSecret,
			}); err != nil {
				return err
			}
		}
		return q.DeleteProjectEnvVarsNotIn(r.Context(), generated.DeleteProjectEnvVarsNotInParams{
			ProjectID: projectUUID,
			Keys:      keysOf(prepared),
		})
	}); err != nil {
		writeServerError(w, r, "failed to save project env vars", err)
		return
	}

	h.audit(r, "update_env_vars", "project_env_var", projectID, envChangeDetails(existing, prepared))

	writeJSON(w, http.StatusOK, map[string]string{"status": "updated"})
}
