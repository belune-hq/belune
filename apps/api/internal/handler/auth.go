package handler

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"golang.org/x/crypto/bcrypt"

	"github.com/belune-hq/belune/internal/proxy"
	"github.com/belune-hq/belune/internal/server/middleware"
	"github.com/belune-hq/belune/internal/service"
	"github.com/belune-hq/belune/internal/store/generated"
)

const refreshCookieName = "refresh_token"
const refreshCookiePath = "/api/auth"

type loginRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

func (h *Handler) Login(w http.ResponseWriter, r *http.Request) {
	var req loginRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	if req.Email == "" || req.Password == "" {
		writeError(w, http.StatusBadRequest, "email and password are required")
		return
	}

	ctx := r.Context()
	clientIP := middleware.ClientIP(r)
	userAgent := r.UserAgent()

	// Lockout check is keyed by lowercased email so attackers cannot bypass by
	// alternating capitalization. We do this *before* DB lookup so that a
	// locked email gets the same response whether the user exists or not.
	emailKey := normaliseEmail(req.Email)

	if locked, retryAfter, err := h.auth.CheckLockout(ctx, emailKey); err != nil {
		slog.Warn("auth: lockout check failed", "error", err)
	} else if locked {
		writeLockedResponse(w, retryAfter)
		return
	}

	outcome, err := h.auth.Login(ctx, req.Email, req.Password, userAgent, clientIP)
	if err != nil {
		if errors.Is(err, service.ErrInvalidCredentials) {
			h.recordLoginFailure(ctx, emailKey, clientIP)
			// Re-check lockout: if this attempt tripped the threshold, return
			// 429 instead of 401 so the UI surfaces the lockout immediately.
			if locked, retryAfter, _ := h.auth.CheckLockout(ctx, emailKey); locked {
				writeLockedResponse(w, retryAfter)
				return
			}
			writeError(w, http.StatusUnauthorized, "invalid email or password")
			return
		}
		if errors.Is(err, service.ErrSecondFactorUnavailable) {
			slog.Error("auth: second factor enrolled but unavailable", "error", err)
			writeError(w, http.StatusServiceUnavailable, "two-factor verification is unavailable")
			return
		}
		slog.Error("auth: login failed", "error", err)
		writeError(w, http.StatusInternalServerError, "login failed")
		return
	}

	// The password was right but is not enough on its own: hand back the
	// challenge and no session at all. The client completes it at
	// POST /api/auth/login/verify.
	//
	// The lockout counter is deliberately NOT cleared here. Clearing it on a
	// correct password would let anyone holding the password reset the counter
	// between code guesses by re-posting this endpoint, so the account-level
	// lockout could never fire against second-factor guessing at all.
	// CompleteLoginChallenge clears it once the login actually completes.
	if outcome.Challenge != nil {
		if h.auditSvc != nil {
			uid := uuidToString(outcome.UserID)
			h.auditSvc.Log(uid, "", clientIP, "login_challenge_issued", "user", uid, nil)
		}
		writeJSON(w, http.StatusOK, outcome.Challenge)
		return
	}

	// A completed login → clear any stale lockout state for this email.
	h.auth.ResetLoginAttempts(ctx, emailKey)

	csrfToken, err := middleware.GenerateCSRFToken()
	if err != nil {
		writeServerError(w, r, "failed to generate csrf token", err)
		return
	}

	h.setSessionCookies(w, r, outcome.Session, csrfToken)

	if h.auditSvc != nil {
		uid := uuidToString(outcome.Session.User.ID)
		h.auditSvc.Log(uid, "", clientIP, "login", "user", uid, nil)
	}

	writeJSON(w, http.StatusOK, outcome.Session)
}

// Refresh exchanges the refresh-token cookie for a fresh access + refresh
// pair. No auth middleware: the refresh cookie *is* the credential. CSRF
// middleware is still applied at the route layer.
func (h *Handler) Refresh(w http.ResponseWriter, r *http.Request) {
	cookie, err := r.Cookie(refreshCookieName)
	if err != nil || cookie.Value == "" {
		writeError(w, http.StatusUnauthorized, "missing refresh token")
		return
	}

	clientIP := middleware.ClientIP(r)
	result, err := h.auth.Refresh(r.Context(), cookie.Value, r.UserAgent(), clientIP)
	if err != nil {
		// Clear the bad cookie so the client doesn't keep retrying with it.
		h.clearSessionCookies(w, r)
		writeError(w, http.StatusUnauthorized, "invalid refresh token")
		return
	}

	csrfToken, err := middleware.GenerateCSRFToken()
	if err != nil {
		writeServerError(w, r, "failed to generate csrf token", err)
		return
	}

	h.setSessionCookies(w, r, result, csrfToken)
	writeJSON(w, http.StatusOK, result)
}

