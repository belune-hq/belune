package proxy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net"
	"strings"

	"github.com/weiliang79/belune/internal/proxy/capability"
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

// RequiredModule names the Caddy module a feature type needs to exist, or ""
// for the features stock Caddy serves on its own. It is the single place that
// ties a feature to a capability, so the reconciler's degradation and (later)
// the write-time check cannot disagree about what needs what.
func RequiredModule(featureType string) string {
	switch featureType {
	case FeatureRateLimit:
		return capability.ModuleRateLimit
	}
	return ""
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

// RateLimitConfig configures per-IP rate limiting (requires caddy-rate-limit).
type RateLimitConfig struct {
	Zone  string `json:"zone,omitempty"`
	Rate  string `json:"rate"`
	Burst int    `json:"burst,omitempty"`
	Key   string `json:"key,omitempty"`
}

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
		// Accepting this stored a config that emitted no Caddy handler at all —
		// the stock caddy image carries no ratelimit module — so a domain looked
		// rate-limited and was not. Rejecting is the honest answer until 0.2.0
		// ships the custom Caddy image. To restore it then, delete this branch
		// and reinstate the parse this replaced:
		//
		//	var c RateLimitConfig
		//	if err := strictUnmarshal(raw, &c); err != nil {
		//		return nil, fmt.Errorf("%s: %w", featureType, err)
		//	}
		//	if c.Rate == "" {
		//		return nil, fmt.Errorf("%s: rate is required (e.g. \"10/s\")", featureType)
		//	}
		//	return &c, nil
		//
		// along with the caddy route-builder case that emits the handler.
		return nil, fmt.Errorf("%s: rate limiting is not supported in this release; it arrives with the custom Caddy image in 0.2.0", featureType)

	default:
		return nil, fmt.Errorf("unknown feature type: %s", featureType)
	}
}

func strictUnmarshal(data []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	return dec.Decode(v)
}
