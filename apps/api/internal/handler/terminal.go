package handler

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"nhooyr.io/websocket"
	"nhooyr.io/websocket/wsjson"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/belune-hq/belune/internal/naming"
	"github.com/belune-hq/belune/internal/pkg/metrics"
	"github.com/belune-hq/belune/internal/pkg/tracing"
	"github.com/belune-hq/belune/internal/runtime"
	"github.com/belune-hq/belune/internal/server/middleware"
	"github.com/belune-hq/belune/internal/terminal"
)

// termMsg is the JSON envelope for terminal WebSocket messages.
type termMsg struct {
	Type string `json:"type"`
	// output: base64-encoded terminal data
	Data string `json:"data,omitempty"`
	// resize
	Cols int `json:"cols,omitempty"`
	Rows int `json:"rows,omitempty"`
	// error / info
	Message string `json:"message,omitempty"`
}

// CreateTerminalSession creates a Docker exec session and returns a session ID.
// POST /api/projects/{projectId}/applications/{applicationId}/terminal
//
//apidoc:tag terminal
func (h *Handler) CreateTerminalSession(w http.ResponseWriter, r *http.Request) {
	ctx, span := tracing.Tracer().Start(r.Context(), "terminal.create")
	defer span.End()
	r = r.WithContext(ctx)

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

	var req struct {
		Shell string `json:"shell"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		req.Shell = ""
	}
	shell := req.Shell
	if shell == "" {
		shell = "sh"
	}
	// Restrict shell to known safe choices
	switch shell {
	case "sh", "bash", "ash", "zsh", "fish":
	default:
		shell = "sh"
	}

	row, err := h.queries.GetApplicationWithProjectSlug(r.Context(), applicationUUID)
	if err != nil {
		writeError(w, http.StatusNotFound, "application not found")
		return
	}
	containerName := naming.ContainerName(row.ProjectSlug, row.Slug, applicationID)

	rt, err := h.runtimes.For(r.Context(), row.ServerID)
	if err != nil {
		writeError(w, http.StatusBadGateway, "failed to reach the application's server")
		return
	}

	sess, err := rt.ContainerExecTTY(r.Context(), containerName, []string{shell})
	if err != nil {
		writeError(w, http.StatusBadGateway, fmt.Sprintf("exec failed: %v", err))
		return
	}

	userID := middleware.UserIDFromContext(r.Context())
	s, ok := h.termManager.Create(applicationID, userID, shell, sess.ExecID, sess.RWC)
	if !ok {
		sess.RWC.Close()
		writeError(w, http.StatusTooManyRequests, "terminal session limit reached")
		return
	}
	metrics.RecordTerminalSessionStarted()
	span.SetAttributes(
		attribute.String("terminal.session_id", s.ID),
		attribute.String("terminal.application_id", applicationID),
		attribute.String("terminal.shell", shell),
	)

	h.audit(r, "terminal.session.started", "application", applicationID, map[string]any{
		"shell":      shell,
		"session_id": s.ID,
	})

	writeJSON(w, http.StatusCreated, map[string]string{"session_id": s.ID})
}

// HandleTerminalWebSocket proxies a WebSocket connection to an active exec session.
// GET /api/ws/terminal/{sessionId}
//
//apidoc:tag terminal
func (h *Handler) HandleTerminalWebSocket(w http.ResponseWriter, r *http.Request) {
	if h.termManager == nil {
		writeError(w, http.StatusServiceUnavailable, "terminal not available")
		return
	}

	sessionID := chi.URLParam(r, "sessionId")
	s := h.termManager.Get(sessionID)
	if s == nil {
		writeError(w, http.StatusNotFound, "session not found")
		return
	}

	// Auth: the route carries middleware.RequireSession (Auth() alone accepts
	// a PAT too, since PR2), so this is really reached with a session — confirm
	// the requesting user owns this terminal session on top of that.
	userID := middleware.UserIDFromContext(r.Context())
	role := middleware.RoleFromContext(r.Context())
	if role != "admin" && s.UserID != userID {
		writeError(w, http.StatusForbidden, "access denied")
		return
	}

	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		InsecureSkipVerify: true,
	})
	if err != nil {
		slog.Error("terminal ws: accept failed", "error", err)
		return
	}

	// Start session span after WS accept so it captures the live session duration.
	sessionCtx, span := tracing.Tracer().Start(r.Context(), "terminal.session",
		trace.WithAttributes(
			attribute.String("terminal.session_id", sessionID),
			attribute.String("terminal.application_id", s.ApplicationID),
			attribute.String("terminal.shell", s.Shell),
		),
	)

	// Tie both directions to a shared cancellation. Whichever side exits
	// first (WS closed, exec stream closed, request cancelled) cancels the
	// other so the input goroutine cannot outlive the handler.
	ctx, cancel := context.WithCancel(sessionCtx)
	defer cancel()

	// Resolved once for the life of the session: placement cannot change under
	// a live exec, and the browser's ResizeObserver is not debounced — a lookup
	// per resize frame would be a database round trip per dragged pixel.
	rt, err := h.runtimeForSession(ctx, s)
	if err != nil {
		slog.Warn("terminal: could not reach the session's server; resize disabled", "session_id", sessionID, "error", err)
	}

	defer func() {
		conn.Close(websocket.StatusNormalClosure, "")
		startedAt := s.CreatedAt
		h.termManager.Delete(sessionID)
		duration := time.Since(startedAt)
		h.auditSvc.Log(s.UserID, middleware.TokenIDFromContext(r.Context()), middleware.ClientIP(r), "terminal.session.ended", "application", s.ApplicationID, map[string]any{
			"shell":      s.Shell,
			"session_id": s.ID,
			"duration_s": int(duration.Seconds()),
		})
		metrics.RecordTerminalSessionClosed(duration)
		span.SetAttributes(attribute.Float64("terminal.duration_seconds", duration.Seconds()))
		span.End()
	}()

	// ws → exec stdin: read from WebSocket and write to container
	go func() {
		defer cancel()
		defer s.Close()
		for {
			var msg termMsg
			if err := wsjson.Read(ctx, conn, &msg); err != nil {
				return
			}
			switch msg.Type {
			case "input":
				data, err := base64.StdEncoding.DecodeString(msg.Data)
				if err != nil {
					// Try raw string as fallback (plain text input)
					data = []byte(msg.Data)
				}
				if _, err := s.RWC.Write(data); err != nil {
					return
				}
			case "resize":
				if msg.Cols > 0 && msg.Rows > 0 {
					// Resizing addresses an exec that already exists, so it goes
					// to the host that created it — rt above, resolved from the
					// session's own scope.
					if rt != nil {
						if err := rt.ContainerExecResize(ctx, s.ExecID, uint(msg.Rows), uint(msg.Cols)); err != nil {
							slog.Debug("terminal: resize failed", "error", err)
						}
					}
				}
			}
		}
	}()

	// exec stdout → ws: read from container and send to WebSocket
	buf := make([]byte, 4096)
	for {
		n, err := s.RWC.Read(buf)
		if n > 0 {
			encoded := base64.StdEncoding.EncodeToString(buf[:n])
			writeErr := wsjson.Write(ctx, conn, termMsg{Type: "output", Data: encoded})
			if writeErr != nil {
				return
			}
		}
		if err != nil {
			_ = wsjson.Write(ctx, conn, termMsg{Type: "closed", Message: "session ended"})
			return
		}
	}
}

// runtimeForSession resolves the host a terminal session's exec lives on. A
// host-shell session is scoped to this machine; an application session follows
// its application's placement.
func (h *Handler) runtimeForSession(ctx context.Context, s *terminal.Session) (runtime.ContainerRuntime, error) {
	if s.ApplicationID == hostShellScope {
		return h.runtimes.Local(ctx)
	}
	var appUUID pgtype.UUID
	if err := appUUID.Scan(s.ApplicationID); err != nil {
		return nil, fmt.Errorf("terminal session %s has no resolvable scope: %w", s.ID, err)
	}
	return h.runtimeForApplication(ctx, appUUID)
}