//apidoc:tag account/profile
func (h *Handler) Logout(w http.ResponseWriter, r *http.Request) {
	// Revoke the refresh token (DB) first — survives Redis being down.
	if cookie, err := r.Cookie(refreshCookieName); err == nil && cookie.Value != "" {
		if err := h.auth.RevokeRefreshToken(r.Context(), cookie.Value); err != nil {
			slog.Warn("auth: failed to revoke refresh token on logout", "error", err)
		}
	}

	// Blacklist the access token (Redis, best-effort).
	if tokenString := extractTokenFromRequest(r); tokenString != "" {
		if err := h.auth.BlacklistToken(r.Context(), tokenString); err != nil {
			slog.Warn("auth: failed to blacklist access token on logout", "error", err)
		}
	}

	h.clearSessionCookies(w, r)

	h.audit(r, "logout", "user", "", nil)

	writeJSON(w, http.StatusOK, map[string]string{"status": "logged out"})
}

// secureCookies reports whether session cookies should carry the Secure
// attribute, i.e. be withheld from any plain-HTTP request.
//
// Derived from the request rather than read only from config, because the
// dashboard's domain — and therefore whether it is served over HTTPS at all — is
// set from inside the UI at runtime. A config-only flag would leave session
// cookies non-Secure after an operator switched to HTTPS, until someone
// remembered to edit .env and restart.
//
// Trusting X-Forwarded-Proto is safe here: Caddy overwrites whatever the client
// sent with the real scheme (verified in both directions), and the API is not
// reachable except through it — port 8080 is bound to loopback. SECURE_COOKIES
// still forces the attribute on, for deployments fronted by something else.
func (h *Handler) secureCookies(r *http.Request) bool {
	if h.cfg.SecureCookies {
		return true
	}
	if r.TLS != nil {
		return true
	}
	return strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}

// publicBaseURL returns the externally reachable origin — scheme://host, no
// trailing slash — for building absolute URLs a browser or third party will use:
// an OAuth redirect_uri, a provider callback, a post-callback UI redirect.
//
// It deliberately does NOT read the request's Host or X-Forwarded-Host. The API
// is fronted by a Caddy catch-all that forwards any Host to it, so those headers
// are attacker-controllable; using them to build an OAuth redirect_uri or a
// Location header is a host-header-injection / open-redirect hole. Both sources
// here are operator-configured instead:
//
//  1. PUBLIC_BASE_URL when set — the declared canonical origin.
//  2. otherwise the stored dashboard domain (Settings → Dashboard domain), which
//     the admin sets and Caddy serves over HTTPS via ACME. That is also what an
//     OAuth provider requires a redirect_uri to be, so https is the right scheme.
//
// Neither configured → an error, so the caller fails loudly with something the
// operator can act on rather than emitting a spoofable or relative URL.
func (h *Handler) publicBaseURL(r *http.Request) (string, error) {
	if h.cfg.PublicBaseURL != "" {
		return strings.TrimRight(h.cfg.PublicBaseURL, "/"), nil
	}
	if s, err := h.queries.GetSetting(r.Context(), proxy.SettingDashboardDomain); err == nil {
		if domain := strings.TrimSpace(s.Value); domain != "" {
			return "https://" + domain, nil
		}
	}
	return "", errors.New("PUBLIC_BASE_URL is not set and no dashboard domain is configured — set one before connecting a git provider")
}

