package handler

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/weiliang79/belune/internal/service"
	"github.com/weiliang79/belune/internal/service/email"
)

// smtpSettingsRequest is the create/update payload. Password is preserved when
// left blank (the stored secret is masked on read, so an unchanged form never
// carries it).
type smtpSettingsRequest struct {
	Host      string `json:"host"`
	Port      int    `json:"port"`
	User      string `json:"user"`
	FromEmail string `json:"from_email"`
	FromName  string `json:"from_name"`
	TLSMode   string `json:"tls_mode"`
	Password  string `json:"password"`
}

// GetSMTPSettings returns the effective SMTP config with the password masked to
// a presence flag.
//
//apidoc:tag platform/maintenance
func (h *Handler) GetSMTPSettings(w http.ResponseWriter, r *http.Request) {
	view, err := h.smtpSettingsSvc.Get(r.Context())
	if err != nil {
		writeServerError(w, r, "failed to load SMTP settings", err)
		return
	}
	writeJSON(w, http.StatusOK, view)
}

// UpdateSMTPSettings validates and persists the SMTP config. Changes take effect
// on the next send — no restart.
//
//apidoc:tag platform/maintenance
func (h *Handler) UpdateSMTPSettings(w http.ResponseWriter, r *http.Request) {
	var req smtpSettingsRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	params := service.SMTPSaveParams{
		Host:      req.Host,
		Port:      req.Port,
		User:      req.User,
		FromEmail: req.FromEmail,
		FromName:  req.FromName,
		TLSMode:   req.TLSMode,
	}
	// A blank password preserves the stored one; a non-blank value replaces it.
	if strings.TrimSpace(req.Password) != "" {
		pw := req.Password
		params.Password = &pw
	}

	if err := h.smtpSettingsSvc.Save(r.Context(), params); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	h.audit(r, "update_smtp_settings", "settings", "smtp", map[string]any{
		"host": strings.TrimSpace(req.Host),
	})
	writeJSON(w, http.StatusOK, map[string]string{"status": "updated"})
}

type testSMTPRequest struct {
	smtpSettingsRequest
	To string `json:"to"`
}

// TestSMTPSettings sends a test email using the posted (possibly unsaved) config.
// A blank password falls back to the stored one, so an operator can test an
// existing setup without re-entering the secret. Delivery errors come back as
// 200 with ok=false, mirroring the other test endpoints.
//
//apidoc:tag platform/maintenance
func (h *Handler) TestSMTPSettings(w http.ResponseWriter, r *http.Request) {
	var req testSMTPRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	to := strings.TrimSpace(req.To)
	if to == "" {
		writeError(w, http.StatusBadRequest, "a recipient address is required")
		return
	}
	if strings.TrimSpace(req.Host) == "" {
		writeError(w, http.StatusBadRequest, "SMTP host is required to test")
		return
	}

	cfg := email.SMTPConfig{
		Host:      strings.TrimSpace(req.Host),
		Port:      req.Port,
		User:      strings.TrimSpace(req.User),
		Password:  req.Password,
		FromEmail: strings.TrimSpace(req.FromEmail),
		FromName:  strings.TrimSpace(req.FromName),
		TLSMode:   strings.TrimSpace(req.TLSMode),
	}
	if cfg.Password == "" {
		if stored, err := h.smtpSettingsSvc.ResolveSMTP(r.Context()); err == nil {
			cfg.Password = stored.Password
		}
	}

	h.audit(r, "test_smtp_settings", "settings", "smtp", nil)
	msg := email.Message{
		To:       to,
		Subject:  "Belune SMTP test",
		TextBody: "This is a test email from Belune. If you received it, your SMTP settings are working.",
	}
	if err := h.emailSvc.SendWithConfig(r.Context(), cfg, msg); err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}
