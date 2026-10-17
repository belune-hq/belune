package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5/pgtype"
	"golang.org/x/crypto/bcrypt"

	"github.com/weiliang79/belune/internal/pkg/metrics"
	"github.com/weiliang79/belune/internal/server/middleware"
	"github.com/weiliang79/belune/internal/service"
	"github.com/weiliang79/belune/internal/store/generated"
)

// settingHostShellEnabled gates the in-UI host shell. Absent or anything other
// than "true" means disabled — the capability is off by default.
const settingHostShellEnabled = "host_shell_enabled"

// hostShellScope is the synthetic termManager "application" scope for host-shell
// sessions, distinguishing them from per-application terminals.
const hostShellScope = "host"

// CreateHostShellSession opens a root shell on the HOST, via a throwaway
// privileged helper container that nsenters into the host namespaces. It is
// triple-gated: the host_shell_enabled setting must be on, the caller must be an
// admin (enforced by the route group), and they must re-enter their password.
// Every session is audited. POST /api/maintenance/host-shell (admin only)
//
//apidoc:tag platform/maintenance
func (h *Handler) CreateHostShellSession(w http.ResponseWriter, r *http.Request) {
	if h.termManager == nil {
		writeError(w, http.StatusServiceUnavailable, "terminal not available")
		return
	}

	// Gate 1: the capability must be explicitly enabled.
	if s, err := h.queries.GetSetting(r.Context(), settingHostShellEnabled); err != nil || strings.TrimSpace(s.Value) != "true" {
		writeError(w, http.StatusForbidden, "host shell is disabled")
		return
	}

	// Gate 2: step-up re-auth. Even with a valid admin session, opening host root
	// requires re-proving the password — this defends against a hijacked session.
	// A password alone does not defend against a stolen one, so anyone with a
	// second factor enrolled must present that too. It follows from having the
	// factor rather than from a setting: this is the highest-privilege action in
	// the product, and the user already said this account needs two.
	var req struct {
		Password string `json:"password"`
		Method   string `json:"method"`
		Code     string `json:"code"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Password == "" {
		writeError(w, http.StatusBadRequest, "password required")
		return
	}
	userID := middleware.UserIDFromContext(r.Context())
	if _, ok := h.stepUpReauth(w, r, req.Password, req.Method, req.Code); !ok {
		return
	}

	// Run the helper from Belune's own image — it ships nsenter (Debian base) and
	// is already local, so there is nothing to pull.
	image := h.selfImage(r.Context())
	if image == "" {
		writeServerError(w, r, "could not determine host-shell helper image", nil)
		return
	}

	// The host shell is by definition a session on this machine, so it is the
	// local runtime rather than a placement lookup.
	rt, err := h.runtimes.Local(r.Context())
	if err != nil {
		writeError(w, http.StatusBadGateway, "failed to reach the Docker host")
		return
	}

	sess, err := rt.HostShellSession(r.Context(), image)
	if err != nil {
		writeError(w, http.StatusBadGateway, fmt.Sprintf("host shell failed: %v", err))
		return
	}

	s, ok := h.termManager.Create(hostShellScope, userID, "host", sess.ExecID, sess.RWC)
	if !ok {
		sess.RWC.Close()
		writeError(w, http.StatusTooManyRequests, "terminal session limit reached")
		return
	}

	metrics.RecordTerminalSessionStarted()
	h.audit(r, "host_shell.session.started", "host", s.ID, map[string]any{"session_id": s.ID})

	writeJSON(w, http.StatusCreated, map[string]string{"session_id": s.ID})
}

// selfImage returns the image reference of Belune's own container, used to run
// the host-shell helper. Empty when it can't be resolved.
func (h *Handler) selfImage(ctx context.Context) string {
	c, ok := h.selfContainer(ctx)
	if !ok {
		return ""
	}
	return c.Image
}

// stepUpReauth re-verifies the caller's password — and, if their account has a
// second factor enrolled, a verification code too — even though they already
// hold a valid session. This defends against a hijacked session for the
// product's highest-privilege actions (host shell, self-update): a stolen
// password alone should not be enough for either, so anyone with a second
// factor enrolled must present that as well.
//
// On failure it writes the appropriate error response itself and returns
// ok=false, so callers can just `if _, ok := h.stepUpReauth(...); !ok { return }`.
func (h *Handler) stepUpReauth(w http.ResponseWriter, r *http.Request, password, method, code string) (user generated.User, ok bool) {
	userID := middleware.UserIDFromContext(r.Context())
	var uid pgtype.UUID
	if err := uid.Scan(userID); err != nil {
		writeError(w, http.StatusUnauthorized, "invalid session")
		return generated.User{}, false
	}
	user, err := h.queries.GetUserByID(r.Context(), uid)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "invalid session")
		return generated.User{}, false
	}
	if bcrypt.CompareHashAndPassword([]byte(user.PasswordHash), []byte(password)) != nil {
		writeError(w, http.StatusUnauthorized, "incorrect password")
		return generated.User{}, false
	}
	if service.HasMFA(user) {
		if strings.TrimSpace(code) == "" {
			writeError(w, http.StatusUnauthorized, "verification code required")
			return generated.User{}, false
		}
		if method == "" {
			method = service.MethodTOTP
		}
		if err := h.totpSvc.Verify(r.Context(), user, method, code); err != nil {
			writeSecondFactorError(w, err)
			return generated.User{}, false
		}
	}
	return user, true
}
