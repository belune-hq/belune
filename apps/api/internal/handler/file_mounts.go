package handler

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"path"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/weiliang79/belune/internal/store/generated"
)

// fileMountReservedPrefixes reject a mount under a kernel/pseudo filesystem.
var fileMountReservedPrefixes = []string{"/proc/", "/sys/", "/dev/"}

// validateFilePath enforces that a file-mount path is an absolute, clean,
// in-container FILE path. Unlike validateMountPath (volumes), it deliberately
// ALLOWS deep paths under system directories — a config file legitimately lands
// at e.g. /etc/nginx/nginx.conf — but it must look like a file (have a
// non-empty basename), never be a bare top-level path, and never sit under a
// kernel/pseudo filesystem. Pure + package-level for unit testing.
func validateFilePath(p string) error {
	if p == "" {
		return errors.New("mount_path is required")
	}
	if !strings.HasPrefix(p, "/") {
		return errors.New("mount_path must be an absolute path (start with /)")
	}
	if path.Clean(p) != p {
		return errors.New("mount_path must be a clean path (no '.', '..', '//', or trailing slash)")
	}
	// Must point at a file: a real basename and at least one directory segment
	// (i.e. not a bare "/foo" top-level entry, and not "/").
	dir, base := path.Split(p)
	if base == "" {
		return errors.New("mount_path must be a file path, not a directory")
	}
	if dir == "/" {
		return errors.New("mount_path must be nested under a directory, e.g. /etc/app/config.yaml")
	}
	for _, prefix := range fileMountReservedPrefixes {
		if strings.HasPrefix(p, prefix) {
			return fmt.Errorf("mount_path %q is under a reserved path", p)
		}
	}
	if len(p) > 500 {
		return errors.New("mount_path is too long")
	}
	return nil
}

// validateFileMode checks a 3-4 digit octal mode string (e.g. "0644", "600").
// setuid/setgid/sticky bits are rejected: a user-supplied config file must never
// carry them.
func validateFileMode(mode string) (string, error) {
	if mode == "" {
		return "0644", nil
	}
	v, err := strconv.ParseUint(mode, 8, 32)
	if err != nil {
		return "", fmt.Errorf("invalid file_mode %q: must be octal like 0644", mode)
	}
	if v > 0o777 {
		return "", fmt.Errorf("invalid file_mode %q: setuid/setgid/sticky bits are not allowed", mode)
	}
	return mode, nil
}

type fileMountResponse struct {
	ID        string `json:"id"`
	MountPath string `json:"mount_path"`
	Content   string `json:"content,omitempty"`
	IsSecret  bool   `json:"is_secret"`
	FileMode  string `json:"file_mode"`
	// Set true when the mount holds secret content that is masked in this
	// response — the client shows "set" without the value.
	ContentMasked bool   `json:"content_masked"`
	CreatedAt     string `json:"created_at"`
}

// toFileMountResponse decrypts + masks a row for the API. Secret content is
// never returned; non-secret content is decrypted so it can be viewed/edited.
func (h *Handler) toFileMountResponse(fm generated.ApplicationFileMount) fileMountResponse {
	resp := fileMountResponse{
		ID:        uuidToString(fm.ID),
		MountPath: fm.MountPath,
		IsSecret:  fm.IsSecret,
		FileMode:  fm.FileMode,
		CreatedAt: fm.CreatedAt.Time.Format("2006-01-02T15:04:05Z07:00"),
	}
	if fm.IsSecret {
		resp.ContentMasked = true
		return resp
	}
	if decrypted, err := h.cfg.Keyring.Decrypt(fm.ContentEncrypted); err == nil {
		resp.Content = string(decrypted)
	}
	return resp
}

