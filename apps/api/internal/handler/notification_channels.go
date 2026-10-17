package handler

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/weiliang79/belune/internal/notify"
	"github.com/weiliang79/belune/internal/server/middleware"
	"github.com/weiliang79/belune/internal/service"
	"github.com/weiliang79/belune/internal/store/generated"
)

// notificationChannelResponse is the secret-free view of a channel returned to
// the admin UI. The provider config never leaves the server.
type notificationChannelResponse struct {
	ID         string   `json:"id"`
	Name       string   `json:"name"`
	Type       string   `json:"type"`
	Events     []string `json:"events"`
	Enabled    bool     `json:"enabled"`
	LastSentAt *string  `json:"last_sent_at"`
	LastError  *string  `json:"last_error"`
	// LastEvent is the human-readable label of the most recently delivered (or
	// failed) event, or null if nothing has been delivered yet.
	LastEvent *string `json:"last_event"`
	// Config is the channel's connection config with secret fields stripped, for
	// prefilling the edit form. Omitted on create/update responses.
	Config    json.RawMessage `json:"config,omitempty"`
	CreatedAt string          `json:"created_at"`
	UpdatedAt string          `json:"updated_at"`
}

// lastEventLabel resolves a nullable stored event type to a display label.
func lastEventLabel(t pgtype.Text) *string {
	if !t.Valid || t.String == "" {
		return nil
	}
	label := notify.EventLabel(t.String)
	return &label
}

func toChannelListResponse(c generated.ListNotificationChannelsRow) notificationChannelResponse {
	resp := notificationChannelResponse{
		ID:        uuidToString(c.ID),
		Name:      c.Name,
		Type:      c.Type,
		Events:    c.Events,
		Enabled:   c.Enabled,
		LastEvent: lastEventLabel(c.LastEventType),
		CreatedAt: c.CreatedAt.Time.UTC().Format("2006-01-02T15:04:05Z07:00"),
		UpdatedAt: c.UpdatedAt.Time.UTC().Format("2006-01-02T15:04:05Z07:00"),
	}
	if c.LastSentAt.Valid {
		s := c.LastSentAt.Time.UTC().Format("2006-01-02T15:04:05Z07:00")
		resp.LastSentAt = &s
	}
	if c.LastError.Valid && c.LastError.String != "" {
		e := c.LastError.String
		resp.LastError = &e
	}
	return resp
}

func toChannelResponse(c generated.NotificationChannel) notificationChannelResponse {
	resp := notificationChannelResponse{
		ID:        uuidToString(c.ID),
		Name:      c.Name,
		Type:      c.Type,
		Events:    c.Events,
		Enabled:   c.Enabled,
		LastEvent: lastEventLabel(c.LastEventType),
		CreatedAt: c.CreatedAt.Time.UTC().Format("2006-01-02T15:04:05Z07:00"),
		UpdatedAt: c.UpdatedAt.Time.UTC().Format("2006-01-02T15:04:05Z07:00"),
	}
	if c.LastSentAt.Valid {
		s := c.LastSentAt.Time.UTC().Format("2006-01-02T15:04:05Z07:00")
		resp.LastSentAt = &s
	}
	if c.LastError.Valid && c.LastError.String != "" {
		e := c.LastError.String
		resp.LastError = &e
	}
	return resp
}

type notificationChannelRequest struct {
	Name    string          `json:"name"`
	Type    string          `json:"type"`
	Events  []string        `json:"events"`
	Enabled *bool           `json:"enabled"`
	Config  json.RawMessage `json:"config"`
}

func (req notificationChannelRequest) toSaveParams() service.SaveChannelParams {
	enabled := true
	if req.Enabled != nil {
		enabled = *req.Enabled
	}
	return service.SaveChannelParams{
		Name:    strings.TrimSpace(req.Name),
		Type:    strings.TrimSpace(req.Type),
		Events:  req.Events,
		Enabled: enabled,
		Config:  req.Config,
	}
}

// ListNotificationEvents returns the canonical event registry so the UI's
// subscription checkboxes stay in lockstep with the Go constants.
//
//apidoc:tag platform/notification-channels
//apidoc:title Get Events
//apidoc:order 9
func (h *Handler) ListNotificationEvents(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, notify.Events())
}

// ListNotificationChannels returns all channels (no provider config).
//
//apidoc:tag platform/notification-channels
//apidoc:title Get Channels
//apidoc:order 1
func (h *Handler) ListNotificationChannels(w http.ResponseWriter, r *http.Request) {
	rows, err := h.notifyChannelSvc.List(r.Context())
	if err != nil {
		writeServerError(w, r, "failed to list notification channels", err)
		return
	}
	resp := make([]notificationChannelResponse, 0, len(rows))
	for _, c := range rows {
		item := toChannelListResponse(c)
		item.Config = h.notifyChannelSvc.RedactedConfig(c.Type, c.ConfigEncrypted)
		resp = append(resp, item)
	}
	writeJSON(w, http.StatusOK, resp)
}

// CreateNotificationChannel validates and stores a new channel.
//
//apidoc:tag platform/notification-channels
//apidoc:title Create Channel
//apidoc:order 2
func (h *Handler) CreateNotificationChannel(w http.ResponseWriter, r *http.Request) {
	var req notificationChannelRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if strings.TrimSpace(req.Name) == "" {
		writeError(w, http.StatusBadRequest, "name is required")
		return
	}

	ch, err := h.notifyChannelSvc.Create(r.Context(), middleware.UserIDFromContext(r.Context()), req.toSaveParams())
	if err != nil {
		writeChannelError(w, r, err)
		return
	}
	h.audit(r, "create_notification_channel", "notification_channel", uuidToString(ch.ID), map[string]any{
		"name": ch.Name, "type": ch.Type,
	})
	writeJSON(w, http.StatusCreated, toChannelResponse(ch))
}

