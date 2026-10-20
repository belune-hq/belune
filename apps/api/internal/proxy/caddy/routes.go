package caddy

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"reflect"
	"regexp"
	"sort"
	"strings"

	"github.com/belune-hq/belune/internal/pkg/metrics"
	"github.com/belune-hq/belune/internal/proxy"
)

// ErrCaddyUnreachable is returned when the Caddy admin API cannot be contacted
// (DNS miss, connection refused, timeout). Callers at the service layer can
// treat this as "degraded in dev" while real schema / validation errors
// continue to surface as regular errors.
var ErrCaddyUnreachable = errors.New("caddy admin API unreachable")

// isTransportError returns true for network-level failures that should surface
// as ErrCaddyUnreachable. Schema errors from Caddy come back as HTTP 4xx/5xx
// responses, so those are handled separately.
func isTransportError(err error) bool {
	if err == nil {
		return false
	}
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		return true
	}
	var netErr net.Error
	return errors.As(err, &netErr)
}

// caddyMatcher is a generic Caddy matcher with arbitrary fields.
type caddyMatcher map[string]any

const catchAllRouteID = "route-catch-all"

// caddyRoute represents a Caddy route configuration.
type caddyRoute struct {
	ID     string        `json:"@id"`
	Match  []caddyMatch  `json:"match,omitempty"`
	Handle []caddyHandle `json:"handle"`
}

type caddyMatch struct {
	Host []string `json:"host"`
	// omitempty matters: a whole-host route must serialise with no path matcher
	// at all, exactly as it did before paths existed, so upgrading does not make
	// every existing route look drifted and rewrite it.
	Path []string `json:"path,omitempty"`
}

// caddyHandle is a flexible map-based handler to support all Caddy handler types
// (reverse_proxy, authentication, headers, subroute, static_response, etc.).
type caddyHandle map[string]any

type caddyUpstream struct {
	Dial string `json:"dial"`
}

// newReverseProxyHandle creates a reverse_proxy handler. If healthCheckPath
// is non-empty, an active upstream health check is configured so Caddy itself
// will mark the upstream unhealthy on consecutive 5xx responses — separate
// from the deploy-time verification probe, which gates whether a deploy ever
// goes live in the first place.
func newReverseProxyHandle(upstreams []caddyUpstream, healthCheckPath string) caddyHandle {
	h := caddyHandle{
		"handler":   "reverse_proxy",
		"upstreams": upstreams,
	}
	if healthCheckPath != "" {
		h["health_checks"] = map[string]any{
			"active": map[string]any{
				"uri":           healthCheckPath,
				"interval":      "30s",
				"timeout":       "5s",
				"expect_status": 2,
			},
		}
	}
	return h
}

// InitCatchAll must be called once at startup. It normalises the routes list so
// that all domain-specific routes come first and the catch-all (dashboard proxy)
// is last with a known @id. This makes every subsequent AddRoute call safe to
// use simple appending — we just move the catch-all to the end afterwards.
func (c *Client) InitCatchAll(ctx context.Context) {
	rawRoutes, err := c.fetchRawRoutes(ctx)
	if err != nil {
		slog.Warn("caddy: InitCatchAll failed to fetch routes", "error", err)
		return
	}

	// Keep only domain-specific routes; drop any catch-alls (no host matcher).
	var domainRoutes []json.RawMessage
	for _, r := range rawRoutes {
		var probe struct {
			ID    string `json:"@id"`
			Match []struct {
				Host []string `json:"host"`
			} `json:"match"`
		}
		if err := json.Unmarshal(r, &probe); err != nil {
			continue
		}
		if probe.ID == catchAllRouteID {
			continue // will be re-appended with correct ID below
		}
		if len(probe.Match) > 0 && len(probe.Match[0].Host) > 0 {
			domainRoutes = append(domainRoutes, r)
		}
		// anonymous catch-alls (no match) are dropped
	}

	// Re-append catch-all with known @id at the end.
	catchAllJSON, _ := json.Marshal(caddyRoute{
		ID:     catchAllRouteID,
		Handle: []caddyHandle{newReverseProxyHandle([]caddyUpstream{{Dial: c.dashboardUpstream}}, "")},
	})
	newRoutes := append(domainRoutes, catchAllJSON)

	if err := c.ensureServer(ctx, newRoutes); err != nil {
		slog.Warn("caddy: InitCatchAll failed to write server config", "error", err)
		return
	}
	slog.Info("caddy catch-all route initialised", "listen", serverListen)
}