// RevealFileMount returns the decrypted content of a single file mount. It
// exists so a secret mount (whose content is masked in list/get) can be loaded
// into the editor for in-place editing instead of forcing a full rewrite. The
// reveal is audited because it deliberately hands back plaintext the UI
// otherwise hides.
//
//apidoc:tag applications/file-mounts
func (h *Handler) RevealFileMount(w http.ResponseWriter, r *http.Request) {
	applicationID := chi.URLParam(r, "applicationId")
	var applicationUUID pgtype.UUID
	if err := applicationUUID.Scan(applicationID); err != nil {
		writeError(w, http.StatusBadRequest, "invalid application id")
		return
	}

	fileMountID := chi.URLParam(r, "fileMountId")
	var fileMountUUID pgtype.UUID
	if err := fileMountUUID.Scan(fileMountID); err != nil {
		writeError(w, http.StatusBadRequest, "invalid file mount id")
		return
	}

	if !h.canAccessApplication(r, applicationUUID) {
		writeError(w, http.StatusForbidden, "access denied")
		return
	}

	fm, err := h.queries.GetApplicationFileMount(r.Context(), fileMountUUID)
	if err != nil {
		writeError(w, http.StatusNotFound, "file mount not found")
		return
	}
	if fm.ApplicationID != applicationUUID {
		writeError(w, http.StatusNotFound, "file mount not found")
		return
	}

	content, err := h.cfg.Keyring.Decrypt(fm.ContentEncrypted)
	if err != nil {
		writeServerError(w, r, "failed to decrypt content", err)
		return
	}

	h.audit(r, "reveal_file_mount", "application_file_mount", fileMountID, map[string]any{
		"application_id": applicationID,
		"mount_path":     fm.MountPath,
	})

	writeJSON(w, http.StatusOK, map[string]string{"content": string(content)})
}

//apidoc:tag applications/file-mounts
//apidoc:title Get Mounts
//apidoc:order 1
func (h *Handler) ListFileMounts(w http.ResponseWriter, r *http.Request) {
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

	mounts, err := h.queries.ListApplicationFileMounts(r.Context(), applicationUUID)
	if err != nil {
		writeServerError(w, r, "failed to list file mounts", err)
		return
	}

	out := make([]fileMountResponse, 0, len(mounts))
	for _, fm := range mounts {
		out = append(out, h.toFileMountResponse(fm))
	}
	writeJSON(w, http.StatusOK, out)
}

type createFileMountRequest struct {
	MountPath string `json:"mount_path"`
	Content   string `json:"content"`
	IsSecret  bool   `json:"is_secret"`
	FileMode  string `json:"file_mode"`
}

//apidoc:tag applications/file-mounts
//apidoc:title Create Mount
//apidoc:order 2
func (h *Handler) CreateFileMount(w http.ResponseWriter, r *http.Request) {
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

	var req createFileMountRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	if err := validateFilePath(req.MountPath); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	mode, err := validateFileMode(req.FileMode)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	encrypted, err := h.cfg.Keyring.Encrypt([]byte(req.Content))
	if err != nil {
		writeServerError(w, r, "failed to encrypt content", err)
		return
	}

	fm, err := h.queries.CreateApplicationFileMount(r.Context(), generated.CreateApplicationFileMountParams{
		ApplicationID:    applicationUUID,
		MountPath:        req.MountPath,
		ContentEncrypted: encrypted,
		IsSecret:         req.IsSecret,
		FileMode:         mode,
	})
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			writeError(w, http.StatusConflict, "a file mount already exists at this path")
			return
		}
		writeServerError(w, r, "failed to create file mount", err)
		return
	}

	h.markConfigChanged(r.Context(), applicationUUID)

	h.audit(r, "create_file_mount", "application_file_mount", uuidToString(fm.ID), map[string]any{
		"application_id": applicationID,
		"mount_path":     req.MountPath,
		"is_secret":      req.IsSecret,
	})
	writeJSON(w, http.StatusCreated, h.toFileMountResponse(fm))
}

type updateFileMountRequest struct {
	// Content is optional on update: when nil, the stored content is kept (so a
	// secret can be edited without re-sending it). When non-nil (including ""),
	// it replaces the stored content.
	Content  *string `json:"content"`
	IsSecret bool    `json:"is_secret"`
	FileMode string  `json:"file_mode"`
}

