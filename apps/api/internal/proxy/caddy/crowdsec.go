package caddy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"reflect"
	"strings"

	"github.com/belune-hq/belune/internal/pkg/metrics"
	"github.com/belune-hq/belune/internal/proxy"
)

// crowdsecRouteID is the bouncer's route: one route in front of every application
// route, like the WAF's. Not an application route, so ListRoutes skips it and
// reorderRoutes pins it first.
const crowdsecRouteID = "route-crowdsec"

// crowdsecAppPath is where the bouncer's connection to the local API lives. The
// crowdsec handler is useless without it, and Caddy refuses a config that has the
// handler and not the app — which fixes the order of everything below.
const crowdsecAppPath = "/config/apps/crowdsec"

// bouncerTickerInterval is how often the bouncer asks the local API what changed.
// An unban or an allowlisting takes effect at the next tick, so this is the time a
// banned admin waits after being let back in; the module's own default is a
// minute. The call is a stream of changes since the last, not the whole list.
const bouncerTickerInterval = "5s"

// buildCrowdSecApp renders the bouncer's connection.
//
// enable_hard_fails is written out as false although false is the default. When
// true, an unreachable local API makes Caddy QUIT, which would turn a CrowdSec
// container that is restarting into an outage of every site; the module's default
// is the only thing keeping it off, and a default can change in an update.
// Measured against Caddy v2.11.7 with the module at v0.14.1: with the local API
// down, the bouncer keeps enforcing the bans it holds, admits everyone else, and
// does not fail a single request.
func buildCrowdSecApp(cfg proxy.CrowdSecConfig) map[string]any {
	return map[string]any{
		"api_url":           cfg.APIURL,
		"api_key":           cfg.APIKey,
		"ticker_interval":   bouncerTickerInterval,
		"enable_streaming":  true,
		"enable_hard_fails": false,
	}
}

// buildCrowdSecRoute renders the route for these hosts, leaving out exempt. No
// `terminal`: a request that passes goes on to the application's own route.
//
// exempt is a `not` matcher and not a filter on hosts, because hosts can be a
// wildcard that covers the exempt name: "*.example.com" matches the dashboard on
// dash.example.com, and only Caddy can say that a request for it is not this
// route's.
func buildCrowdSecRoute(hosts, exempt []string) caddyRoute {
	m := caddyMatch{Host: hosts}
	if len(exempt) > 0 {
		m.Not = []caddyMatch{{Host: exempt}}
	}
	return caddyRoute{
		ID:     crowdsecRouteID,
		Match:  []caddyMatch{m},
		Handle: []caddyHandle{{"handler": "crowdsec"}},
	}
}