// buildRoute renders a route config into the exact Caddy route it should become.
// Pulled out of AddRoute so the same construction can be compared against what
// Caddy actually holds — see EnsureRoute.
func buildRoute(cfg proxy.RouteConfig) (caddyRoute, error) {
	// Build the handler chain: feature handlers first, then reverse_proxy as the terminal handler.
	handlers, err := buildFeatureHandlers(cfg)
	if err != nil {
		return caddyRoute{}, fmt.Errorf("build handlers: %w", err)
	}

	// Strip the public prefix immediately before proxying, so the app sees the
	// path it was written for: mounted at /api, it gets /users, not /api/users.
	//
	// This sits after the feature handlers on purpose. Basic auth, an IP
	// allowlist and a redirect are all written against the *public* URL — the one
	// the operator typed and the visitor sees — so they must run while the path
	// is still the public one. Strip first and a rule guarding /api/admin would
	// silently stop matching.
	if cfg.StripPath && !isRootPath(cfg.Path) {
		handlers = append(handlers, caddyHandle{
			"handler":           "rewrite",
			"strip_path_prefix": cfg.Path,
		})
	}

	// Then prepend the internal prefix, for an app that serves everything under a
	// base path of its own. Strip first, prepend second — the composed result is
	// what the container receives.
	//
	// {http.request.uri} rather than {http.request.uri.path}: it carries the query
	// string too, and rewriting to the bare path would silently drop ?page=2 from
	// every request the app ever sees.
	if cfg.InternalPath != "" {
		handlers = append(handlers, caddyHandle{
			"handler": "rewrite",
			"uri":     cfg.InternalPath + "{http.request.uri}",
		})
	}

	// Reverse proxy is always the last handler.
	handlers = append(handlers, newReverseProxyHandle(
		[]caddyUpstream{{Dial: targetToDial(cfg.TargetURL)}},
		cfg.HealthCheckPath,
	))

	// If ForceHTTPS is enabled, prepend an HTTP→HTTPS redirect subroute.
	if cfg.ForceHTTPS && cfg.SSLMode != proxy.SSLModeOff {
		handlers = append([]caddyHandle{
			{
				"handler": "subroute",
				"routes": []map[string]any{{
					"match": []caddyMatcher{{"protocol": "http"}},
					"handle": []caddyHandle{{
						"handler":     "static_response",
						"headers":     map[string][]string{"Location": {"{http.request.scheme}s://{http.request.host}{http.request.uri}"}},
						"status_code": "301",
					}},
				}},
			},
		}, handlers...)
	}

	return caddyRoute{
		ID: routeIDFor(cfg.Hostname, cfg.Path),
		Match: []caddyMatch{
			{Host: []string{cfg.Hostname}, Path: pathMatcher(cfg.Path)},
		},
		Handle: handlers,
	}, nil
}

// isRootPath reports whether a path covers the whole host. The empty string is
// included deliberately: RouteConfig values built before paths existed (and any
// test that omits the field) mean "the whole host", and must not silently become
// a route that matches nothing.
func isRootPath(path string) bool { return path == "" || path == "/" }

// pathMatcher is the Caddy path matcher for a prefix, or nil for a whole-host
// route (no matcher at all — matching every path is the absence of a constraint,
// not a constraint of "/*").
//
// Two entries, not one: "/api" alone matches only the exact path, and "/api/*"
// alone does not match "/api" itself. An app mounted at /api must answer both,
// or its own root 404s. Neither matches "/apifoo", which is the point.
func pathMatcher(path string) []string {
	if isRootPath(path) {
		return nil
	}
	return []string{path, path + "/*"}
}

// routeIDFor is the @id Caddy stores a domain's route under.
//
// A whole-host route keeps the historical "route-<hostname>" exactly, so
// upgrading an existing install does not orphan every route it already has.
//
// A path route cannot simply append the path: the id is interpolated into the
// admin API's URL ("/id/<id>"), so a literal "/" in it reads as another path
// segment and Caddy rejects it as a traversal. The path is therefore slugified
// for legibility and suffixed with a short digest of the exact original, which
// is what actually guarantees uniqueness — "/api/v2" and "/api-v2" slugify the
// same but must never collide onto one route.
func routeIDFor(hostname, path string) string {
	if isRootPath(path) {
		return fmt.Sprintf("route-%s", hostname)
	}
	sum := sha256.Sum256([]byte(path))
	return fmt.Sprintf("route-%s-%s-%x", hostname, pathSlug(path), sum[:3])
}

// pathFromMatcher is the inverse of pathMatcher: it recovers the configured
// prefix from what Caddy holds. No matcher means the whole host, i.e. "/".
func pathFromMatcher(match []string) string {
	if len(match) == 0 {
		return "/"
	}
	// pathMatcher writes the bare prefix first and "<prefix>/*" second; take the
	// one that is not the wildcard so a round trip is exact.
	for _, p := range match {
		if !strings.HasSuffix(p, "/*") {
			return p
		}
	}
	return strings.TrimSuffix(match[0], "/*")
}

