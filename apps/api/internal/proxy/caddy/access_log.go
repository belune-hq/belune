package caddy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
)

// ConfigureAccessLogs configures the Caddy HTTP server to write structured JSON
// access logs. The logPath written to Caddy is always the container-internal
// path (/var/log/caddy/access.log); the host-side path used by the log tailer
// is a separate concern handled by the caller.
func (c *Client) ConfigureAccessLogs(ctx context.Context) error {
	const loggerName = "http.access.srv0"
	// Path inside the Caddy container — the bind mount makes this visible on
	// the host at a different path (e.g. infra/caddy/logs/access.log).
	const caddyLogPath = "/var/log/caddy/access.log"

	// Step 1: create/replace the named logger. The logging section may not exist
	// yet (fresh Caddy) or may already be there (API restart against a live
	// Caddy), so neither PUT nor PATCH alone suffices.
	loggingCfg := map[string]any{
		"logs": map[string]any{
			loggerName: map[string]any{
				"writer": map[string]any{
					"output":         "file",
					"filename":       caddyLogPath,
					"roll_size_mb":   100,
					"roll_keep":      5,
					"roll_keep_days": 7, // verified via `caddy adapt` against caddy:2-alpine
					// Caddy defaults the log file to 0600 root-owned. Belune runs as
					// a non-root user and tails this file from its own container, so
					// the default made every read fail with "permission denied" and
					// request logging silently collected nothing in production.
					// Verified against caddy:2-alpine (2.11): the writer accepts
					// `mode` as a string and the file lands as -rw-r--r--.
					//
					// This file is world-readable and kept for 7 days, so nothing
					// secret may reach it — see the URI filter below, which exists
					// precisely because 0644 makes a leak here cheap to exploit.
					"mode": "0644",
				},
				// The deploy-hook token travels in the request path, and an access
				// log records the request line verbatim — so the raw URI would
				// persist here as a working credential. The filter encoder wraps
				// the JSON encoder and rewrites just that one field.
				"encoder": map[string]any{
					"format": "filter",
					"wrap":   map[string]any{"format": "json"},
					"fields": map[string]any{
						"request>uri": map[string]any{
							"filter": "regexp",
							// Only the token segment is replaced; the route stays
							// intact so these requests remain greppable.
							"regexp": `/api/webhooks/deploy/[^/?\s]+`,
							"value":  "/api/webhooks/deploy/[REDACTED]",
						},
					},
				},
				// ⚠️ A named log with no `include` is a catch-all: Caddy sent it
				// EVERY logger — ACME and TLS lines, and any module's own log, such
				// as a WAF rule match carrying the request URI and matched payload.
				// The filter above only rewrites `request>uri`, a field of access
				// entries, so a line of any other shape reached this 0644 file
				// unredacted. Scope it to the access entries the tailer reads
				// (Caddy names them "http.log.access." + default_logger_name).
				//
				// This does not take those lines out of Caddy's stderr: the default
				// log still carries them, and that is what the log collector and the
				// TLS status pipeline read.
				"include": []string{"http.log.access." + loggerName},
				"level":   "INFO",
			},
		},
	}
	if err := c.putOrPatchConfig(ctx, "/config/logging", loggingCfg); err != nil {
		return fmt.Errorf("configure access log writer: %w", err)
	}

	// Step 2: point srv0 to use that logger. ensureServer preserves srv0's `logs`
	// key, so on an API restart against a still-running Caddy it is already set and
	// a PUT would fail with 409 "key already exists" — hence create-or-replace.
	serverLogsCfg := map[string]any{
		"default_logger_name": loggerName,
	}
	if err := c.putOrPatchConfig(ctx, "/config/apps/http/servers/srv0/logs", serverLogsCfg); err != nil {
		return fmt.Errorf("configure server access logs: %w", err)
	}

	slog.Info("caddy access logging configured", "caddy_path", caddyLogPath)
	return nil
}

// putOrPatchConfig creates the value at path, replacing it if it already exists.
// Neither Caddy verb is idempotent on its own: PUT rejects an existing key with
// 409, PATCH rejects a missing one with 404. Both writes here re-run on every API
// start, against a Caddy that may or may not still hold the previous config.
func (c *Client) putOrPatchConfig(ctx context.Context, path string, payload any) error {
	status, err := c.doConfig(ctx, http.MethodPut, path, payload)
	if err != nil || status != http.StatusConflict {
		return err
	}
	_, err = c.doConfig(ctx, http.MethodPatch, path, payload)
	return err
}

// doConfig sends one config write and reports the response status. A transport
// error is non-fatal in dev (Caddy may not be running) and reported as status 0.
func (c *Client) doConfig(ctx context.Context, method, path string, payload any) (int, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return 0, fmt.Errorf("marshal payload: %w", err)
	}

	url := c.adminURL + path
	req, err := http.NewRequestWithContext(ctx, method, url, bytes.NewReader(body))
	if err != nil {
		return 0, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		// Non-fatal in dev: Caddy may not be running
		slog.Warn("caddy config update failed (caddy may not be running)", "method", method, "path", path, "error", err)
		return 0, nil
	}
	defer resp.Body.Close()

	// 409 is expected on the PUT half of putOrPatchConfig, which retries as PATCH.
	if resp.StatusCode >= 400 && resp.StatusCode != http.StatusConflict {
		respBody, _ := io.ReadAll(resp.Body)
		slog.Warn("caddy config update returned error", "method", method, "path", path, "status", resp.StatusCode, "body", string(respBody))
	}
	return resp.StatusCode, nil
}