// SyncCrowdSec makes the bouncer's app and route match cfg. Called every pass, so
// it is two GETs when nothing has changed and a write only on a real difference: a
// write is a config reload.
//
// Order is not a style choice, each half measured against a real Caddy: the app
// goes in BEFORE the route (a route naming the handler without the app is refused
// whole), and comes out AFTER it (an app deleted under a route that uses it is
// refused too).
func (c *Client) SyncCrowdSec(ctx context.Context, cfg proxy.CrowdSecConfig) (err error) {
	defer func() { metrics.RecordCaddyCall("sync_crowdsec", err) }()
	// The key is in every config body this sends, and Caddy's refusals quote parts
	// of what it refused.
	defer func() {
		if err != nil && cfg.APIKey != "" {
			err = &redactedError{msg: strings.ReplaceAll(err.Error(), cfg.APIKey, "[REDACTED]"), err: err}
		}
	}()

	hosts := wafHosts(cfg.Hosts)
	route, routeFound, err := c.fetchRouteByID(ctx, crowdsecRouteID)
	if err != nil {
		return err
	}
	app, appFound, err := c.fetchConfigValue(ctx, crowdsecAppPath)
	if err != nil {
		return err
	}

	if !cfg.Enabled || len(hosts) == 0 {
		if routeFound {
			if err := c.deleteRouteByID(ctx, crowdsecRouteID); err != nil {
				return err
			}
			slog.Info("caddy: CrowdSec route removed")
		}
		if appFound {
			if err := c.deleteConfigValue(ctx, crowdsecAppPath); err != nil {
				return err
			}
			slog.Info("caddy: CrowdSec app removed")
		}
		return nil
	}

	desiredApp := buildCrowdSecApp(cfg)
	switch {
	case !appFound:
		if err := c.sendJSON(ctx, http.MethodPut, crowdsecAppPath, desiredApp); err != nil {
			return fmt.Errorf("add CrowdSec app: %w", err)
		}
		slog.Info("caddy: CrowdSec app added")
	case !jsonEqual(app, desiredApp):
		// PATCH replaces; PUT on an existing key is refused with 409.
		if err := c.sendJSON(ctx, http.MethodPatch, crowdsecAppPath, desiredApp); err != nil {
			return fmt.Errorf("update CrowdSec app: %w", err)
		}
		slog.Info("caddy: CrowdSec app updated")
	}

	desiredRoute := buildCrowdSecRoute(hosts, wafHosts(cfg.Exempt))
	if routeFound {
		same, err := routeJSONEqual(route, desiredRoute)
		if err != nil {
			return err
		}
		if same {
			return nil
		}
		// Replaced where it stands, so it stays first.
		if err := c.sendJSON(ctx, http.MethodPatch, "/id/"+crowdsecRouteID, desiredRoute); err != nil {
			return fmt.Errorf("update CrowdSec route: %w", err)
		}
		slog.Info("caddy: CrowdSec route updated", "hosts", len(hosts))
		return nil
	}

	// Index 0: ahead of the WAF route and every application route, because routes
	// are first-match-wins and the application's reverse_proxy ends the chain.
	if err := c.sendJSON(ctx, http.MethodPut, "/config/apps/http/servers/srv0/routes/0", desiredRoute); err != nil {
		return fmt.Errorf("add CrowdSec route: %w", err)
	}
	slog.Info("caddy: CrowdSec route added", "hosts", len(hosts))
	return nil
}

// redactedError carries a message with a secret taken out and still unwraps to the
// original, so errors.Is(err, ErrCaddyUnreachable) keeps working for callers.
type redactedError struct {
	msg string
	err error
}

func (e *redactedError) Error() string { return e.msg }
func (e *redactedError) Unwrap() error { return e.err }

// fetchConfigValue reads one value from Caddy's config tree. Caddy answers a path
// that holds nothing with 200 and `null`, which is "not found" here and not a value.
func (c *Client) fetchConfigValue(ctx context.Context, path string) (raw json.RawMessage, found bool, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.adminURL+path, nil)
	if err != nil {
		return nil, false, fmt.Errorf("create request: %w", err)
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		if isTransportError(err) {
			return nil, false, fmt.Errorf("%w: %v", ErrCaddyUnreachable, err)
		}
		return nil, false, fmt.Errorf("get %s: %w", path, err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, false, fmt.Errorf("read %s: %w", path, err)
	}
	if resp.StatusCode == http.StatusNotFound {
		return nil, false, nil
	}
	if resp.StatusCode >= 400 {
		return nil, false, fmt.Errorf("get %s: HTTP %d", path, resp.StatusCode)
	}
	if trimmed := bytes.TrimSpace(body); len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return nil, false, nil
	}
	return body, true, nil
}

// deleteConfigValue removes one value from Caddy's config tree; already gone is
// success.
func (c *Client) deleteConfigValue(ctx context.Context, path string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, c.adminURL+path, nil)
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		if isTransportError(err) {
			return fmt.Errorf("%w: %v", ErrCaddyUnreachable, err)
		}
		return fmt.Errorf("delete %s: %w", path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 400 && resp.StatusCode != http.StatusNotFound {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("delete %s: HTTP %d: %s", path, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return nil
}

// jsonEqual compares what Caddy holds with what would be written, through a
// generic decode so key order and formatting cannot read as drift.
func jsonEqual(current json.RawMessage, desired any) bool {
	desiredJSON, err := json.Marshal(desired)
	if err != nil {
		return false
	}
	var a, b any
	if json.Unmarshal(current, &a) != nil || json.Unmarshal(desiredJSON, &b) != nil {
		return false
	}
	return reflect.DeepEqual(a, b)
}