// setSessionCookies emits the access, refresh, and CSRF cookies for a
// successful login or refresh. The refresh cookie is scoped to /api/auth so
// it is never sent to non-auth endpoints — defence-in-depth in case of a
// rogue endpoint that echoes Cookie headers.
func (h *Handler) setSessionCookies(w http.ResponseWriter, r *http.Request, result *service.LoginResult, csrfToken string) {
	accessTTL := h.auth.AccessExpiry()
	refreshTTL := h.auth.RefreshExpiry()
	secure := h.secureCookies(r)

	http.SetCookie(w, &http.Cookie{
		Name:     "token",
		Value:    result.Token,
		Path:     "/",
		HttpOnly: true,
		Secure:   secure,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(accessTTL / time.Second),
	})

	http.SetCookie(w, &http.Cookie{
		Name:     refreshCookieName,
		Value:    result.RefreshToken,
		Path:     refreshCookiePath,
		HttpOnly: true,
		Secure:   secure,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(refreshTTL / time.Second),
	})

	// CSRF cookie covers the longer refresh window so the token doesn't
	// expire mid-session and break in-flight requests after a refresh.
	http.SetCookie(w, &http.Cookie{
		Name:     middleware.CSRFCookieName,
		Value:    csrfToken,
		Path:     "/",
		HttpOnly: false,
		Secure:   secure,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(refreshTTL / time.Second),
	})
}

func (h *Handler) clearSessionCookies(w http.ResponseWriter, r *http.Request) {
	secure := h.secureCookies(r)

	http.SetCookie(w, &http.Cookie{
		Name: "token", Value: "", Path: "/",
		HttpOnly: true, Secure: secure,
		SameSite: http.SameSiteLaxMode, MaxAge: -1,
	})
	http.SetCookie(w, &http.Cookie{
		Name: refreshCookieName, Value: "", Path: refreshCookiePath,
		HttpOnly: true, Secure: secure,
		SameSite: http.SameSiteLaxMode, MaxAge: -1,
	})
	http.SetCookie(w, &http.Cookie{
		Name: middleware.CSRFCookieName, Value: "", Path: "/",
		HttpOnly: false, Secure: secure,
		SameSite: http.SameSiteLaxMode, MaxAge: -1,
	})
}

func (h *Handler) recordLoginFailure(ctx context.Context, emailKey, clientIP string) {
	// Audit *before* writing the failure counter so the audit row exists even
	// if the lockout write fails.
	if h.auditSvc != nil {
		h.auditSvc.Log("", "", clientIP, "login_failed", "user", emailKey, map[string]any{"email": emailKey})
	}
	if _, _, err := h.auth.RecordFailedLogin(ctx, emailKey); err != nil {
		slog.Warn("auth: failed to record login failure", "error", err)
	}
}

func writeLockedResponse(w http.ResponseWriter, retryAfter time.Duration) {
	secs := int(retryAfter.Seconds())
	if secs < 1 {
		secs = 1
	}
	w.Header().Set("Retry-After", strconv.Itoa(secs))
	writeJSON(w, http.StatusTooManyRequests, map[string]any{
		"error":       "account temporarily locked due to repeated failed login attempts",
		"retry_after": secs,
	})
}

func normaliseEmail(s string) string {
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'A' && c <= 'Z' {
			c += 'a' - 'A'
		}
		out = append(out, c)
	}
	return string(out)
}

// extractTokenFromRequest extracts the JWT from Authorization header or cookie.
func extractTokenFromRequest(r *http.Request) string {
	if auth := r.Header.Get("Authorization"); len(auth) > 7 && auth[:7] == "Bearer " {
		return auth[7:]
	}
	if cookie, err := r.Cookie("token"); err == nil {
		return cookie.Value
	}
	return ""
}

//apidoc:tag account/profile
//apidoc:title Get Profile
//apidoc:order 1
func (h *Handler) Me(w http.ResponseWriter, r *http.Request) {
	userID := middleware.UserIDFromContext(r.Context())
	if userID == "" {
		writeError(w, http.StatusUnauthorized, "not authenticated")
		return
	}

	var userUUID pgtype.UUID
	if err := userUUID.Scan(userID); err != nil {
		writeServerError(w, r, "invalid user id", err)
		return
	}

	user, err := h.queries.GetUserByID(r.Context(), userUUID)
	if err != nil {
		writeServerError(w, r, "failed to get user", err)
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"id":         userID,
		"email":      user.Email,
		"role":       user.Role,
		"username":   user.Username,
		"first_name": user.FirstName,
		"last_name":  user.LastName,
		"created_at": user.CreatedAt,
	})
}

type updateProfileRequest struct {
	Username  string `json:"username"`
	FirstName string `json:"first_name"`
	LastName  string `json:"last_name"`
}

