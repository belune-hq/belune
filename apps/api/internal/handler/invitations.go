package handler

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/weiliang79/belune/internal/pkg/metrics"
	"github.com/weiliang79/belune/internal/server/middleware"
	"github.com/weiliang79/belune/internal/service"
	"github.com/weiliang79/belune/internal/store/generated"
	"github.com/weiliang79/belune/internal/worker"
)

const invitationTTL = 7 * 24 * time.Hour

type inviteUserRequest struct {
	Email string `json:"email"`
	Role  string `json:"role"`
}

// invitationResponse is the safe public view of an invitation row — omits token_hash.
type invitationResponse struct {
	ID              pgtype.UUID        `json:"id"`
	Email           string             `json:"email"`
	Role            string             `json:"role"`
	InvitedByUserID pgtype.UUID        `json:"invited_by_user_id"`
	ExpiresAt       pgtype.Timestamptz `json:"expires_at"`
	AcceptedAt      pgtype.Timestamptz `json:"accepted_at"`
	CreatedAt       pgtype.Timestamptz `json:"created_at"`
}

func toInvitationResponse(inv generated.Invitation) invitationResponse {
	return invitationResponse{
		ID:              inv.ID,
		Email:           inv.Email,
		Role:            inv.Role,
		InvitedByUserID: inv.InvitedByUserID,
		ExpiresAt:       inv.ExpiresAt,
		AcceptedAt:      inv.AcceptedAt,
		CreatedAt:       inv.CreatedAt,
	}
}