//apidoc:tag applications/file-mounts
//apidoc:title Update Mount
//apidoc:order 3
func (h *Handler) UpdateFileMount(w http.ResponseWriter, r *http.Request) {
	applicationID := chi.URLParam(r, "applicationId")
	var applicationUUID pgtype.UUID
	if err := applicationUUID.Scan(applicationID); err != nil {
		writeError(w, http.StatusBadRequest, "invalid application id")
		return
	}

	fileMountID := chi.URLParam(r, "fileMountId")
	var fileMountUUID pgtype.UUID
	if err := fileMountUUID.Scan(fileMountID); err != nil {
		writeError(w, http.StatusBadRequest, "invalid file mount id")
		return
	}

	if !h.canAccessApplication(r, applicationUUID) {
		writeError(w, http.StatusForbidden, "access denied")
		return
	}

	existing, err := h.queries.GetApplicationFileMount(r.Context(), fileMountUUID)
	if err != nil {
		writeError(w, http.StatusNotFound, "file mount not found")
		return
	}
	if existing.ApplicationID != applicationUUID {
		writeError(w, http.StatusNotFound, "file mount not found")
		return
	}

	var req updateFileMountRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	mode, err := validateFileMode(req.FileMode)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	// Keep the stored content when the client omits it (editing a secret's
	// metadata without re-sending the value); otherwise re-encrypt the new one.
	encrypted := existing.ContentEncrypted
	if req.Content != nil {
		encrypted, err = h.cfg.Keyring.Encrypt([]byte(*req.Content))
		if err != nil {
			writeServerError(w, r, "failed to encrypt content", err)
			return
		}
	}

	fm, err := h.queries.UpdateApplicationFileMount(r.Context(), generated.UpdateApplicationFileMountParams{
		ID:               fileMountUUID,
		ContentEncrypted: encrypted,
		IsSecret:         req.IsSecret,
		FileMode:         mode,
	})
	if err != nil {
		writeServerError(w, r, "failed to update file mount", err)
		return
	}

	h.markConfigChanged(r.Context(), applicationUUID)

	h.audit(r, "update_file_mount", "application_file_mount", fileMountID, map[string]any{
		"application_id": applicationID,
		"mount_path":     existing.MountPath,
		"is_secret":      req.IsSecret,
	})
	writeJSON(w, http.StatusOK, h.toFileMountResponse(fm))
}

//apidoc:tag applications/file-mounts
//apidoc:title Delete Mount
//apidoc:order 4
func (h *Handler) DeleteFileMount(w http.ResponseWriter, r *http.Request) {
	applicationID := chi.URLParam(r, "applicationId")
	var applicationUUID pgtype.UUID
	if err := applicationUUID.Scan(applicationID); err != nil {
		writeError(w, http.StatusBadRequest, "invalid application id")
		return
	}

	fileMountID := chi.URLParam(r, "fileMountId")
	var fileMountUUID pgtype.UUID
	if err := fileMountUUID.Scan(fileMountID); err != nil {
		writeError(w, http.StatusBadRequest, "invalid file mount id")
		return
	}

	if !h.canAccessApplication(r, applicationUUID) {
		writeError(w, http.StatusForbidden, "access denied")
		return
	}

	fm, err := h.queries.GetApplicationFileMount(r.Context(), fileMountUUID)
	if err != nil {
		writeError(w, http.StatusNotFound, "file mount not found")
		return
	}
	if fm.ApplicationID != applicationUUID {
		writeError(w, http.StatusNotFound, "file mount not found")
		return
	}

	if err := h.queries.DeleteApplicationFileMount(r.Context(), fileMountUUID); err != nil {
		writeServerError(w, r, "failed to delete file mount", err)
		return
	}

	h.markConfigChanged(r.Context(), applicationUUID)

	h.audit(r, "delete_file_mount", "application_file_mount", fileMountID, map[string]any{
		"application_id": applicationID,
		"mount_path":     fm.MountPath,
	})
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}