// UpdateNotificationChannel replaces name/events/enabled and, when config is
// supplied, the provider config (type is immutable).
//
//apidoc:tag platform/notification-channels
//apidoc:title Update Channel
//apidoc:order 3
func (h *Handler) UpdateNotificationChannel(w http.ResponseWriter, r *http.Request) {
	id, ok := h.channelIDFromPath(w, r)
	if !ok {
		return
	}
	var req notificationChannelRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if strings.TrimSpace(req.Name) == "" {
		writeError(w, http.StatusBadRequest, "name is required")
		return
	}

	ch, err := h.notifyChannelSvc.Update(r.Context(), id, req.toSaveParams())
	if err != nil {
		writeChannelError(w, r, err)
		return
	}
	h.audit(r, "update_notification_channel", "notification_channel", uuidToString(id), map[string]any{
		"name": ch.Name, "type": ch.Type,
	})
	writeJSON(w, http.StatusOK, toChannelResponse(ch))
}

type setChannelEnabledRequest struct {
	Enabled bool `json:"enabled"`
}

// SetNotificationChannelEnabled toggles a channel without touching its config,
// backing the immediate-effect enable/disable Switch.
//
//apidoc:tag platform/notification-channels
//apidoc:title Toggle Channel
//apidoc:order 4
func (h *Handler) SetNotificationChannelEnabled(w http.ResponseWriter, r *http.Request) {
	id, ok := h.channelIDFromPath(w, r)
	if !ok {
		return
	}
	var req setChannelEnabledRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	ch, err := h.notifyChannelSvc.SetEnabled(r.Context(), id, req.Enabled)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeError(w, http.StatusNotFound, "notification channel not found")
			return
		}
		writeServerError(w, r, "failed to update channel", err)
		return
	}
	h.audit(r, "set_notification_channel_enabled", "notification_channel", uuidToString(id), map[string]any{
		"enabled": req.Enabled,
	})
	writeJSON(w, http.StatusOK, toChannelResponse(ch))
}

// DeleteNotificationChannel removes a channel.
//
//apidoc:tag platform/notification-channels
//apidoc:title Delete Channel
//apidoc:order 5
func (h *Handler) DeleteNotificationChannel(w http.ResponseWriter, r *http.Request) {
	id, ok := h.channelIDFromPath(w, r)
	if !ok {
		return
	}
	if err := h.notifyChannelSvc.Delete(r.Context(), id); err != nil {
		writeServerError(w, r, "failed to delete channel", err)
		return
	}
	h.audit(r, "delete_notification_channel", "notification_channel", uuidToString(id), nil)
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

// TestNotificationChannel sends a sample event through the channel and returns
// the provider result verbatim (never fails the request — a delivery error is a
// 200 with ok=false so the UI can display the exact reason).
//
//apidoc:tag platform/notification-channels
//apidoc:title Test Channel
//apidoc:order 6
func (h *Handler) TestNotificationChannel(w http.ResponseWriter, r *http.Request) {
	id, ok := h.channelIDFromPath(w, r)
	if !ok {
		return
	}
	h.audit(r, "test_notification_channel", "notification_channel", uuidToString(id), nil)
	if err := h.notifyChannelSvc.Test(r.Context(), id); err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

type testChannelParamsRequest struct {
	// ID lets the edit form test the stored config when the secret fields are
	// left blank (config omitted).
	ID     string          `json:"id"`
	Type   string          `json:"type"`
	Config json.RawMessage `json:"config"`
}

// TestNotificationChannelParams sends a sample event using ad-hoc config from the
// create/edit dialog, before the channel is saved. Like the saved-channel test,
// a delivery error comes back as 200 with ok=false so the UI shows the reason.
//
//apidoc:tag platform/notification-channels
//apidoc:title Test Configs
//apidoc:order 8
func (h *Handler) TestNotificationChannelParams(w http.ResponseWriter, r *http.Request) {
	var req testChannelParamsRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if strings.TrimSpace(req.Type) == "" {
		writeError(w, http.StatusBadRequest, "type is required")
		return
	}

	var fallbackID pgtype.UUID
	if req.ID != "" {
		_ = fallbackID.Scan(req.ID)
	}
	if err := h.notifyChannelSvc.TestConfig(r.Context(), strings.TrimSpace(req.Type), req.Config, fallbackID); err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// channelIDFromPath parses the {channelId} path param.
func (h *Handler) channelIDFromPath(w http.ResponseWriter, r *http.Request) (pgtype.UUID, bool) {
	var id pgtype.UUID
	if err := id.Scan(chi.URLParam(r, "channelId")); err != nil {
		writeError(w, http.StatusBadRequest, "invalid channel id")
		return id, false
	}
	return id, true
}

// writeChannelError maps a create/update service error to an HTTP status and a
// UI-friendly message: validation problems and unknown types/events are the
// operator's to fix (400), a duplicate name is a conflict (409), a missing row
// is 404, everything else is a generic 500.
func writeChannelError(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusNotFound, "notification channel not found")
		return
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		writeError(w, http.StatusConflict, "a channel with that name already exists")
		return
	}
	msg := err.Error()
	switch {
	case strings.Contains(msg, "required"),
		strings.HasPrefix(msg, "unknown "),
		strings.Contains(msg, "invalid config"):
		writeError(w, http.StatusBadRequest, msg)
	default:
		writeServerError(w, r, "failed to save notification channel", err)
	}
}