var nonSlugChars = regexp.MustCompile(`[^a-zA-Z0-9]+`)

func pathSlug(path string) string {
	return strings.Trim(nonSlugChars.ReplaceAllString(path, "-"), "-")
}

func (c *Client) AddRoute(ctx context.Context, cfg proxy.RouteConfig) (err error) {
	defer func() { metrics.RecordCaddyCall("add_route", err) }()
	// Remove any existing route for this host+path first to prevent duplicates.
	// Ignore "not found" — normal on fresh routes.
	if err := c.RemoveRoute(ctx, cfg.Hostname, cfg.Path); err != nil && !errors.Is(err, ErrCaddyUnreachable) {
		slog.Debug("caddy: pre-remove returned non-fatal error", "hostname", cfg.Hostname, "path", cfg.Path, "error", err)
	}

	route, err := buildRoute(cfg)
	if err != nil {
		return err
	}

	body, err := json.Marshal(route)
	if err != nil {
		return fmt.Errorf("marshal route: %w", err)
	}

	// Everything TLS must be in place *before* the route lands: srv0 listens on
	// :443, so the moment a host matcher appears Caddy runs automatic HTTPS for
	// that name — issuing a doomed certificate for an ssl_mode=off domain, or
	// minting an internal one that would then be served in preference to the
	// operator's uploaded certificate. Failures here are not fatal (the route
	// itself is still correct); the reconciler re-asserts both on its next pass.
	if c.setMode(cfg.Hostname, cfg.SSLMode) {
		if err := c.ensureServer(ctx, nil); err != nil {
			slog.Warn("caddy: failed to update auto-HTTPS exclusions", "hostname", cfg.Hostname, "error", err)
		}
	}
	if cfg.TLS && cfg.SSLMode == proxy.SSLModeCustom {
		if err := c.SetupTLS(ctx, cfg.Hostname, cfg.SSLMode, cfg.CertPEM, cfg.KeyPEM); err != nil {
			// The route still goes live — serving the app over HTTP beats serving
			// nothing — but the failure is no longer invisible: it lands on the
			// domain's tls_error and the badge turns red with the reason.
			slog.Warn("caddy: TLS setup failed", "hostname", cfg.Hostname, "error", err)
			c.reportTLSError(ctx, cfg.Hostname, err.Error())
		}
	}

	// Append the domain route; reorderRoutes below puts the array back in
	// first-match-wins order, so where it lands right now does not matter.
	appendURL := fmt.Sprintf("%s/config/apps/http/servers/srv0/routes", c.adminURL)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, appendURL, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		if isTransportError(err) {
			return fmt.Errorf("%w: %v", ErrCaddyUnreachable, err)
		}
		return fmt.Errorf("caddy add route: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("caddy add route: HTTP %d: %s", resp.StatusCode, string(respBody))
	}

	// Re-sort: the catch-all must stay last, and a path route must sit ahead of
	// the whole-host route for the same name or it would never be reached.
	if err := c.reorderRoutes(ctx); err != nil {
		return fmt.Errorf("reorder catch-all: %w", err)
	}

	slog.Info("caddy route added", "hostname", cfg.Hostname, "target", cfg.TargetURL)
	return nil
}

