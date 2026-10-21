package handler

import (
	"context"
	"log/slog"
	"net/http"

	"github.com/belune-hq/belune/internal/proxy/caddyconfig"
	"github.com/belune-hq/belune/internal/proxy/caddymetrics"
)

// ProxyAdminReader is what the Proxy page reads from Caddy's admin API. It is a
// separate, read-only interface — not part of ProxyManager — so nothing here can
// be mistaken for a way to change Caddy.
type ProxyAdminReader interface {
	Metrics(ctx context.Context) ([]byte, error)
	MetricsEnabled(ctx context.Context) (bool, error)
	RunningConfig(ctx context.Context) ([]byte, error)
}

// SetProxyAdmin wires the admin API reader. Called once at startup.
func (h *Handler) SetProxyAdmin(a ProxyAdminReader) {
	h.proxyAdmin = a
	h.proxyMetrics = caddymetrics.NewObserver(a)
}

// GetProxyMetrics reports how much traffic Caddy is proxying to apps and how it
// is going. Rates come from comparing scrapes, so the first reading after a
// start or a Caddy restart is "collecting" and carries no rate.
// GET /api/maintenance/proxy/metrics (admin only)
//
//apidoc:tag platform/maintenance
//apidoc:title Get Proxy Metrics
//apidoc:order 8
func (h *Handler) GetProxyMetrics(w http.ResponseWriter, r *http.Request) {
	// A nil observer is the "Caddy admin not wired" case and answers unknown.
	writeJSON(w, http.StatusOK, h.proxyMetrics.Observe(r.Context()))
}

// proxyConfigResponse is Caddy's live configuration with secrets removed.
type proxyConfigResponse struct {
	// RedactedFields counts the values replaced, so a reader can tell redaction
	// happened and is not mistaking the markers for Caddy's own content.
	RedactedFields int `json:"redacted_fields"`
	Config         any `json:"config"`
}

// GetProxyConfig returns Caddy's running configuration, read-only. Private keys
// and password hashes are removed here, on the server: a value that is sent and
// then hidden in the browser is still sent.
// GET /api/maintenance/proxy/config (admin only)
//
//apidoc:tag platform/maintenance
//apidoc:title Get Proxy Config
//apidoc:order 9
func (h *Handler) GetProxyConfig(w http.ResponseWriter, r *http.Request) {
	if h.proxyAdmin == nil {
		writeError(w, http.StatusServiceUnavailable, "proxy admin API unavailable")
		return
	}
	raw, err := h.proxyAdmin.RunningConfig(r.Context())
	if err != nil {
		slog.Warn("reading caddy running config", "error", err)
		writeError(w, http.StatusBadGateway, "could not read Caddy's configuration")
		return
	}
	cfg, n, err := caddyconfig.Redact(raw)
	if err != nil {
		// Never fall back to the raw bytes.
		slog.Error("redacting caddy running config", "error", err)
		writeError(w, http.StatusInternalServerError, "could not prepare Caddy's configuration for display")
		return
	}
	writeJSON(w, http.StatusOK, proxyConfigResponse{RedactedFields: n, Config: cfg})
}
