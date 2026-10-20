package caddy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"

	"github.com/belune-hq/belune/internal/pkg/metrics"
	"github.com/belune-hq/belune/internal/proxy"
)

// dashboardRouteID is the Belune dashboard's own route. It is kept separate from
// the application routes (route-<hostname>) because it is not backed by a row in
// the domains table — which is also why the reconciler has to be told not to
// sweep it away as stale.
const dashboardRouteID = "route-dashboard"

// SetDashboardRoute publishes the dashboard on its own hostname, so Caddy's
// automatic HTTPS obtains a certificate for it exactly as it does for an app
// domain. An empty hostname removes the route and the dashboard falls back to
// the catch-all, which answers on any host over plain HTTP — the bare-IP install.
//
// Serving the dashboard from a matcher-less catch-all is *why* it could never
// have TLS: automatic HTTPS derives the names it issues for from host matchers,
// and a catch-all contributes none.
//
// This is called on every reconcile pass, so it must be cheap and idempotent: it
// no-ops when Caddy already has the right route, rather than rewriting the config
// (and triggering a reload) every 30 seconds.
func (c *Client) SetDashboardRoute(ctx context.Context, hostname, sslMode string) (err error) {
	defer func() { metrics.RecordCaddyCall("set_dashboard_route", err) }()

	// Off means plain HTTP, so the redirect must go too. Left in, it would bounce
	// the operator to a port that has no certificate for this name — locking them
	// out of the panel they just switched to HTTP on purpose.
	forceHTTPS := sslMode != proxy.SSLModeOff

	current, currentForce, currentUpstream, found, err := c.dashboardRouteState(ctx)
	if err != nil {
		return err
	}
	if found && current == hostname && currentForce == forceHTTPS &&
		currentUpstream == c.dashboardUpstream {
		return nil // already correct
	}

	// Drop the old route: the hostname changed, the mode changed, or it is being
	// cleared.
	if found {
		if err := c.deleteRouteByID(ctx, dashboardRouteID); err != nil {
			return err
		}
		slog.Info("caddy: dashboard route removed", "hostname", current)
	}
	if hostname == "" {
		return nil
	}

	handles := make([]caddyHandle, 0, 2)
	if forceHTTPS {
		// The dashboard is a login form; there is no reason to serve it over plain
		// HTTP once it has a certificate.
		handles = append(handles, caddyHandle{
			"handler": "subroute",
			"routes": []map[string]any{{
				"match": []caddyMatcher{{"protocol": "http"}},
				"handle": []caddyHandle{{
					"handler":     "static_response",
					"headers":     map[string][]string{"Location": {"{http.request.scheme}s://{http.request.host}{http.request.uri}"}},
					"status_code": "301",
				}},
			}},
		})
	}
	handles = append(handles, newReverseProxyHandle([]caddyUpstream{{Dial: c.dashboardUpstream}}, ""))

	route := caddyRoute{
		ID:     dashboardRouteID,
		Match:  []caddyMatch{{Host: []string{hostname}}},
		Handle: handles,
	}

	body, err := json.Marshal(route)
	if err != nil {
		return fmt.Errorf("marshal dashboard route: %w", err)
	}

	url := fmt.Sprintf("%s/config/apps/http/servers/srv0/routes", c.adminURL)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		if isTransportError(err) {
			return fmt.Errorf("%w: %v", ErrCaddyUnreachable, err)
		}
		return fmt.Errorf("caddy add dashboard route: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("caddy add dashboard route: HTTP %d: %s", resp.StatusCode, string(respBody))
	}

	// The catch-all matches everything, so it must stay last or it would shadow
	// the dashboard route we just added.
	if err := c.reorderRoutes(ctx); err != nil {
		return fmt.Errorf("reorder catch-all: %w", err)
	}

	slog.Info("caddy: dashboard route published",
		"hostname", hostname, "ssl_mode", sslMode, "force_https", forceHTTPS, "upstream", c.dashboardUpstream)
	return nil
}

// dashboardRouteState reports what the dashboard route currently is: the hostname
// it serves, whether it renders the HTTP→HTTPS redirect, and where it dials.
//
// All three are read back, not just the hostname. Switching the mode between off
// and automatic changes only the handlers, and changing DASHBOARD_UPSTREAM changes
// only the dial address — comparing hostnames alone would call either "already
// correct" and never rewrite the route, leaving the dashboard on plain HTTP or
// pointed at an upstream that no longer exists.
func (c *Client) dashboardRouteState(ctx context.Context) (hostname string, forceHTTPS bool, upstream string, found bool, err error) {
	rawRoutes, err := c.fetchRawRoutes(ctx)
	if err != nil {
		if isTransportError(err) {
			return "", false, "", false, fmt.Errorf("%w: %v", ErrCaddyUnreachable, err)
		}
		return "", false, "", false, fmt.Errorf("fetch routes: %w", err)
	}

	for _, r := range rawRoutes {
		var probe struct {
			ID    string `json:"@id"`
			Match []struct {
				Host []string `json:"host"`
			} `json:"match"`
			Handle []struct {
				Handler   string `json:"handler"`
				Upstreams []struct {
					Dial string `json:"dial"`
				} `json:"upstreams"`
			} `json:"handle"`
		}
		if err := json.Unmarshal(r, &probe); err != nil {
			continue
		}
		if probe.ID != dashboardRouteID {
			continue
		}
		for _, h := range probe.Handle {
			switch h.Handler {
			case "subroute":
				forceHTTPS = true
			case "reverse_proxy":
				if len(h.Upstreams) > 0 {
					upstream = h.Upstreams[0].Dial
				}
			}
		}
		if len(probe.Match) > 0 && len(probe.Match[0].Host) > 0 {
			return probe.Match[0].Host[0], forceHTTPS, upstream, true, nil
		}
		return "", forceHTTPS, upstream, true, nil
	}
	return "", false, "", false, nil
}

// deleteRouteByID removes a route by its @id, treating "already gone" as success.
func (c *Client) deleteRouteByID(ctx context.Context, id string) error {
	url := fmt.Sprintf("%s/id/%s", c.adminURL, id)
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, url, nil)
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		if isTransportError(err) {
			return fmt.Errorf("%w: %v", ErrCaddyUnreachable, err)
		}
		return fmt.Errorf("delete route %s: %w", id, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 && resp.StatusCode != http.StatusNotFound {
		respBody, _ := io.ReadAll(resp.Body)
		if !bytes.Contains(respBody, []byte("unknown object")) {
			return fmt.Errorf("delete route %s: HTTP %d: %s", id, resp.StatusCode, string(respBody))
		}
	}
	return nil
}
