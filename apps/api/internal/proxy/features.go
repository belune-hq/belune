package proxy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net"
	"sort"
	"strings"
	"time"

	"github.com/belune-hq/belune/internal/proxy/capability"
)

// Feature type identifiers. Must match the values stored in
// domain_route_features.feature_type.
const (
	FeatureBasicAuth   = "basic_auth"
	FeatureHeaders     = "headers"
	FeatureIPAllowlist = "ip_allowlist"
	FeatureRedirect    = "redirect"
	FeatureRateLimit   = "rate_limit"
)

// requiredModules maps each feature that stock Caddy cannot serve to the module
// it needs. It is the single place that ties a feature to a capability, so the
// reconciler's degradation, the write-time check and the capabilities endpoint
// cannot disagree about what needs what.
var requiredModules = map[string]string{
	FeatureRateLimit: capability.ModuleRateLimit,
}

// RequiredModule names the Caddy module a feature type needs, or "" for the
// features stock Caddy serves on its own.
func RequiredModule(featureType string) string {
	return requiredModules[featureType]
}

// ModuleBackedFeatures lists the feature types that depend on an optional module,
// in a stable order.
func ModuleBackedFeatures() []string {
	out := make([]string, 0, len(requiredModules))
	for t := range requiredModules {
		out = append(out, t)
	}
	sort.Strings(out)
	return out
}

// BasicAuthConfig configures HTTP basic auth on a route.
type BasicAuthConfig struct {
	Username       string `json:"username"`
	HashedPassword string `json:"hashed_password"`
}

// HeaderOps describes header mutations Caddy should apply.
// Map values are forced to []string so a caller cannot encode a bare string
// and produce a malformed Caddy config.
type HeaderOps struct {
	Set    map[string][]string `json:"set,omitempty"`
	Add    map[string][]string `json:"add,omitempty"`
	Delete []string            `json:"delete,omitempty"`
}

// HeadersConfig configures request and/or response header manipulation.
type HeadersConfig struct {
	Request  *HeaderOps `json:"request,omitempty"`
	Response *HeaderOps `json:"response,omitempty"`
}

// IPAllowlistConfig lists CIDR ranges that are permitted; everything else is 403.
type IPAllowlistConfig struct {
	Ranges []string `json:"ranges"`
}

// RedirectConfig configures a path-based redirect.
type RedirectConfig struct {
	From       string `json:"from"`
	To         string `json:"to"`
	StatusCode int    `json:"status_code,omitempty"`
}

// RateLimitConfig configures per-client-IP rate limiting (needs the rate_limit
// Caddy module). It is the module's own sliding-window vocabulary rather than a
// "10/s" shorthand, so nothing between this struct and Caddy can misreport what
// is enforced. The zone and the per-IP key are deliberately not configurable:
// see the notes on the handler in caddy/routes.go.
type RateLimitConfig struct {
	// Window is a Go duration, e.g. "1s", "1m".
	Window string `json:"window"`
	// MaxEvents is how many requests one client IP may make per Window.
	MaxEvents int `json:"max_events"`
}

// Bounds on Window. The module itself accepts "500ms" and anything longer, but
// a sub-second window is rarely intended and a multi-hour one would hold
// per-client state for it.
const (
	rateLimitMinWindow = time.Second
	rateLimitMaxWindow = time.Hour

	rateLimitShape = `expected {"window":"1s","max_events":10}`
)

// ParseFeatureConfig validates raw JSON against the typed schema for the given
// feature type. Returns a pointer to the parsed struct on success, or a user-
// readable error. Callers should return HTTP 400 with the error message.
func ParseFeatureConfig(featureType string, raw json.RawMessage) (any, error) {
	if len(raw) == 0 {
		return nil, fmt.Errorf("%s: config required", featureType)
	}

	switch featureType {
	case FeatureBasicAuth:
		var c BasicAuthConfig
		if err := strictUnmarshal(raw, &c); err != nil {
			return nil, fmt.Errorf("%s: %w", featureType, err)
		}
		if c.Username == "" || c.HashedPassword == "" {
			return nil, fmt.Errorf("%s: username and hashed_password are required", featureType)
		}
		return &c, nil

	case FeatureHeaders:
		var c HeadersConfig
		if err := strictUnmarshal(raw, &c); err != nil {
			return nil, fmt.Errorf("%s: %w (values must be arrays of strings, e.g. \"X-Hdr\": [\"v\"])", featureType, err)
		}
		if c.Request == nil && c.Response == nil {
			return nil, fmt.Errorf("%s: at least one of request or response must be set", featureType)
		}
		return &c, nil

	case FeatureIPAllowlist:
		var c IPAllowlistConfig
		if err := strictUnmarshal(raw, &c); err != nil {
			return nil, fmt.Errorf("%s: %w", featureType, err)
		}
		if len(c.Ranges) == 0 {
			return nil, fmt.Errorf("%s: at least one CIDR range is required", featureType)
		}
		for _, r := range c.Ranges {
			if _, _, err := net.ParseCIDR(r); err != nil {
				// Allow bare IPs as well.
				if ip := net.ParseIP(r); ip == nil {
					return nil, fmt.Errorf("%s: %q is not a valid CIDR or IP", featureType, r)
				}
			}
		}
		return &c, nil

	case FeatureRedirect:
		var c RedirectConfig
		if err := strictUnmarshal(raw, &c); err != nil {
			return nil, fmt.Errorf("%s: %w", featureType, err)
		}
		if c.From == "" || c.To == "" {
			return nil, fmt.Errorf("%s: from and to are required", featureType)
		}
		if !strings.HasPrefix(c.From, "/") {
			return nil, fmt.Errorf("%s: from must be a path starting with /", featureType)
		}
		if c.StatusCode == 0 {
			c.StatusCode = 301
		}
		if c.StatusCode < 300 || c.StatusCode >= 400 {
			return nil, fmt.Errorf("%s: status_code must be a 3xx value", featureType)
		}
		return &c, nil

	case FeatureRateLimit:
		var c RateLimitConfig
		if err := strictUnmarshal(raw, &c); err != nil {
			// Rows written before 0.1.3 use {rate, burst}. They are rejected, not
			// translated: a sliding window has no burst, and dropping it silently
			// would recreate the bug that got this feature withdrawn — a domain
			// that looks rate-limited and is not.
			return nil, fmt.Errorf("%s: %w (%s; the older rate/burst form is no longer accepted)", featureType, err, rateLimitShape)
		}
		// max_events is checked, not defaulted: 0 is Go's zero value for an
		// omitted field, and Caddy accepts it — the route then answers nothing at
		// all (empty reply, no status, no log line).
		if c.MaxEvents < 1 {
			return nil, fmt.Errorf("%s: max_events is required and must be at least 1 (%s)", featureType, rateLimitShape)
		}
		if c.Window == "" {
			return nil, fmt.Errorf("%s: window is required (%s)", featureType, rateLimitShape)
		}
		d, err := time.ParseDuration(c.Window)
		if err != nil {
			return nil, fmt.Errorf("%s: window %q is not a duration such as \"1s\" or \"1m\"", featureType, c.Window)
		}
		if d < rateLimitMinWindow || d > rateLimitMaxWindow {
			return nil, fmt.Errorf("%s: window must be between %s and %s", featureType, rateLimitMinWindow, rateLimitMaxWindow)
		}
		return &c, nil

	default:
		return nil, fmt.Errorf("unknown feature type: %s", featureType)
	}
}

func strictUnmarshal(data []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	return dec.Decode(v)
}
