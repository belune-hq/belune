package handler

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/belune-hq/belune/internal/service"
	"github.com/belune-hq/belune/internal/store/generated"
)

type registryCredentialRequest struct {
	RegistryHost *string `json:"registry_host"`
	Username     *string `json:"username"`
	Password     *string `json:"password"`
}

type registryCredentialResponse struct {
	ID           string    `json:"id"`
	RegistryHost string    `json:"registry_host"`
	Username     string    `json:"username"`
	PasswordSet  bool      `json:"password_set"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

func registryCredentialView(row generated.RegistryCredential) registryCredentialResponse {
	return registryCredentialResponse{ID: uuidToString(row.ID), RegistryHost: row.RegistryHost, Username: row.Username, PasswordSet: true, CreatedAt: row.CreatedAt.Time, UpdatedAt: row.UpdatedAt.Time}
}

// ListRegistryCredentials returns install-wide registry credentials without secrets.
//
//apidoc:tag platform/registries
//apidoc:title List Registry Credentials
//apidoc:order 1
func (h *Handler) ListRegistryCredentials(w http.ResponseWriter, r *http.Request) {
	rows, err := h.queries.ListRegistryCredentials(r.Context())
	if err != nil {
		writeServerError(w, r, "failed to list registry credentials", err)
		return
	}
	result := make([]registryCredentialResponse, 0, len(rows))
	for _, row := range rows {
		result = append(result, registryCredentialResponse{ID: uuidToString(row.ID), RegistryHost: row.RegistryHost, Username: row.Username, PasswordSet: true, CreatedAt: row.CreatedAt.Time, UpdatedAt: row.UpdatedAt.Time})
	}
	writeJSON(w, http.StatusOK, result)
}

// CreateRegistryCredential stores a write-only password for one registry host.
//
//apidoc:tag platform/registries
//apidoc:title Create Registry Credential
//apidoc:order 2
func (h *Handler) CreateRegistryCredential(w http.ResponseWriter, r *http.Request) {
	if response, ok := h.saveRegistryCredential(w, r, false); ok {
		writeJSON(w, http.StatusCreated, response)
	}
}

// UpdateRegistryCredential updates only supplied fields; omitting password preserves it.
//
//apidoc:tag platform/registries
//apidoc:title Update Registry Credential
//apidoc:order 3
func (h *Handler) UpdateRegistryCredential(w http.ResponseWriter, r *http.Request) {
	if response, ok := h.saveRegistryCredential(w, r, true); ok {
		writeJSON(w, http.StatusOK, response)
	}
}

func (h *Handler) saveRegistryCredential(w http.ResponseWriter, r *http.Request, update bool) (registryCredentialResponse, bool) {
	var id pgtype.UUID
	if update {
		if err := id.Scan(chi.URLParam(r, "credentialId")); err != nil {
			writeError(w, http.StatusBadRequest, "invalid registry credential id")
			return registryCredentialResponse{}, false
		}
	}
	var request registryCredentialRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<17)).Decode(&request); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return registryCredentialResponse{}, false
	}
	row, err := service.NewRegistryCredentialService(h.queries, h.cfg.Keyring).Save(r.Context(), id, request.RegistryHost, request.Username, request.Password)
	if err != nil {
		var duplicate *pgconn.PgError
		switch {
		case errors.Is(err, service.ErrRegistryCredentialInvalid):
			writeError(w, http.StatusBadRequest, err.Error())
		case errors.Is(err, pgx.ErrNoRows):
			writeError(w, http.StatusNotFound, "registry credential not found")
		case errors.As(err, &duplicate) && duplicate.Code == "23505":
			writeError(w, http.StatusConflict, "a credential already exists for that registry host")
		default:
			writeServerError(w, r, "failed to save registry credential", err)
		}
		return registryCredentialResponse{}, false
	}
	action := "create_registry_credential"
	if update {
		action = "update_registry_credential"
	}
	h.audit(r, action, "registry_credential", uuidToString(row.ID), map[string]any{"registry_host": row.RegistryHost, "rotated": request.Password != nil})
	return registryCredentialView(row), true
}

// DeleteRegistryCredential removes a registry's stored credentials.
//
//apidoc:tag platform/registries
//apidoc:title Delete Registry Credential
//apidoc:order 4
func (h *Handler) DeleteRegistryCredential(w http.ResponseWriter, r *http.Request) {
	var id pgtype.UUID
	if err := id.Scan(chi.URLParam(r, "credentialId")); err != nil {
		writeError(w, http.StatusBadRequest, "invalid registry credential id")
		return
	}
	if _, err := h.queries.DeleteRegistryCredential(r.Context(), id); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeError(w, http.StatusNotFound, "registry credential not found")
		} else {
			writeServerError(w, r, "failed to delete registry credential", err)
		}
		return
	}
	h.audit(r, "delete_registry_credential", "registry_credential", uuidToString(id), nil)
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}
