package handler

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"golang.org/x/crypto/bcrypt"

	"github.com/weiliang79/belune/internal/server/middleware"
	"github.com/weiliang79/belune/internal/service"
	"github.com/weiliang79/belune/internal/store/generated"
)

//apidoc:tag platform/users
//apidoc:title Get Users
//apidoc:order 1
func (h *Handler) ListUsers(w http.ResponseWriter, r *http.Request) {
	users, err := h.queries.ListUsers(r.Context())
	if err != nil {
		writeServerError(w, r, "failed to list users", err)
		return
	}

	writeJSON(w, http.StatusOK, users)
}

type createUserRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
	Role     string `json:"role"`
	Username string `json:"username"`
}

// CreateUser creates a user with an admin-supplied plaintext password.
// Deprecated: prefer POST /api/users/invite — the invitation flow avoids
// transmitting plaintext passwords out-of-band.
//
//apidoc:tag platform/users
func (h *Handler) CreateUser(w http.ResponseWriter, r *http.Request) {
	slog.Warn("CreateUser: deprecated endpoint called — use POST /api/users/invite instead",
		"remote_addr", r.RemoteAddr)

	var req createUserRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	if req.Email == "" || req.Password == "" {
		writeError(w, http.StatusBadRequest, "email and password are required")
		return
	}

	if len(req.Password) < 8 {
		writeError(w, http.StatusBadRequest, "password must be at least 8 characters")
		return
	}

	if req.Role != "admin" && req.Role != "member" {
		writeError(w, http.StatusBadRequest, "role must be admin or member")
		return
	}

	user, err := h.auth.Register(r.Context(), req.Email, req.Password, req.Role, req.Username)
	if err != nil {
		if errors.Is(err, service.ErrUserAlreadyExists) {
			writeError(w, http.StatusConflict, "user already exists")
			return
		}
		writeServerError(w, r, "failed to create user", err)
		return
	}

	h.audit(r, "create_user", "user", uuidToString(user.ID), map[string]any{"email": user.Email, "role": user.Role})

	writeJSON(w, http.StatusCreated, generated.ListUsersRow{
		ID:        user.ID,
		Email:     user.Email,
		Role:      user.Role,
		Username:  user.Username,
		FirstName: user.FirstName,
		LastName:  user.LastName,
		CreatedAt: user.CreatedAt,
	})
}

type updateUserRoleRequest struct {
	Role string `json:"role"`
}

//apidoc:tag platform/users
func (h *Handler) UpdateUserRole(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "userId")
	var uuid pgtype.UUID
	if err := uuid.Scan(id); err != nil {
		writeError(w, http.StatusBadRequest, "invalid user id")
		return
	}

	var req updateUserRoleRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	if req.Role != "admin" && req.Role != "member" {
		writeError(w, http.StatusBadRequest, "role must be admin or member")
		return
	}

	// Prevent leaving zero admins: if demoting an admin, check count
	target, err := h.queries.GetUserByID(r.Context(), uuid)
	if err != nil {
		writeError(w, http.StatusNotFound, "user not found")
		return
	}

	if target.Role == "admin" && req.Role != "admin" {
		count, err := h.queries.CountAdmins(r.Context())
		if err != nil {
			writeServerError(w, r, "failed to check admin count", err)
			return
		}
		if count <= 1 {
			writeError(w, http.StatusBadRequest, "cannot demote the last admin")
			return
		}
	}

	updated, err := h.queries.UpdateUserRole(r.Context(), generated.UpdateUserRoleParams{
		ID:   uuid,
		Role: req.Role,
	})
	if err != nil {
		writeServerError(w, r, "failed to update user role", err)
		return
	}

	// Force the target user to re-login so they pick up the new role: revoke
	// all their refresh tokens and set the per-user invalidated_after flag so
	// existing access tokens are rejected on next use.
	if err := h.auth.RevokeUserSessions(r.Context(), uuid); err != nil {
		slog.Warn("auth: failed to revoke sessions on role change", "user_id", id, "error", err)
	}

	h.audit(r, "update_user_role", "user", id, map[string]any{"role": req.Role})

	writeJSON(w, http.StatusOK, updated)
}

//apidoc:tag platform/users
func (h *Handler) DeleteUser(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "userId")
	var uuid pgtype.UUID
	if err := uuid.Scan(id); err != nil {
		writeError(w, http.StatusBadRequest, "invalid user id")
		return
	}

	// Prevent self-deletion
	callerID := middleware.UserIDFromContext(r.Context())
	if id == callerID {
		writeError(w, http.StatusBadRequest, "cannot delete your own account")
		return
	}

	// Prevent deleting last admin
	target, err := h.queries.GetUserByID(r.Context(), uuid)
	if err != nil {
		writeError(w, http.StatusNotFound, "user not found")
		return
	}

	if target.Role == "admin" {
		count, err := h.queries.CountAdmins(r.Context())
		if err != nil {
			writeServerError(w, r, "failed to check admin count", err)
			return
		}
		if count <= 1 {
			writeError(w, http.StatusBadRequest, "cannot delete the last admin")
			return
		}
	}

	if err := h.queries.DeleteUser(r.Context(), uuid); err != nil {
		writeServerError(w, r, "failed to delete user", err)
		return
	}

	h.audit(r, "delete_user", "user", id, map[string]any{"email": target.Email})

	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

type resetUserPasswordRequest struct {
	Password        string `json:"password"`
	CurrentPassword string `json:"current_password"`
}

// ResetUserPassword sets a NEW password for another user with no re-auth of
// its own beyond the RequireSession gate — a hijacked or borrowed admin
// session could otherwise take over any account, including another admin's,
// with nothing re-checked. Step up the same way ChangeOwnPassword does: the
// CALLER's own current password, not the target's.
//
//apidoc:tag platform/users
func (h *Handler) ResetUserPassword(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "userId")
	var uuid pgtype.UUID
	if err := uuid.Scan(id); err != nil {
		writeError(w, http.StatusBadRequest, "invalid user id")
		return
	}

	var req resetUserPasswordRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	if len(req.Password) < 8 {
		writeError(w, http.StatusBadRequest, "password must be at least 8 characters")
		return
	}

	var callerID pgtype.UUID
	callerID.Scan(middleware.UserIDFromContext(r.Context()))
	caller, err := h.queries.GetUserByID(r.Context(), callerID)
	if err != nil {
		writeServerError(w, r, "failed to get caller", err)
		return
	}
	if bcrypt.CompareHashAndPassword([]byte(caller.PasswordHash), []byte(req.CurrentPassword)) != nil {
		writeError(w, http.StatusUnauthorized, "incorrect password")
		return
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(req.Password), bcrypt.DefaultCost)
	if err != nil {
		writeServerError(w, r, "failed to hash password", err)
		return
	}

	if err := h.queries.UpdateUserPassword(r.Context(), generated.UpdateUserPasswordParams{
		ID:           uuid,
		PasswordHash: string(hash),
	}); err != nil {
		writeServerError(w, r, "failed to reset password", err)
		return
	}

	// Admin-initiated password reset must invalidate every active session for
	// the target user — they may have been compromised.
	if err := h.auth.RevokeUserSessions(r.Context(), uuid); err != nil {
		slog.Warn("auth: failed to revoke sessions on admin password reset", "user_id", id, "error", err)
	}

	h.audit(r, "reset_user_password", "user", id, nil)

	writeJSON(w, http.StatusOK, map[string]string{"status": "password updated"})
}