// buildFeatureHandlers translates RouteFeatures into Caddy handler JSON.
// Feature configs are parsed through proxy.ParseFeatureConfig so any
// malformed payload is dropped with a warning rather than rendered as a
// broken Caddy config.
func buildFeatureHandlers(cfg proxy.RouteConfig) ([]caddyHandle, error) {
	var handlers []caddyHandle

	for _, f := range cfg.Features {
		if !f.Enabled {
			continue
		}
		parsed, err := proxy.ParseFeatureConfig(f.Type, f.Config)
		if err != nil {
			// Name the domain: this is how an operator finds the offending row —
			// notably a rate_limit row stored in the older rate/burst form,
			// which no longer parses. Skipping
			// keeps the rest of the route building — dropping the whole route
			// would take a live site down over one bad feature.
			slog.Warn("caddy: skipping invalid route feature",
				"hostname", cfg.Hostname, "type", f.Type, "error", err)
			continue
		}

		switch c := parsed.(type) {
		case *proxy.BasicAuthConfig:
			handlers = append(handlers, caddyHandle{
				"handler": "authentication",
				"providers": map[string]any{
					"http_basic": map[string]any{
						"accounts": []map[string]string{{
							"username": c.Username,
							"password": c.HashedPassword,
						}},
					},
				},
			})

		case *proxy.HeadersConfig:
			h := caddyHandle{"handler": "headers"}
			if c.Request != nil {
				h["request"] = headerOpsToMap(c.Request)
			}
			if c.Response != nil {
				h["response"] = headerOpsToMap(c.Response)
			}
			handlers = append(handlers, h)

		case *proxy.RateLimitConfig:
			// Both of these are fixed on purpose, not oversights:
			//  - the zone is derived from the route, never operator-supplied,
			//    because zones with the same name share ONE counter across
			//    domains — two hosts both set to "10/s" would eat each other's
			//    budget;
			//  - the key is the client IP. Omitted, the module uses a single
			//    global bucket, which from one client looks identical to
			//    per-IP working. (Behind another proxy, remote.host is that
			//    proxy.)
			// Caddy hands "window" back verbatim, not normalised to nanoseconds, so
			// routeJSONEqual sees no drift and EnsureRoute does not re-POST. That
			// matters: a re-POST resets every client's counter, so a re-POST per
			// reconcile would make any window at or above the interval
			// unenforceable. If a Caddy upgrade ever normalises it, this breaks.
			// Nothing else from the module's handler is emitted: "distributed"
			// would silently turn on distributed limiting with default storage.
			handlers = append(handlers, caddyHandle{
				"handler": "rate_limit",
				"rate_limits": map[string]any{
					routeIDFor(cfg.Hostname, cfg.Path): map[string]any{
						"key":        "{http.request.remote.host}",
						"window":     c.Window,
						"max_events": c.MaxEvents,
					},
				},
			})

		case *proxy.IPAllowlistConfig:
			handlers = append(handlers, caddyHandle{
				"handler": "subroute",
				"routes": []map[string]any{{
					"match": []caddyMatcher{{"not": []caddyMatcher{{"remote_ip": map[string]any{"ranges": c.Ranges}}}}},
					"handle": []caddyHandle{{
						"handler":     "static_response",
						"status_code": "403",
						"body":        "Forbidden",
					}},
				}},
			})

		case *proxy.RedirectConfig:
			handlers = append(handlers, caddyHandle{
				"handler": "subroute",
				"routes": []map[string]any{{
					"match": []caddyMatcher{{"path": []string{c.From}}},
					"handle": []caddyHandle{{
						"handler":     "static_response",
						"headers":     map[string][]string{"Location": {c.To}},
						"status_code": fmt.Sprintf("%d", c.StatusCode),
					}},
				}},
			})

		}
	}

	// Append advanced config handlers if provided.
	if len(cfg.AdvancedConfig) > 0 {
		advHandlers, err := parseAdvancedConfig(cfg.AdvancedConfig)
		if err != nil {
			return nil, err
		}
		handlers = append(handlers, advHandlers...)
	}

	return handlers, nil
}

// headerOpsToMap converts the typed HeaderOps to the map shape Caddy expects.
// Nil sub-fields are omitted so the resulting JSON stays compact.
func headerOpsToMap(ops *proxy.HeaderOps) map[string]any {
	m := map[string]any{}
	if len(ops.Set) > 0 {
		m["set"] = ops.Set
	}
	if len(ops.Add) > 0 {
		m["add"] = ops.Add
	}
	if len(ops.Delete) > 0 {
		m["delete"] = ops.Delete
	}
	return m
}

// advancedHandlerAllowlist lists Caddy handler modules that are safe to expose
// through the per-domain AdvancedConfig field. Anything outside this list is
// rejected so user-supplied JSON cannot attach arbitrary modules (e.g. a
// reverse_proxy that routes to an attacker-controlled upstream).
var advancedHandlerAllowlist = map[string]struct{}{
	"headers":        {},
	"rewrite":        {},
	"redir":          {},
	"handle_path":    {},
	"subroute":       {},
	"authentication": {}, // scoped to http_basic by parser below
}

// parseAdvancedConfig validates user-supplied AdvancedConfig JSON. The input
// must decode to a JSON array of Caddy handlers ([]caddyHandle). Each handler
// must declare a "handler" key from the allowlist. Errors list every
// disallowed handler so the user sees the full diagnosis in one round trip.
func parseAdvancedConfig(raw []byte) ([]caddyHandle, error) {
	var handlers []caddyHandle
	if err := json.Unmarshal(raw, &handlers); err != nil {
		return nil, fmt.Errorf("advanced_config: expected a JSON array of handlers: %w", err)
	}
	var bad []string
	for i, h := range handlers {
		name, _ := h["handler"].(string)
		if name == "" {
			bad = append(bad, fmt.Sprintf("#%d: missing \"handler\" key", i))
			continue
		}
		if _, ok := advancedHandlerAllowlist[name]; !ok {
			bad = append(bad, fmt.Sprintf("#%d: %q is not allowed", i, name))
		}
	}
	if len(bad) > 0 {
		return nil, fmt.Errorf("advanced_config: disallowed handlers: %s", strings.Join(bad, "; "))
	}
	return handlers, nil
}

