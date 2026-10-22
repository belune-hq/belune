package caddy

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
)

// maxAdminRead bounds what a read-only admin fetch will hold in memory. The
// running config grows with every domain and certificate, so the bound is
// generous, but it is finite: this runs on a request path.
const maxAdminRead = 32 << 20

func (c *Client) adminGet(ctx context.Context, path string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.adminURL+path, nil)
	if err != nil {
		return nil, fmt.Errorf("caddy GET %s: build request: %w", path, err)
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("caddy GET %s: %w", path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("caddy GET %s: HTTP %d", path, resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxAdminRead))
	if err != nil {
		return nil, fmt.Errorf("caddy GET %s: reading body: %w", path, err)
	}
	return body, nil
}

// Metrics returns Caddy's Prometheus exposition from the admin API.
func (c *Client) Metrics(ctx context.Context) ([]byte, error) {
	return c.adminGet(ctx, "/metrics")
}

// MetricsEnabled reports whether Caddy is configured to record HTTP metrics.
// Without it /metrics still answers, but with no caddy_http_* series — which is
// indistinguishable, from the exposition alone, from a Caddy that simply has not
// proxied anything yet. The config is the only place that tells them apart.
func (c *Client) MetricsEnabled(ctx context.Context) (bool, error) {
	body, err := c.adminGet(ctx, "/config/apps/http/metrics")
	if err != nil {
		return false, err
	}
	// An unset path reads back as JSON null; an enabled one as an object.
	return !bytes.Equal(bytes.TrimSpace(body), []byte("null")), nil
}

// RunningConfig returns Caddy's live configuration, UNREDACTED. It holds the
// private key of every custom-TLS domain, so it must pass through
// caddyconfig.Redact before it leaves the process.
func (c *Client) RunningConfig(ctx context.Context) ([]byte, error) {
	return c.adminGet(ctx, "/config/")
}
