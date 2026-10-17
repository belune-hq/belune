package handler

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"golang.org/x/crypto/bcrypt"

	"github.com/weiliang79/belune/internal/server/middleware"
	"github.com/weiliang79/belune/internal/service"
	"github.com/weiliang79/belune/internal/store/generated"
)

// settingDashboardDomain labels the entry in the user's authenticator app. The
// domain is the only thing distinguishing two Belune installs in a list of
// accounts, so it is worth the lookup; the product name is the fallback.
const settingDashboardDomain = "dashboard_domain"

func (h *Handler) totpIssuer(r *http.Request) string {
	if s, err := h.queries.GetSetting(r.Context(), settingDashboardDomain); err == nil {
		if domain := strings.TrimSpace(s.Value); domain != "" {
			return domain
		}
	}
	return "Belune"
}

// currentUser resolves the authenticated user's row. The session carries only
// an id, and every second-factor decision needs the stored secret.
func (h *Handler) currentUser(r *http.Request) (uid pgtype.UUID, user generated.User, err error) {
	if scanErr := uid.Scan(middleware.UserIDFromContext(r.Context())); scanErr != nil {
		return uid, user, scanErr
	}
	user, err = h.queries.GetUserByID(r.Context(), uid)
	return uid, user, err
}

// rotateSessionAfterFactorChange ends every session this user has and starts a
// new one on this device. Turning a factor on or off is something people do
// when they think someone else is signed in, so the other sessions have to go —
// but signing the user out of the browser they just re-authenticated in makes
// the safe action feel like a punishment. Revocation is per-user, so keeping
// the current session means replacing it.
func (h *Handler) rotateSessionAfterFactorChange(w http.ResponseWriter, r *http.Request, user generated.User) *service.LoginResult {
	if err := h.auth.RevokeUserSessions(r.Context(), user.ID); err != nil {
		slog.Warn("totp: failed to revoke sessions after a factor change", "error", err)
	}

	session, err := h.auth.IssueSessionFor(r.Context(), user, r.UserAgent(), middleware.ClientIP(r))
	if err != nil {
		slog.Error("totp: failed to re-issue session after a factor change", "error", err)
		// The revocation already happened, so this browser's token is dead
		// whatever we do. Clearing the cookies makes that visible immediately
		// rather than leaving the user to discover it on their next request.
		h.clearSessionCookies(w, r)
		return nil
	}
	csrfToken, err := middleware.GenerateCSRFToken()
	if err != nil {
		slog.Error("totp: failed to generate csrf token after a factor change", "error", err)
		h.clearSessionCookies(w, r)
		return nil
	}
	h.setSessionCookies(w, r, session, csrfToken)
	return session
}

// GetTOTPStatus reports whether the second factor is on, and how many recovery
// codes are left — the number is what warns a user before they run out.
// GET /api/auth/totp
//
//apidoc:tag account/profile
func (h *Handler) GetTOTPStatus(w http.ResponseWriter, r *http.Request) {
	uid, user, err := h.currentUser(r)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "invalid session")
		return
	}

	remaining, err := h.totpSvc.RemainingRecoveryCodes(r.Context(), uid)
	if err != nil {
		writeServerError(w, r, "failed to read two-factor status", err)
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"enabled":                  service.HasMFA(user),
		"enabled_at":               user.TotpEnabledAt,
		"recovery_codes_remaining": remaining,
	})
}

// EnrollTOTP generates a secret and returns it for scanning. It does NOT enable
// anything: a secret the user's app never actually stored must not be able to
// lock them out. POST /api/auth/totp/enroll
//
//apidoc:tag account/profile
func (h *Handler) EnrollTOTP(w http.ResponseWriter, r *http.Request) {
	var req enrollRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	_, user, err := h.currentUser(r)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "invalid session")
		return
	}
	// The password, not just a session. Enabling a factor is at least as
	// dangerous as disabling one: whoever enrolls holds the only authenticator,
	// and enabling revokes every other session — so a hijacked session alone
	// must not be able to do it, or the real owner is locked out of their own
	// account with no recovery codes and no way back short of an admin reset.
	if bcrypt.CompareHashAndPassword([]byte(user.PasswordHash), []byte(req.Password)) != nil {
		writeError(w, http.StatusUnauthorized, "incorrect password")
		return
	}
	if service.HasMFA(user) {
		writeError(w, http.StatusConflict, "two-factor authentication is already enabled")
		return
	}

	enrollment, err := h.totpSvc.Enroll(r.Context(), user, h.totpIssuer(r))
	if err != nil {
		slog.Error("totp: enroll failed", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to start two-factor enrollment")
		return
	}

	h.audit(r, "totp.enrollment_started", "user", uuidToString(user.ID), nil)
	writeJSON(w, http.StatusOK, enrollment)
}

type enrollRequest struct {
	Password string `json:"password"`
}

type verifyEnrollmentRequest struct {
	Code string `json:"code"`
}