func (c *Client) RemoveRoute(ctx context.Context, hostname, path string) (err error) {
	defer func() { metrics.RecordCaddyCall("remove_route", err) }()
	// routeIDFor, not a second hand-built "route-%s". The id scheme now depends on
	// the path, and a delete that computes it differently from the write would
	// leave the real route in place while reporting success.
	routeID := routeIDFor(hostname, path)
	deleteURL := fmt.Sprintf("%s/id/%s", c.adminURL, routeID)

	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, deleteURL, nil)
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		if isTransportError(err) {
			return fmt.Errorf("%w: %v", ErrCaddyUnreachable, err)
		}
		return fmt.Errorf("caddy remove route: %w", err)
	}
	defer resp.Body.Close()

	// 404 / 500 "unknown object" is treated as success — the route is gone,
	// which is exactly what the caller asked for. Any other 4xx/5xx is an error.
	if resp.StatusCode >= 400 {
		respBody, _ := io.ReadAll(resp.Body)
		bodyStr := string(respBody)
		if resp.StatusCode == http.StatusNotFound || strings.Contains(bodyStr, "unknown object") {
			slog.Debug("caddy: route not found on delete", "hostname", hostname)
			return nil
		}
		return fmt.Errorf("caddy remove route: HTTP %d: %s", resp.StatusCode, bodyStr)
	}

	// The hostname no longer exists, so it has no business in either exclusion set.
	if c.forgetHost(hostname) {
		if err := c.ensureServer(ctx, nil); err != nil {
			slog.Warn("caddy: failed to prune auto-HTTPS exclusions", "hostname", hostname, "error", err)
		}
	}

	slog.Info("caddy route removed", "hostname", hostname)
	return nil
}

func (c *Client) ListRoutes(ctx context.Context) (result []proxy.RouteConfig, err error) {
	defer func() { metrics.RecordCaddyCall("list_routes", err) }()
	url := fmt.Sprintf("%s/config/apps/http/servers/srv0/routes", c.adminURL)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("caddy list routes: %w", err)
	}
	defer resp.Body.Close()

	var routes []caddyRoute
	if err := json.NewDecoder(resp.Body).Decode(&routes); err != nil {
		return nil, fmt.Errorf("decode routes: %w", err)
	}

	for _, route := range routes {
		if len(route.Match) > 0 && len(route.Match[0].Host) > 0 {
			cfg := proxy.RouteConfig{
				Hostname: route.Match[0].Host[0],
				// Read the path back too. The reconciler diffs what Caddy holds
				// against what the database says; a route whose path it could not
				// see would look like a stale whole-host route on every pass and be
				// removed and re-added for ever.
				Path: pathFromMatcher(route.Match[0].Path),
			}
			// Extract target URL from the last handler (reverse_proxy).
			for i := len(route.Handle) - 1; i >= 0; i-- {
				h := route.Handle[i]
				if h["handler"] == "reverse_proxy" {
					if upstreams, ok := h["upstreams"].([]any); ok && len(upstreams) > 0 {
						if u, ok := upstreams[0].(map[string]any); ok {
							if dial, ok := u["dial"].(string); ok {
								cfg.TargetURL = dial
							}
						}
					}
					break
				}
			}
			result = append(result, cfg)
		}
	}

	return result, nil
}

