package handler

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"golang.org/x/crypto/bcrypt"

	"github.com/belune-hq/belune/internal/pkg/metrics"
	"github.com/belune-hq/belune/internal/server/middleware"
	"github.com/belune-hq/belune/internal/store/generated"
	"github.com/belune-hq/belune/internal/worker"
)

const passwordResetTTL = 30 * time.Minute

type forgotPasswordRequest struct {
	Email string `json:"email"`
}

// ForgotPassword always returns 200 — no enumeration.
// On a valid email: invalidates prior tokens, creates a new one, enqueues email.
// On an unknown email: sleeps a random 100–200 ms to equalise timing.
func (h *Handler) ForgotPassword(w http.ResponseWriter, r *http.Request) {
	var req forgotPasswordRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Email == "" {
		writeError(w, http.StatusBadRequest, "email is required")
		return
	}

	ctx := r.Context()
	clientIP := middleware.ClientIP(r)

	user, err := h.queries.GetUserByEmail(ctx, normaliseEmail(req.Email))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// Timing equalisation — sleep 100–200 ms on miss.
			jitter, _ := rand.Int(rand.Reader, big.NewInt(101))
			time.Sleep(time.Duration(100+jitter.Int64()) * time.Millisecond)
			writeJSON(w, http.StatusOK, map[string]string{"status": "if the email exists you will receive a reset link"})
			return
		}
		slog.ErrorContext(ctx, "forgot-password: db error looking up user", "error", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}

	// Per-email rate limit: 3 reset requests per hour. Return 200 to avoid enumeration.
	if h.rdb != nil {
		rlKey := "pw_reset:" + normaliseEmail(req.Email)
		count, _ := h.rdb.Incr(ctx, rlKey).Result()
		if count == 1 {
			h.rdb.Expire(ctx, rlKey, time.Hour)
		}
		if count > 3 {
			writeJSON(w, http.StatusOK, map[string]string{"status": "if the email exists you will receive a reset link"})
			return
		}
	}

	// Invalidate any outstanding tokens for this user.
	if err := h.queries.InvalidateUserPasswordResetTokens(ctx, user.ID); err != nil {
		slog.WarnContext(ctx, "forgot-password: failed to invalidate prior tokens", "error", err)
	}

	// Generate 32-byte opaque token; store only the SHA-256 hash.
	plaintext, tokenHash, err := generateResetToken()
	if err != nil {
		slog.ErrorContext(ctx, "forgot-password: failed to generate token", "error", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}

	expiresAt := pgtype.Timestamptz{Time: time.Now().Add(passwordResetTTL), Valid: true}

	if _, err := h.queries.CreatePasswordResetToken(ctx, generated.CreatePasswordResetTokenParams{
		UserID:    user.ID,
		TokenHash: tokenHash,
		ExpiresAt: expiresAt,
		CreatedIp: clientIP,
	}); err != nil {
		slog.ErrorContext(ctx, "forgot-password: failed to create token", "error", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	metrics.RecordPasswordResetIssued()

	// Enqueue email asynchronously — never block the request on SMTP.
	baseURL := ""
	if h.emailSvc != nil {
		baseURL = h.emailSvc.PublicURL()
	}
	resetURL := fmt.Sprintf("%s/reset-password?token=%s", baseURL, plaintext)

	firstName := user.FirstName
	if firstName == "" {
		firstName = user.Email
	}

	task, err := worker.NewEmailSendTask("password_reset", user.Email, map[string]any{
		"FirstName": firstName,
		"ResetURL":  resetURL,
	})
	if err != nil {
		slog.WarnContext(ctx, "forgot-password: failed to build email task", "error", err)
	} else if _, err := h.asynq.Enqueue(task); err != nil {
		slog.WarnContext(ctx, "forgot-password: failed to enqueue email", "error", err)
	}

	if h.auditSvc != nil {
		h.auditSvc.Log(uuidToString(user.ID), "", clientIP, "password_reset_requested", "user", uuidToString(user.ID), nil)
	}

	writeJSON(w, http.StatusOK, map[string]string{"status": "if the email exists you will receive a reset link"})
}

type resetPasswordRequest struct {
	Token       string `json:"token"`
	NewPassword string `json:"new_password"`
}

// ResetPassword validates the token, updates the password, and revokes all sessions.
func (h *Handler) ResetPassword(w http.ResponseWriter, r *http.Request) {
	var req resetPasswordRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Token == "" || req.NewPassword == "" {
		writeError(w, http.StatusBadRequest, "token and new_password are required")
		return
	}
	if len(req.NewPassword) < 8 {
		writeError(w, http.StatusBadRequest, "new password must be at least 8 characters")
		return
	}

	ctx := r.Context()
	clientIP := middleware.ClientIP(r)

	tokenHash := hashResetToken(req.Token)

	record, err := h.queries.GetPasswordResetTokenByHash(ctx, tokenHash)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeError(w, http.StatusBadRequest, "invalid or expired reset token")
			return
		}
		slog.ErrorContext(ctx, "reset-password: db error looking up token", "error", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}

	if record.UsedAt.Valid {
		writeError(w, http.StatusBadRequest, "reset token has already been used")
		return
	}
	if !record.ExpiresAt.Valid || time.Now().After(record.ExpiresAt.Time) {
		writeError(w, http.StatusBadRequest, "reset token has expired")
		return
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(req.NewPassword), bcrypt.DefaultCost)
	if err != nil {
		slog.ErrorContext(ctx, "reset-password: failed to hash password", "error", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}

	if err := h.queries.UpdateUserPassword(ctx, generated.UpdateUserPasswordParams{
		ID:           record.UserID,
		PasswordHash: string(hash),
	}); err != nil {
		slog.ErrorContext(ctx, "reset-password: failed to update password", "error", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	metrics.RecordPasswordResetRedeemed()

	// Mark token used before revoking sessions (so partial failures don't leave the token reusable).
	if err := h.queries.MarkPasswordResetTokenUsed(ctx, record.ID); err != nil {
		slog.WarnContext(ctx, "reset-password: failed to mark token used", "error", err)
	}

	// Revoke all active sessions for this user.
	if err := h.auth.RevokeUserSessions(ctx, record.UserID); err != nil {
		slog.WarnContext(ctx, "reset-password: failed to revoke sessions", "error", err)
	}

	if h.auditSvc != nil {
		h.auditSvc.Log(uuidToString(record.UserID), "", clientIP, "password_reset_completed", "user", uuidToString(record.UserID), nil)
	}

	writeJSON(w, http.StatusOK, map[string]string{"status": "password reset successful"})
}

// generateResetToken creates a 32-byte cryptographically random token.
// Returns (plaintext hex, sha256 hex hash, error).
func generateResetToken() (plaintext, tokenHash string, err error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", "", fmt.Errorf("generate token: %w", err)
	}
	plaintext = hex.EncodeToString(b)
	tokenHash = hashResetToken(plaintext)
	return plaintext, tokenHash, nil
}

func hashResetToken(plaintext string) string {
	sum := sha256.Sum256([]byte(plaintext))
	return hex.EncodeToString(sum[:])
}