// VerifyTOTPEnrollment turns the factor on, and only now, once the user has
// produced a code from the secret. The recovery codes are returned here and
// never again. POST /api/auth/totp/enroll/verify
//
//apidoc:tag account/profile
func (h *Handler) VerifyTOTPEnrollment(w http.ResponseWriter, r *http.Request) {
	var req verifyEnrollmentRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || strings.TrimSpace(req.Code) == "" {
		writeError(w, http.StatusBadRequest, "code is required")
		return
	}

	_, user, err := h.currentUser(r)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "invalid session")
		return
	}
	if service.HasMFA(user) {
		writeError(w, http.StatusConflict, "two-factor authentication is already enabled")
		return
	}

	codes, err := h.totpSvc.ConfirmEnrollment(r.Context(), user, req.Code)
	if err != nil {
		writeSecondFactorError(w, err)
		return
	}

	session := h.rotateSessionAfterFactorChange(w, r, user)

	h.audit(r, "totp.enabled", "user", uuidToString(user.ID), nil)
	writeJSON(w, http.StatusOK, map[string]any{"recovery_codes": codes, "session": session})
}

type disableTOTPRequest struct {
	Password string `json:"password"`
	Code     string `json:"code"`
	Method   string `json:"method"`
}

// DisableTOTP requires the password AND a current code: turning the factor off
// is exactly what someone with a stolen session would want to do first.
// POST /api/auth/totp/disable
//
//apidoc:tag account/profile
func (h *Handler) DisableTOTP(w http.ResponseWriter, r *http.Request) {
	var req disableTOTPRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	uid, user, err := h.currentUser(r)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "invalid session")
		return
	}
	if !service.HasMFA(user) {
		writeError(w, http.StatusConflict, "two-factor authentication is not enabled")
		return
	}
	if bcrypt.CompareHashAndPassword([]byte(user.PasswordHash), []byte(req.Password)) != nil {
		writeError(w, http.StatusUnauthorized, "incorrect password")
		return
	}

	method := req.Method
	if method == "" {
		method = service.MethodTOTP
	}
	if err := h.totpSvc.Verify(r.Context(), user, method, req.Code); err != nil {
		writeSecondFactorError(w, err)
		return
	}

	if err := h.totpSvc.Disable(r.Context(), uid); err != nil {
		slog.Error("totp: disable failed", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to disable two-factor authentication")
		return
	}
	session := h.rotateSessionAfterFactorChange(w, r, user)

	h.audit(r, "totp.disabled", "user", uuidToString(user.ID), nil)
	writeJSON(w, http.StatusOK, map[string]any{"status": "disabled", "session": session})
}

type regenerateCodesRequest struct {
	Password string `json:"password"`
	Code     string `json:"code"`
	Method   string `json:"method"`
}

// RegenerateRecoveryCodes issues a new set and kills the old one — the only
// safe reading of the request, since the usual reason to ask is believing the
// old list is compromised. POST /api/auth/totp/recovery-codes
//
//apidoc:tag account/profile
func (h *Handler) RegenerateRecoveryCodes(w http.ResponseWriter, r *http.Request) {
	var req regenerateCodesRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	_, user, err := h.currentUser(r)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "invalid session")
		return
	}
	if bcrypt.CompareHashAndPassword([]byte(user.PasswordHash), []byte(req.Password)) != nil {
		writeError(w, http.StatusUnauthorized, "incorrect password")
		return
	}
	// A current code as well, for the same reason disabling needs one: ten fresh
	// recovery codes are ten working second factors, so without this a stolen
	// session plus a stolen password walks straight past the factor.
	method := req.Method
	if method == "" {
		method = service.MethodTOTP
	}
	if err := h.totpSvc.Verify(r.Context(), user, method, req.Code); err != nil {
		writeSecondFactorError(w, err)
		return
	}

	codes, err := h.totpSvc.RegenerateRecoveryCodes(r.Context(), user)
	if err != nil {
		if errors.Is(err, service.ErrTOTPNotEnrolled) {
			writeError(w, http.StatusConflict, "two-factor authentication is not enabled")
			return
		}
		slog.Error("totp: regenerate recovery codes failed", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to regenerate recovery codes")
		return
	}

	h.audit(r, "totp.recovery_codes_regenerated", "user", uuidToString(user.ID), nil)
	writeJSON(w, http.StatusOK, map[string]any{"recovery_codes": codes})
}

type loginVerifyRequest struct {
	Challenge string `json:"challenge"`
	Method    string `json:"method"`
	Code      string `json:"code"`
}

