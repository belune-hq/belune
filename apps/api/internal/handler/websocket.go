package handler

import (
	"log/slog"
	"net/http"
	"net/url"
	"strings"

	"nhooyr.io/websocket"

	"github.com/belune-hq/belune/internal/server/middleware"
	"github.com/belune-hq/belune/internal/ws"
)

// HandleWebSocket upgrades the connection to WebSocket and registers the client.
// The dashboard authenticates with its session cookie, because a browser cannot
// set headers on a WebSocket; a script sends the usual bearer token instead.
//
// ⚠️ Filed under Platform, not a live-updates domain of its own. That domain
// held this one operation, which put it in the sidebar directly beside the
// hand-written WebSockets guide — two top-level entries for one subject, and a
// reader had to open both to find that out. The socket is cross-cutting rather
// than operator-only (a member uses it for their own app's build logs), so
// Platform slightly over-signals; removing the duplicate entry is worth more.
//
//apidoc:tag platform
//apidoc:title Open the Live Updates Socket
//apidoc:order 4
//apidoc:description Subscribe to live metrics, logs, request traces and container status over one socket. OpenAPI cannot describe a WebSocket protocol, so the frames, the channel list and the connection limits are documented in the [WebSockets guide](/docs/api/overview/websockets).
func (h *Handler) HandleWebSocket(w http.ResponseWriter, r *http.Request) {
	if h.hub == nil {
		writeError(w, http.StatusServiceUnavailable, "websocket not available")
		return
	}

	// Extract user ID from auth context (set by auth middleware)
	userID := middleware.UserIDFromContext(r.Context())
	if userID == "" {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}

	// Validate the Origin header to prevent cross-site WebSocket hijacking (CSWSH).
	if origin := r.Header.Get("Origin"); origin != "" && !h.originAllowed(r, origin) {
		slog.Warn("ws: rejected connection from disallowed origin", "origin", origin, "host", r.Host)
		writeError(w, http.StatusForbidden, "origin not allowed")
		return
	}

	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		// Origin is validated by originAllowed above; skip the library's own
		// host-match check so a configured cross-origin client (the Vite dev
		// server on another port) can still connect.
		InsecureSkipVerify: true,
	})
	if err != nil {
		slog.Error("ws: failed to accept connection", "error", err)
		return
	}

	client := ws.NewClient(h.hub, conn, userID)
	if !h.hub.Register(client) {
		conn.Close(websocket.StatusTryAgainLater, "too many connections")
		return
	}

	// Start read and write pumps. ReadPump runs synchronously, so r.Context() —
	// which carries the user, role and any token pin Auth resolved — lives for
	// the whole socket, and the authorizer reads the caller straight from it.
	go client.WritePump(r.Context())
	client.ReadPump(r.Context(), &wsChannelAuthorizer{queries: h.queries})
}

// originAllowed reports whether a browser Origin may open a WebSocket.
//
// Same-origin is always allowed: the page making the request was served by this
// very host, so it cannot be a cross-site hijack. This has to be derived from the
// request rather than configured, because the dashboard's hostname is set at
// runtime from the UI — a static allowlist would reject the panel's own socket
// the moment an operator pointed a domain at it, and again on every change.
//
// CORS_ORIGINS remains for genuinely cross-origin clients, i.e. the Vite dev
// server on :5173 talking to the API on :8080.
func (h *Handler) originAllowed(r *http.Request, origin string) bool {
	if u, err := url.Parse(origin); err == nil && u.Host != "" && strings.EqualFold(u.Host, r.Host) {
		return true
	}
	for _, o := range h.cfg.CORSOrigins {
		if strings.EqualFold(o, origin) {
			return true
		}
	}
	return false
}