// InviteUser generates an invitation token, stores the invitation, and
// enqueues the invitation email. Admin-only.
// POST /api/users/invite
//
//apidoc:tag platform/users
func (h *Handler) InviteUser(w http.ResponseWriter, r *http.Request) {
	var req inviteUserRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Email == "" {
		writeError(w, http.StatusBadRequest, "email is required")
		return
	}
	if req.Role != "admin" && req.Role != "member" {
		writeError(w, http.StatusBadRequest, "role must be admin or member")
		return
	}

	ctx := r.Context()
	clientIP := middleware.ClientIP(r)
	inviterID := middleware.UserIDFromContext(ctx)

	email := normaliseEmail(req.Email)

	// Reject if the email is already a registered user.
	if _, err := h.queries.GetUserByEmail(ctx, email); err == nil {
		writeError(w, http.StatusConflict, "a user with that email already exists")
		return
	}

	// Cancel any existing pending invitation for this email so the
	// partial-unique index stays satisfied and the user receives a fresh link.
	if existing, err := h.queries.GetInvitationByEmailPending(ctx, email); err == nil {
		if err := h.queries.RevokeInvitation(ctx, existing.ID); err != nil {
			slog.WarnContext(ctx, "invite-user: failed to revoke prior invitation", "error", err)
		}
	}

	plaintext, tokenHash, err := generateResetToken() // reuse CSPRNG helper
	if err != nil {
		slog.ErrorContext(ctx, "invite-user: failed to generate token", "error", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}

	inviterUUID := pgtype.UUID{}
	if err := inviterUUID.Scan(inviterID); err != nil {
		slog.ErrorContext(ctx, "invite-user: bad inviter user id", "error", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}

	inv, err := h.queries.CreateInvitation(ctx, generated.CreateInvitationParams{
		Email:           email,
		Role:            req.Role,
		TokenHash:       tokenHash,
		InvitedByUserID: inviterUUID,
		ExpiresAt:       pgtype.Timestamptz{Time: time.Now().Add(invitationTTL), Valid: true},
	})
	if err != nil {
		slog.ErrorContext(ctx, "invite-user: failed to create invitation", "error", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	metrics.RecordInvitationIssued()

	baseURL := ""
	if h.emailSvc != nil {
		baseURL = h.emailSvc.PublicURL()
	}
	acceptURL := fmt.Sprintf("%s/accept-invite?token=%s", baseURL, plaintext)

	task, err := worker.NewEmailSendTask("user_invitation", email, map[string]any{
		"InviteURL": acceptURL,
		"Role":      req.Role,
	})
	if err != nil {
		slog.WarnContext(ctx, "invite-user: failed to build email task", "error", err)
	} else if _, err := h.asynq.Enqueue(task); err != nil {
		slog.WarnContext(ctx, "invite-user: failed to enqueue email", "error", err)
	}

	if h.auditSvc != nil {
		h.auditSvc.Log(inviterID, middleware.TokenIDFromContext(ctx), clientIP, "invitation_sent", "invitation", uuidToString(inv.ID), nil)
	}

	writeJSON(w, http.StatusCreated, toInvitationResponse(inv))
}

// ListPendingInvitations returns all active (non-expired, non-accepted) invitations.
// Admin-only.
// GET /api/users/invitations
//
//apidoc:tag platform/users
//apidoc:title Get Invitations
//apidoc:order 2
func (h *Handler) ListPendingInvitations(w http.ResponseWriter, r *http.Request) {
	invs, err := h.queries.ListPendingInvitations(r.Context())
	if err != nil {
		writeServerError(w, r, "failed to list invitations", err)
		return
	}
	out := make([]invitationResponse, len(invs))
	for i, inv := range invs {
		out[i] = toInvitationResponse(inv)
	}
	writeJSON(w, http.StatusOK, out)
}

// RevokeInvitation deletes a pending invitation. Admin-only.
// DELETE /api/users/invitations/{invitationId}
//
//apidoc:tag platform/users
//apidoc:title Revoke Invitation
//apidoc:order 3
func (h *Handler) RevokeInvitation(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	clientIP := middleware.ClientIP(r)
	invID := chi.URLParam(r, "invitationId")

	var id pgtype.UUID
	if err := id.Scan(invID); err != nil {
		writeError(w, http.StatusBadRequest, "invalid invitation id")
		return
	}

	if err := h.queries.RevokeInvitation(ctx, id); err != nil {
		slog.ErrorContext(ctx, "revoke-invitation: db error", "error", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}

	if h.auditSvc != nil {
		callerID := middleware.UserIDFromContext(ctx)
		h.auditSvc.Log(callerID, middleware.TokenIDFromContext(ctx), clientIP, "invitation_revoked", "invitation", invID, nil)
	}

	w.WriteHeader(http.StatusNoContent)
}

// GetInvitation returns the email and role encoded in an invitation token
// without consuming the token. Lets the acceptance UI render context.
// GET /api/auth/invitation?token=<plaintext>
func (h *Handler) GetInvitation(w http.ResponseWriter, r *http.Request) {
	token := r.URL.Query().Get("token")
	if token == "" {
		writeError(w, http.StatusBadRequest, "token is required")
		return
	}

	ctx := r.Context()
	tokenHash := hashResetToken(token) // same SHA-256 util
	inv, err := h.queries.GetInvitationByHash(ctx, tokenHash)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeError(w, http.StatusNotFound, "invitation not found or no longer valid")
			return
		}
		slog.ErrorContext(ctx, "get-invitation: db error", "error", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}

	if inv.AcceptedAt.Valid {
		writeError(w, http.StatusGone, "invitation has already been accepted")
		return
	}
	if !inv.ExpiresAt.Valid || time.Now().After(inv.ExpiresAt.Time) {
		writeError(w, http.StatusGone, "invitation has expired")
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{
		"email": inv.Email,
		"role":  inv.Role,
	})
}

type acceptInvitationRequest struct {
	Token     string `json:"token"`
	Password  string `json:"password"`
	Username  string `json:"username"`
	FirstName string `json:"first_name"`
	LastName  string `json:"last_name"`
}

// AcceptInvitation creates a user account from a valid invitation token and
// immediately issues a login session. Public endpoint.
// POST /api/auth/accept-invitation
func (h *Handler) AcceptInvitation(w http.ResponseWriter, r *http.Request) {
	var req acceptInvitationRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Token == "" || req.Password == "" {
		writeError(w, http.StatusBadRequest, "token and password are required")
		return
	}
	if len(req.Password) < 8 {
		writeError(w, http.StatusBadRequest, "password must be at least 8 characters")
		return
	}

	ctx := r.Context()
	clientIP := middleware.ClientIP(r)

	tokenHash := hashResetToken(req.Token)
	inv, err := h.queries.GetInvitationByHash(ctx, tokenHash)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeError(w, http.StatusBadRequest, "invalid or expired invitation")
			return
		}
		slog.ErrorContext(ctx, "accept-invitation: db error", "error", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}

	if inv.AcceptedAt.Valid {
		writeError(w, http.StatusBadRequest, "invitation has already been accepted")
		return
	}
	if !inv.ExpiresAt.Valid || time.Now().After(inv.ExpiresAt.Time) {
		writeError(w, http.StatusBadRequest, "invitation has expired")
		return
	}

	user, err := h.auth.Register(ctx, inv.Email, req.Password, inv.Role, req.Username)
	if err != nil {
		if errors.Is(err, service.ErrUserAlreadyExists) {
			writeError(w, http.StatusConflict, "a user with that email already exists")
			return
		}
		slog.ErrorContext(ctx, "accept-invitation: failed to register user", "error", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}

	// Set optional name fields directly — Register doesn't expose them.
	if req.FirstName != "" || req.LastName != "" {
		if _, err := h.queries.UpdateUserProfile(ctx, generated.UpdateUserProfileParams{
			ID:        user.ID,
			FirstName: req.FirstName,
			LastName:  req.LastName,
			Username:  user.Username,
		}); err != nil {
			slog.WarnContext(ctx, "accept-invitation: failed to set name fields", "error", err)
		}
		// Refresh user row so the JWT carries the updated name.
		if updated, err := h.queries.GetUserByID(ctx, user.ID); err == nil {
			user = updated
		}
	}

	if err := h.queries.MarkInvitationAccepted(ctx, generated.MarkInvitationAcceptedParams{
		ID:             inv.ID,
		AcceptedUserID: pgtype.UUID{Bytes: user.ID.Bytes, Valid: true},
	}); err != nil {
		slog.ErrorContext(ctx, "accept-invitation: failed to mark invitation accepted", "error", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	metrics.RecordInvitationAccepted()

	result, err := h.auth.LoginUser(ctx, user, r.UserAgent(), clientIP)
	if err != nil {
		slog.ErrorContext(ctx, "accept-invitation: failed to issue session", "error", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}

	csrfToken, err := middleware.GenerateCSRFToken()
	if err != nil {
		writeServerError(w, r, "failed to generate csrf token", err)
		return
	}

	h.setSessionCookies(w, r, result, csrfToken)

	if h.auditSvc != nil {
		uid := uuidToString(user.ID)
		h.auditSvc.Log(uid, "", clientIP, "invitation_accepted", "user", uid, nil)
	}

	writeJSON(w, http.StatusCreated, result)
}