// VerifyLogin is the second step of login. It takes the method as data rather
// than living at a per-method URL, so adding another factor later needs no new
// endpoint and no client change. POST /api/auth/login/verify
func (h *Handler) VerifyLogin(w http.ResponseWriter, r *http.Request) {
	var req loginVerifyRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Challenge == "" || strings.TrimSpace(req.Code) == "" {
		writeError(w, http.StatusBadRequest, "challenge and code are required")
		return
	}
	method := req.Method
	if method == "" {
		method = service.MethodTOTP
	}

	ctx := r.Context()
	clientIP := middleware.ClientIP(r)

	// Resolve the subject before spending the challenge so a failed attempt is
	// audited against a real account rather than an opaque token.
	subject, subjErr := h.auth.ChallengeSubject(ctx, req.Challenge)
	if subjErr == nil {
		if locked, retryAfter, err := h.auth.CheckLockout(ctx, normaliseEmail(subject.Email)); err == nil && locked {
			writeLockedResponse(w, retryAfter)
			return
		}
	}

	result, err := h.auth.CompleteLoginChallenge(ctx, req.Challenge, method, req.Code, r.UserAgent(), clientIP)
	if err != nil {
		if h.auditSvc != nil && subjErr == nil {
			uid := uuidToString(subject.ID)
			h.auditSvc.Log(uid, "", clientIP, "login_2fa_failed", "user", uid, map[string]any{"method": method})
		}
		writeSecondFactorError(w, err)
		return
	}

	csrfToken, err := middleware.GenerateCSRFToken()
	if err != nil {
		writeServerError(w, r, "failed to generate csrf token", err)
		return
	}
	h.setSessionCookies(w, r, result, csrfToken)

	if h.auditSvc != nil {
		uid := uuidToString(result.User.ID)
		h.auditSvc.Log(uid, "", clientIP, "login", "user", uid, map[string]any{"method": method})
	}

	writeJSON(w, http.StatusOK, result)
}

type adminResetUserTOTPRequest struct {
	CurrentPassword string `json:"current_password"`
	Method          string `json:"method"`
	Code            string `json:"code"`
}

// AdminResetUserTOTP clears another user's second factor, for the lost-device
// case that recovery codes did not cover. An admin can already do nearly
// anything, so the control is not permission but visibility: it is audited
// loudly and it ends that user's sessions. It also removes a factor from
// someone else's account, so — like ResetUserPassword — it steps up on the
// CALLER's own password first: a hijacked or borrowed admin session must not
// be able to strip another admin's second factor with nothing re-checked.
// If the CALLER has MFA enrolled, a fresh code is required too — the same
// "password alone doesn't defend against a stolen session" reasoning
// DisableTOTP, RegenerateRecoveryCodes, and CreateHostShellSession already
// apply to actions of comparable or lesser sensitivity; stripping someone
// ELSE's factor should not need less proof than disabling your own.
// POST /api/users/{userId}/totp/reset (admin only)
//
//apidoc:tag platform/users
func (h *Handler) AdminResetUserTOTP(w http.ResponseWriter, r *http.Request) {
	var uid pgtype.UUID
	if err := uid.Scan(chi.URLParam(r, "userId")); err != nil {
		writeError(w, http.StatusBadRequest, "invalid user id")
		return
	}

	var req adminResetUserTOTPRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
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
	if service.HasMFA(caller) {
		if strings.TrimSpace(req.Code) == "" {
			writeError(w, http.StatusUnauthorized, "verification code required")
			return
		}
		method := req.Method
		if method == "" {
			method = service.MethodTOTP
		}
		if err := h.totpSvc.Verify(r.Context(), caller, method, req.Code); err != nil {
			writeSecondFactorError(w, err)
			return
		}
	}

	target, err := h.queries.GetUserByID(r.Context(), uid)
	if err != nil {
		writeError(w, http.StatusNotFound, "user not found")
		return
	}
	if !service.HasMFA(target) {
		writeError(w, http.StatusConflict, "two-factor authentication is not enabled for this user")
		return
	}

	if err := h.totpSvc.Disable(r.Context(), uid); err != nil {
		slog.Error("totp: admin reset failed", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to reset two-factor authentication")
		return
	}
	if err := h.auth.RevokeUserSessions(r.Context(), uid); err != nil {
		slog.Warn("totp: failed to revoke sessions after admin reset", "error", err)
	}

	h.audit(r, "totp.admin_reset", "user", uuidToString(uid), map[string]any{"email": target.Email})
	writeJSON(w, http.StatusOK, map[string]string{"status": "reset"})
}

// writeSecondFactorError maps the verification failures to responses. The
// already-used case is kept distinct because "invalid" for a code the user
// entered correctly is a dead end they cannot act on.
func writeSecondFactorError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, service.ErrSecondFactorUsed):
		writeError(w, http.StatusUnauthorized, "that code has already been used — wait for the next one")
	case errors.Is(err, service.ErrInvalidSecondFactor):
		writeError(w, http.StatusUnauthorized, "invalid verification code")
	case errors.Is(err, service.ErrInvalidChallenge):
		writeError(w, http.StatusUnauthorized, "your sign-in session expired — please sign in again")
	case errors.Is(err, service.ErrUnsupportedMethod):
		writeError(w, http.StatusBadRequest, "unsupported verification method")
	case errors.Is(err, service.ErrTOTPNotEnrolled):
		writeError(w, http.StatusConflict, "two-factor authentication is not enabled")
	case errors.Is(err, service.ErrSecondFactorUnavailable):
		writeError(w, http.StatusServiceUnavailable, "two-factor verification is unavailable")
	default:
		slog.Error("totp: verification failed", "error", err)
		writeError(w, http.StatusInternalServerError, "verification failed")
	}
}
