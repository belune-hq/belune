package handler

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/belune-hq/belune/internal/service"
	"github.com/belune-hq/belune/internal/store"
	"github.com/belune-hq/belune/internal/store/generated"
)

// The key shape lives in the service so the REST replace and the MCP merge
// cannot disagree about what a valid name is.
var envKeyRegex = service.EnvKeyRegex

// preparedEnvVar is one validated, already-encrypted variable ready to be
// written.
type preparedEnvVar struct {
	key       string
	encrypted []byte
	isSecret  bool
}

// prepareEnvVars validates every submitted key and encrypts every value before
// anything is written, returning a client error message on the first problem.
//
// Doing all of this up front is the point: these endpoints replace the whole
// set, so a failure discovered halfway through would otherwise leave the stored
// variables partially rewritten — or, in the project endpoint's original form,
// already deleted. Nothing here touches the database.
//
// Blank keys are skipped rather than rejected: the editor submits an empty row
// for the "add variable" affordance.
//
// existing maps key -> currently-stored ciphertext. The list endpoint masks
// secret values as "••••••••", so a row the editor never touched carries that
// mask string, not the real value — encrypting it as submitted would overwrite
// the real secret with the literal mask. A row marked Unchanged is exempt: its
// existing ciphertext is reused verbatim instead of re-encrypting v.Value.
func prepareEnvVars(vars []envVarInput, existing map[string][]byte, encrypt func([]byte) ([]byte, error)) ([]preparedEnvVar, string) {
	out := make([]preparedEnvVar, 0, len(vars))
	seen := make(map[string]struct{}, len(vars))
	for _, v := range vars {
		if v.Key == "" {
			continue
		}
		if !envKeyRegex.MatchString(v.Key) {
			return nil, fmt.Sprintf("invalid env var key: %q", v.Key)
		}
		// A duplicate key would make the upsert order decide the winner, so
		// reject it instead of silently keeping the last one.
		if _, dup := seen[v.Key]; dup {
			return nil, fmt.Sprintf("duplicate env var key: %q", v.Key)
		}
		seen[v.Key] = struct{}{}

		if v.Unchanged {
			if encrypted, ok := existing[v.Key]; ok {
				out = append(out, preparedEnvVar{key: v.Key, encrypted: encrypted, isSecret: v.IsSecret})
				continue
			}
		}

		encrypted, err := encrypt([]byte(v.Value))
		if err != nil {
			return nil, "failed to encrypt value"
		}
		out = append(out, preparedEnvVar{key: v.Key, encrypted: encrypted, isSecret: v.IsSecret})
	}
	return out, ""
}

// envChangeDetails names the keys a whole-set replace created, rewrote and
// removed, for the audit log. Key names only: the log is the record of what
// happened to a secret set, and must not become a second copy of it.
//
// "updated" means the row was submitted with a value and so was re-encrypted;
// an Unchanged row reuses its stored ciphertext verbatim and is listed in
// neither. Removed keys matter most — a delete reaches the database only as
// "absent from the replacement set", so without this nothing would record it.
func envChangeDetails(existing map[string][]byte, prepared []preparedEnvVar) map[string]any {
	created, updated, removed := []string{}, []string{}, []string{}
	kept := make(map[string]struct{}, len(prepared))
	for _, p := range prepared {
		kept[p.key] = struct{}{}
		old, had := existing[p.key]
		switch {
		case !had:
			created = append(created, p.key)
		case !bytes.Equal(old, p.encrypted):
			updated = append(updated, p.key)
		}
	}
	for k := range existing {
		if _, ok := kept[k]; !ok {
			removed = append(removed, k)
		}
	}
	sort.Strings(created)
	sort.Strings(updated)
	sort.Strings(removed)
	return map[string]any{"created": created, "updated": updated, "removed": removed}
}

// keysOf returns the keys to retain, for the delete-what-was-not-submitted step.
func keysOf(prepared []preparedEnvVar) []string {
	keys := make([]string, 0, len(prepared))
	for _, p := range prepared {
		keys = append(keys, p.key)
	}
	return keys
}

type envVarResponse struct {
	ID        pgtype.UUID `json:"id"`
	Key       string      `json:"key"`
	Value     string      `json:"value,omitempty"`
	IsSecret  bool        `json:"is_secret"`
	CreatedAt string      `json:"created_at"`
	UpdatedAt string      `json:"updated_at"`
}