//apidoc:tag account/profile
func (h *Handler) UpdateProfile(w http.ResponseWriter, r *http.Request) {
	userID := middleware.UserIDFromContext(r.Context())
	var userUUID pgtype.UUID
	if err := userUUID.Scan(userID); err != nil {
		writeServerError(w, r, "invalid user id", err)
		return
	}

	var req updateProfileRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	updated, err := h.queries.UpdateUserProfile(r.Context(), generated.UpdateUserProfileParams{
		ID:        userUUID,
		Username:  req.Username,
		FirstName: req.FirstName,
		LastName:  req.LastName,
	})
	if err != nil {
		writeServerError(w, r, "failed to update profile", err)
		return
	}

	writeJSON(w, http.StatusOK, updated)
}

type changePasswordRequest struct {
	CurrentPassword string `json:"current_password"`
	NewPassword     string `json:"new_password"`
}

//apidoc:tag account/profile
func (h *Handler) ChangeOwnPassword(w http.ResponseWriter, r *http.Request) {
	var req changePasswordRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	if req.CurrentPassword == "" || req.NewPassword == "" {
		writeError(w, http.StatusBadRequest, "current_password and new_password are required")
		return
	}

	if len(req.NewPassword) < 8 {
		writeError(w, http.StatusBadRequest, "new password must be at least 8 characters")
		return
	}

	var userID pgtype.UUID
	userID.Scan(middleware.UserIDFromContext(r.Context()))

	user, err := h.queries.GetUserByID(r.Context(), userID)
	if err != nil {
		writeServerError(w, r, "failed to get user", err)
		return
	}

	if err := bcrypt.CompareHashAndPassword([]byte(user.PasswordHash), []byte(req.CurrentPassword)); err != nil {
		writeError(w, http.StatusUnauthorized, "current password is incorrect")
		return
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(req.NewPassword), bcrypt.DefaultCost)
	if err != nil {
		writeServerError(w, r, "failed to hash password", err)
		return
	}

	if err := h.queries.UpdateUserPassword(r.Context(), generated.UpdateUserPasswordParams{
		ID:           userID,
		PasswordHash: string(hash),
	}); err != nil {
		writeServerError(w, r, "failed to update password", err)
		return
	}

	// Self-service password change revokes other sessions but keeps the
	// caller's current cookie working; their access token is still valid for
	// up to AccessExpiry, after which their next refresh attempt will fail
	// because all refresh tokens were revoked. Acceptable trade-off: we don't
	// have to forge a new refresh token here.
	if err := h.auth.RevokeUserSessions(r.Context(), userID); err != nil {
		slog.Warn("auth: failed to revoke sessions on password change", "error", err)
	}

	writeJSON(w, http.StatusOK, map[string]string{"status": "password updated"})
}

type setupRequest struct {
	Email        string `json:"email"`
	Password     string `json:"password"`
	Username     string `json:"username"`
	InstanceName string `json:"instance_name"`
}

func (h *Handler) Setup(w http.ResponseWriter, r *http.Request) {
	required, err := h.auth.IsSetupRequired(r.Context())
	if err != nil {
		writeServerError(w, r, "failed to check setup status", err)
		return
	}

	if r.Method == http.MethodGet {
		writeJSON(w, http.StatusOK, map[string]bool{"setup_required": required})
		return
	}

	if !required {
		writeError(w, http.StatusConflict, "setup already completed")
		return
	}

	var req setupRequest
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

	user, err := h.auth.Register(r.Context(), req.Email, req.Password, "admin", req.Username)
	if err != nil {
		writeServerError(w, r, "failed to create admin user", err)
		return
	}

	// Persist the instance name chosen during onboarding (best-effort; failure
	// here shouldn't fail setup since the admin is already created).
	if name := strings.TrimSpace(req.InstanceName); name != "" {
		if _, err := h.queries.UpsertSetting(r.Context(), generated.UpsertSettingParams{
			Key: "instance_name", Value: name,
		}); err != nil {
			slog.WarnContext(r.Context(), "failed to persist instance_name during setup", "error", err)
		}
	}

	writeJSON(w, http.StatusCreated, map[string]any{
		"id":       user.ID,
		"email":    user.Email,
		"role":     user.Role,
		"username": user.Username,
	})
}
