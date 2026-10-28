package caddy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"strings"

	"github.com/belune-hq/belune/internal/pkg/metrics"
	"github.com/belune-hq/belune/internal/proxy"
)

// serverPath is the one HTTP server Belune owns. trusted_proxies is a property of
// the server, not of a route, so none of the route machinery applies to it.
const serverPath = "/config/apps/http/servers/srv0"

// SyncTrustedProxies makes the server's trusted proxies match cfg, and does
// nothing when they already do. A write is a config reload, so it is a GET when
// nothing has changed. Like routes, the setting is lost when Caddy restarts (the
// static Caddyfile is what it reloads), so the reconciler re-asserts it each pass.
//
// ⚠️ trusted_proxies and trusted_proxies_strict are always written TOGETHER, and in
// an order that never leaves ranges standing without strict. Caddy reads
// X-Forwarded-For left to right by default: a visitor behind a trusted CDN
// pre-seeds the header, the CDN appends the real address, and the pre-seeded one
// wins — the client chooses the address Belune records. Strict reads it right to
// left. Strict with no ranges does nothing, so it goes first when adding and last
// when removing. Measured against Caddy v2.11.7.
//
// A value Caddy refuses changes nothing: the admin API loads a config whole or not
// at all (a 500, and the running config is as it was), so a bad value cannot take
// the proxy down — it can only fail to apply, which is returned.
func (c *Client) SyncTrustedProxies(ctx context.Context, cfg proxy.TrustedProxiesConfig) (err error) {
	defer func() { metrics.RecordCaddyCall("sync_trusted_proxies", err) }()

	server, err := c.fetchServer(ctx)
	if err != nil {
		return err
	}
	_, hasRanges := server["trusted_proxies"]
	strict := serverStrict(server)

	if len(cfg.CIDRs) == 0 {
		if !hasRanges && !hasStrictKey(server) {
			return nil
		}
		// Ranges first: strict alone is harmless, ranges without it are not.
		if err := c.adminDelete(ctx, serverPath+"/trusted_proxies"); err != nil {
			return fmt.Errorf("remove trusted_proxies: %w", err)
		}
		if err := c.adminDelete(ctx, serverPath+"/trusted_proxies_strict"); err != nil {
			return fmt.Errorf("remove trusted_proxies_strict: %w", err)
		}
		slog.Info("caddy: trusted proxies removed")
		return nil
	}

	if !strict {
		if err := c.adminUpsert(ctx, serverPath+"/trusted_proxies_strict", 1); err != nil {
			return fmt.Errorf("set trusted_proxies_strict: %w", err)
		}
	}
	if !trustedProxiesEqual(server["trusted_proxies"], cfg.CIDRs) {
		desired := map[string]any{"source": "static", "ranges": cfg.CIDRs}
		if err := c.adminUpsert(ctx, serverPath+"/trusted_proxies", desired); err != nil {
			return fmt.Errorf("set trusted_proxies: %w", err)
		}
		slog.Info("caddy: trusted proxies updated", "ranges", len(cfg.CIDRs))
	}
	return nil
}

func hasStrictKey(server map[string]any) bool {
	_, ok := server["trusted_proxies_strict"]
	return ok
}

// serverStrict reports whether strict parsing is on. Caddy stores it as a number
// in JSON; a bool or a zero is not "on".
func serverStrict(server map[string]any) bool {
	switch v := server["trusted_proxies_strict"].(type) {
	case float64:
		return v == 1
	}
	return false
}

// trustedProxiesEqual compares what Caddy holds with what is wanted. Anything but
// exactly {source: static, ranges: [...]} with the same ranges is a difference —
// including an extra field, which Belune did not write.
func trustedProxiesEqual(current any, want []string) bool {
	m, ok := current.(map[string]any)
	if !ok || len(m) != 2 || m["source"] != "static" {
		return false
	}
	raw, ok := m["ranges"].([]any)
	if !ok {
		return false
	}
	have := make([]string, 0, len(raw))
	for _, r := range raw {
		s, ok := r.(string)
		if !ok {
			return false
		}
		have = append(have, s)
	}
	slices.Sort(have)
	w := slices.Clone(want)
	slices.Sort(w)
	return slices.Equal(have, w)
}

// adminUpsert sets a key whether or not it exists: Caddy's PUT only creates (409
// when present) and PATCH only replaces (404 when absent).
func (c *Client) adminUpsert(ctx context.Context, path string, v any) error {
	status, body, err := c.adminDo(ctx, http.MethodPut, path, v)
	if err != nil {
		return err
	}
	if status == http.StatusConflict {
		status, body, err = c.adminDo(ctx, http.MethodPatch, path, v)
		if err != nil {
			return err
		}
	}
	if status >= 400 {
		return fmt.Errorf("HTTP %d: %s", status, strings.TrimSpace(body))
	}
	return nil
}

// adminDelete removes a key, treating one that is already gone as removed.
func (c *Client) adminDelete(ctx context.Context, path string) error {
	status, body, err := c.adminDo(ctx, http.MethodDelete, path, nil)
	if err != nil {
		return err
	}
	if status >= 400 && status != http.StatusNotFound {
		return fmt.Errorf("HTTP %d: %s", status, strings.TrimSpace(body))
	}
	return nil
}

func (c *Client) adminDo(ctx context.Context, method, path string, v any) (int, string, error) {
	var rdr io.Reader
	if v != nil {
		b, err := json.Marshal(v)
		if err != nil {
			return 0, "", fmt.Errorf("marshal: %w", err)
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.adminURL+path, rdr)
	if err != nil {
		return 0, "", fmt.Errorf("create request: %w", err)
	}
	if v != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		if isTransportError(err) {
			return 0, "", fmt.Errorf("%w: %v", ErrCaddyUnreachable, err)
		}
		return 0, "", err
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	return resp.StatusCode, string(body), nil
}