//apidoc:tag applications/env
//apidoc:title Get Variables
//apidoc:order 1
func (h *Handler) ListEnvVars(w http.ResponseWriter, r *http.Request) {
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

	envVars, err := h.queries.ListEnvVarsByApplication(r.Context(), applicationUUID)
	if err != nil {
		writeServerError(w, r, "failed to list env vars", err)
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
		// RevealEnvVar instead.
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

// RevealEnvVar returns the decrypted value of a single env var. It exists so a
// secret (masked in the list response) can be loaded into the editor without
// forcing a rewrite of every row. The reveal is audited because it deliberately
// hands back plaintext the UI otherwise hides.
//
//apidoc:tag applications/env
func (h *Handler) RevealEnvVar(w http.ResponseWriter, r *http.Request) {
	applicationID := chi.URLParam(r, "applicationId")
	var applicationUUID pgtype.UUID
	if err := applicationUUID.Scan(applicationID); err != nil {
		writeError(w, http.StatusBadRequest, "invalid application id")
		return
	}

	envVarID := chi.URLParam(r, "envVarId")
	var envVarUUID pgtype.UUID
	if err := envVarUUID.Scan(envVarID); err != nil {
		writeError(w, http.StatusBadRequest, "invalid env var id")
		return
	}

	if !h.canAccessApplication(r, applicationUUID) {
		writeError(w, http.StatusForbidden, "access denied")
		return
	}

	ev, err := h.queries.GetEnvVar(r.Context(), envVarUUID)
	if err != nil {
		writeError(w, http.StatusNotFound, "env var not found")
		return
	}
	if ev.ApplicationID != applicationUUID {
		writeError(w, http.StatusNotFound, "env var not found")
		return
	}

	decrypted, err := h.cfg.Keyring.Decrypt(ev.ValueEncrypted)
	if err != nil {
		writeServerError(w, r, "failed to decrypt value", err)
		return
	}

	h.audit(r, "reveal_env_var", "env_var", envVarID, map[string]any{
		"application_id": applicationID,
		"key":            ev.Key,
	})

	writeJSON(w, http.StatusOK, map[string]string{"value": string(decrypted)})
}

type updateEnvVarsRequest struct {
	Vars []envVarInput `json:"vars"`
}

type envVarInput struct {
	Key      string `json:"key"`
	Value    string `json:"value"`
	IsSecret bool   `json:"is_secret"`
	// Unchanged marks a secret row the editor never touched: its value is
	// still the "••••••••" mask from the list response, not the real secret,
	// so prepareEnvVars must reuse the stored ciphertext instead of
	// encrypting this value.
	Unchanged bool `json:"unchanged,omitempty"`
}

//apidoc:tag applications/env
//apidoc:title Update Variables
//apidoc:order 2
func (h *Handler) UpdateEnvVars(w http.ResponseWriter, r *http.Request) {
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

	var req updateEnvVarsRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	existingVars, err := h.queries.ListEnvVarsByApplication(r.Context(), applicationUUID)
	if err != nil {
		writeServerError(w, r, "failed to load existing env vars", err)
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

	// The request carries the complete set, so anything missing from it was
	// deleted in the editor. Upserting alone left those rows behind, and the
	// deploy kept injecting them into the container — removing a variable in the
	// UI appeared to work but changed nothing.
	//
	// Upsert and delete run in one transaction so the stored set is never
	// observed half-applied: a failure partway through leaves the previous set
	// intact rather than a mixture of old and new.
	if err := store.WithTx(r.Context(), h.db, func(q *generated.Queries) error {
		for _, p := range prepared {
			if _, err := q.UpsertEnvVar(r.Context(), generated.UpsertEnvVarParams{
				ApplicationID:  applicationUUID,
				Key:            p.key,
				ValueEncrypted: p.encrypted,
				IsSecret:       p.isSecret,
			}); err != nil {
				return err
			}
		}
		return q.DeleteEnvVarsNotIn(r.Context(), generated.DeleteEnvVarsNotInParams{
			ApplicationID: applicationUUID,
			Keys:          keysOf(prepared),
		})
	}); err != nil {
		writeServerError(w, r, "failed to save env vars", err)
		return
	}

	h.markConfigChanged(r.Context(), applicationUUID)

	h.audit(r, "update_env_vars", "env_var", applicationID, envChangeDetails(existing, prepared))

	writeJSON(w, http.StatusOK, map[string]string{"status": "updated"})
}