// reorderRoutes rewrites srv0.routes into the order Caddy must see them in:
// most specific path first, then the catch-all last. Uses a single PATCH on the
// routes array so that two concurrent AddRoute callers cannot observe an
// interleaved state where a route has been deleted but not yet re-appended.
//
// Caddy is first-match-wins on an ordered array. Routes used to be appended in
// whatever order they were created and that was harmless, because every route
// matched a whole host and hosts are disjoint — no two could ever both match a
// request. Paths break that: shop.com/ and shop.com/api both match a request for
// shop.com/api/users, and the one Caddy reaches first wins outright. Created in
// the wrong order — and creation order is just whatever the operator did first —
// the root route silently swallows every request meant for the API, which looks
// exactly like the API app being broken.
//
// The array is read back from Caddy, never rebuilt from the database, and that is
// load-bearing. The PATCH below is atomic: measured on stock caddy:2-alpine, one
// route naming an absent module (http.handlers.rate_limit) gets the WHOLE patch
// rejected with HTTP 500. Caddy only holds routes it already accepted, so this
// array is always loadable; an array assembled from stored rows is not, and
// would turn one unsatisfiable domain into a reconcile that can never land.
func (c *Client) reorderRoutes(ctx context.Context) error {
	rawRoutes, err := c.fetchRawRoutes(ctx)
	if err != nil {
		if isTransportError(err) {
			return fmt.Errorf("%w: %v", ErrCaddyUnreachable, err)
		}
		return fmt.Errorf("fetch routes: %w", err)
	}

	var domainRoutes []json.RawMessage
	var catchAll json.RawMessage
	for _, r := range rawRoutes {
		var probe struct {
			ID    string `json:"@id"`
			Match []struct {
				Host []string `json:"host"`
			} `json:"match"`
		}
		_ = json.Unmarshal(r, &probe)
		if probe.ID == catchAllRouteID {
			catchAll = r
			continue
		}
		// A route with no host matcher is the Caddyfile's default site, which
		// every Caddy start re-creates as an anonymous catch-all. InitCatchAll
		// drops it, but only at API startup — so a Caddy restarted on its own left
		// it in place, and because it matches no host it sorts AHEAD of every
		// real hostname (key "" < any name): each reconcile restored the domain
		// routes and then ordered the shadowing route back to index 0, so every
		// domain served the dashboard. Dropping it here self-heals within the next
		// add. Still read from Caddy, never rebuilt from the database (see above).
		if len(probe.Match) == 0 || len(probe.Match[0].Host) == 0 {
			slog.Warn("caddy: dropping a route with no host matcher that would shadow every domain",
				"id", probe.ID)
			continue
		}
		domainRoutes = append(domainRoutes, r)
	}

	sortRoutesBySpecificity(domainRoutes)

	// Synthesise a catch-all if none exists yet (fresh server).
	if len(catchAll) == 0 {
		catchAll, err = json.Marshal(caddyRoute{
			ID:     catchAllRouteID,
			Handle: []caddyHandle{newReverseProxyHandle([]caddyUpstream{{Dial: c.dashboardUpstream}}, "")},
		})
		if err != nil {
			return fmt.Errorf("marshal catch-all: %w", err)
		}
	}
	newRoutes := append(domainRoutes, catchAll)

	return c.patchRoutes(ctx, newRoutes)
}

// sortRoutesBySpecificity orders routes longest-path-first, so /api/v2 is
// consulted before /api and both before the whole-host route for the same name.
//
// The ordering is total and deterministic — depth, then path, then host — not
// merely "sorted enough". Two routes that compare equal would be left in
// fetch order, which Caddy does not promise to be stable, so the config would
// churn between passes and every reconcile would look like drift.
func sortRoutesBySpecificity(routes []json.RawMessage) {
	type key struct {
		depth int
		path  string
		host  string
	}
	keyOf := func(raw json.RawMessage) key {
		var probe struct {
			Match []caddyMatch `json:"match"`
		}
		if err := json.Unmarshal(raw, &probe); err != nil || len(probe.Match) == 0 {
			return key{}
		}
		var host string
		if len(probe.Match[0].Host) > 0 {
			host = probe.Match[0].Host[0]
		}
		path := pathFromMatcher(probe.Match[0].Path)
		// Segment count, not string length: "/ab" must not outrank "/a/b".
		depth := 0
		if !isRootPath(path) {
			depth = strings.Count(strings.Trim(path, "/"), "/") + 1
		}
		return key{depth: depth, path: path, host: host}
	}

	sort.SliceStable(routes, func(i, j int) bool {
		a, b := keyOf(routes[i]), keyOf(routes[j])
		if a.depth != b.depth {
			return a.depth > b.depth // deeper prefix wins
		}
		if a.path != b.path {
			return a.path < b.path
		}
		return a.host < b.host
	})
}

// patchRoutes replaces the srv0 routes array atomically via a single PATCH.
func (c *Client) patchRoutes(ctx context.Context, routes []json.RawMessage) error {
	body, err := json.Marshal(routes)
	if err != nil {
		return fmt.Errorf("marshal routes: %w", err)
	}
	patchURL := fmt.Sprintf("%s/config/apps/http/servers/srv0/routes", c.adminURL)
	req, err := http.NewRequestWithContext(ctx, http.MethodPatch, patchURL, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		if isTransportError(err) {
			return fmt.Errorf("%w: %v", ErrCaddyUnreachable, err)
		}
		return fmt.Errorf("patch routes: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("patch routes: HTTP %d: %s", resp.StatusCode, string(respBody))
	}
	return nil
}

// fetchRawRoutes returns the current routes as raw JSON messages.
func (c *Client) fetchRawRoutes(ctx context.Context) ([]json.RawMessage, error) {
	url := fmt.Sprintf("%s/config/apps/http/servers/srv0/routes", c.adminURL)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var routes []json.RawMessage
	if err := json.NewDecoder(resp.Body).Decode(&routes); err != nil {
		return nil, err
	}
	return routes, nil
}

// serverListen is the listener set srv0 must always have. The HTTPS listener is
// asserted from here rather than the Caddyfile because the Caddyfile adapter
// emits one server per port: `:80, :443` would produce srv0=:443 + srv1=:80,
// stranding every route below (all of which address srv0 by name) on the wrong
// server. Without :443 in this list Caddy applies no automatic HTTPS at all.
var serverListen = []string{":80", ":443"}

// ensureServer writes Belune's required srv0 configuration: the dual listener,
// the automatic-HTTPS policy, and a TLS connection policy (without at least one
// policy Caddy accepts TCP on :443 but never completes a handshake). Passing a
// non-nil routes replaces the route table; nil leaves it untouched.
//
// Caddy's PATCH replaces the whole object at the given path, and PATCH cannot
// create a key that does not exist yet (tls_connection_policies is absent from
// the adapted Caddyfile), so this reads the current server and merges into it —
// preserving keys it does not own, notably the `logs` set by ConfigureAccessLogs.
func (c *Client) ensureServer(ctx context.Context, routes []json.RawMessage) error {
	server, err := c.fetchServer(ctx)
	if err != nil {
		return err
	}

	server["listen"] = serverListen
	skip, skipCerts := c.skipLists()
	server["automatic_https"] = map[string]any{
		// Belune owns HTTP→HTTPS redirection per-domain via the ForceHTTPS
		// subroute in AddRoute; Caddy's own redirect routes would shadow it.
		"disable_redirects": true,
		// ssl_mode=off: no automatic HTTPS for these names at all.
		"skip": skip,
		// ssl_mode=custom: the operator's certificate is loaded in-band, so Caddy
		// must not manage one of its own — without this it mints an internal-CA
		// cert for the name and serves that in preference to the uploaded one.
		"skip_certificates": skipCerts,
	}
	server["tls_connection_policies"] = []map[string]any{{}}
	if routes != nil {
		server["routes"] = routes
	}

	body, err := json.Marshal(server)
	if err != nil {
		return fmt.Errorf("marshal server: %w", err)
	}
	url := fmt.Sprintf("%s/config/apps/http/servers/srv0", c.adminURL)
	req, err := http.NewRequestWithContext(ctx, http.MethodPatch, url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		if isTransportError(err) {
			return fmt.Errorf("%w: %v", ErrCaddyUnreachable, err)
		}
		return fmt.Errorf("patch server: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("patch server: HTTP %d: %s", resp.StatusCode, string(respBody))
	}
	return nil
}

// fetchServer returns srv0's current configuration as a mutable map.
func (c *Client) fetchServer(ctx context.Context) (map[string]any, error) {
	url := fmt.Sprintf("%s/config/apps/http/servers/srv0", c.adminURL)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		if isTransportError(err) {
			return nil, fmt.Errorf("%w: %v", ErrCaddyUnreachable, err)
		}
		return nil, fmt.Errorf("fetch server: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		respBody, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("fetch server: HTTP %d: %s", resp.StatusCode, string(respBody))
	}

	server := map[string]any{}
	if err := json.NewDecoder(resp.Body).Decode(&server); err != nil {
		return nil, fmt.Errorf("decode server: %w", err)
	}
	return server, nil
}

// SyncAutoHTTPS replaces both auto-HTTPS exclusion sets and re-asserts the full
// server config. The reconciler calls this every pass, which is what restores the
// :443 listener and the exclusion lists after Caddy restarts back to its
// Caddyfile-only config.
func (c *Client) SyncAutoHTTPS(ctx context.Context, skip, skipCertificates []string) error {
	c.skipMu.Lock()
	c.autoHTTPSSkip = toSet(skip)
	c.autoHTTPSSkipCerts = toSet(skipCertificates)
	c.skipMu.Unlock()

	return c.ensureServer(ctx, nil)
}

// setMode places a hostname in whichever exclusion set its SSL mode calls for
// (and out of the other), reporting whether anything actually changed.
func (c *Client) setMode(hostname, sslMode string) bool {
	c.skipMu.Lock()
	defer c.skipMu.Unlock()

	changed := setMembership(c.autoHTTPSSkip, hostname, sslMode == proxy.SSLModeOff)
	changed = setMembership(c.autoHTTPSSkipCerts, hostname, sslMode == proxy.SSLModeCustom) || changed
	return changed
}

// forgetHost drops a hostname from both exclusion sets.
func (c *Client) forgetHost(hostname string) bool {
	c.skipMu.Lock()
	defer c.skipMu.Unlock()

	changed := setMembership(c.autoHTTPSSkip, hostname, false)
	changed = setMembership(c.autoHTTPSSkipCerts, hostname, false) || changed
	return changed
}

// skipLists returns both sets as sorted slices, so an unchanged set always
// renders to identical JSON and does not churn Caddy's config.
func (c *Client) skipLists() (skip, skipCertificates []string) {
	c.skipMu.Lock()
	defer c.skipMu.Unlock()
	return sortedKeys(c.autoHTTPSSkip), sortedKeys(c.autoHTTPSSkipCerts)
}

func setMembership(set map[string]struct{}, key string, want bool) bool {
	_, present := set[key]
	switch {
	case want && !present:
		set[key] = struct{}{}
	case !want && present:
		delete(set, key)
	default:
		return false
	}
	return true
}

func toSet(keys []string) map[string]struct{} {
	set := make(map[string]struct{}, len(keys))
	for _, k := range keys {
		set[k] = struct{}{}
	}
	return set
}

func sortedKeys(set map[string]struct{}) []string {
	list := make([]string, 0, len(set))
	for k := range set {
		list = append(list, k)
	}
	sort.Strings(list)
	return list
}

// targetToDial converts "http://host:port" to "host:port" for Caddy upstream dial.
func targetToDial(target string) string {
	// Strip protocol prefix
	for _, prefix := range []string{"http://", "https://"} {
		if len(target) > len(prefix) && target[:len(prefix)] == prefix {
			return target[len(prefix):]
		}
	}
	return target
}

// EnsureRoute makes Caddy's route for cfg.Hostname match cfg, reporting whether
// it had to change anything.
//
// The reconciler used to diff on hostname alone: present in Caddy meant correct.
// It is not. UpdateDomain writes the database, then pushes the route — and if
// that push fails (Caddy restarting, admin API blip) the handler logs it, returns
// 200, and leaves the OLD route in place. The hostname is still there, so the
// reconciler saw nothing wrong and the domain served stale configuration for
// ever: the wrong port, no HTTP→HTTPS redirect, basic auth silently missing.
//
// Comparing the whole route is exact, because the route is a pure function of the
// config. A no-op costs one GET; only a real difference rewrites Caddy, so this
// does not churn a reload every pass.
func (c *Client) EnsureRoute(ctx context.Context, cfg proxy.RouteConfig) (changed bool, err error) {
	defer func() { metrics.RecordCaddyCall("ensure_route", err) }()

	desired, err := buildRoute(cfg)
	if err != nil {
		return false, err
	}

	current, found, err := c.fetchRouteByID(ctx, routeIDFor(cfg.Hostname, cfg.Path))
	if err != nil {
		return false, err
	}
	if found {
		same, err := routeJSONEqual(current, desired)
		if err != nil {
			return false, err
		}
		if same {
			return false, nil
		}
	}

	if err := c.AddRoute(ctx, cfg); err != nil {
		return false, err
	}
	return true, nil
}

// fetchRouteByID reads one route by its @id. A missing route is not an error.
func (c *Client) fetchRouteByID(ctx context.Context, id string) (raw json.RawMessage, found bool, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.adminURL+"/id/"+id, nil)
	if err != nil {
		return nil, false, fmt.Errorf("create request: %w", err)
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		if isTransportError(err) {
			return nil, false, fmt.Errorf("%w: %v", ErrCaddyUnreachable, err)
		}
		return nil, false, fmt.Errorf("get route %s: %w", id, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, false, fmt.Errorf("read route %s: %w", id, err)
	}
	// Caddy answers an unknown @id with 500 "unknown object" rather than a 404.
	if resp.StatusCode == http.StatusNotFound || bytes.Contains(body, []byte("unknown object")) {
		return nil, false, nil
	}
	if resp.StatusCode >= 400 {
		return nil, false, fmt.Errorf("get route %s: HTTP %d: %s", id, resp.StatusCode, string(body))
	}
	return body, true, nil
}

// routeJSONEqual compares Caddy's stored route against the one we would write.
// Both are normalised through a generic decode so key order and formatting — which
// Caddy does not preserve — cannot masquerade as drift and cause a rewrite loop.
func routeJSONEqual(current json.RawMessage, desired caddyRoute) (bool, error) {
	desiredJSON, err := json.Marshal(desired)
	if err != nil {
		return false, fmt.Errorf("marshal desired route: %w", err)
	}

	var a, b any
	if err := json.Unmarshal(current, &a); err != nil {
		// Unreadable is not equal; rewriting it is the right answer.
		return false, nil //nolint:nilerr // treat as drift, not as a failure
	}
	if err := json.Unmarshal(desiredJSON, &b); err != nil {
		return false, fmt.Errorf("normalise desired route: %w", err)
	}
	return reflect.DeepEqual(a, b), nil
}
