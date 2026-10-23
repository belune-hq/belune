package proxy

import (
	"context"
	"encoding/json"
)

// Settings keys describing how the Belune dashboard itself is served. The
// dashboard has no row in the domains table — it is the panel, not an app — so
// its hostname and TLS choice live in settings, and the reconciler has to apply
// them by hand rather than through the domain sweep.
const (
	// SettingDashboardDomain holds the hostname the dashboard is served on.
	SettingDashboardDomain = "dashboard_domain"
	// SettingDashboardSSLMode holds one of the SSLMode* values below. Absent or
	// empty means automatic, which is what every install before this setting
	// existed was doing.
	SettingDashboardSSLMode = "dashboard_ssl_mode"
	// SettingDashboardCertificateID names the uploaded certificate to serve when
	// the mode is custom. Ignored otherwise.
	SettingDashboardCertificateID = "dashboard_certificate_id"

	// SettingWAFEnabled is the operator's switch for the WAF: "true" turns it on,
	// anything else is off. One switch for the whole install — see WAFConfig.
	SettingWAFEnabled = "waf_enabled"
)

const (
	// SSLModeOff serves plain HTTP only: no certificate is obtained for the
	// hostname and no HTTP→HTTPS redirect is rendered.
	SSLModeOff = "off"
	// SSLModeAutomatic obtains a certificate from Let's Encrypt. The default.
	SSLModeAutomatic = "automatic"
	// SSLModeCustom serves an operator-uploaded certificate from the store.
	SSLModeCustom = "custom"
	// SSLModeDNSChallenge obtains a certificate over ACME DNS-01 with a stored DNS
	// provider credential. The only mode that can issue a wildcard, and the one
	// that works for a hostname behind a CDN that never lets HTTP-01 through.
	SSLModeDNSChallenge = "dns_challenge"
)

// ValidSSLMode reports whether mode is one the proxy knows how to serve. Empty
// counts as automatic, which is the historical default.
func ValidSSLMode(mode string) bool {
	switch mode {
	case "", SSLModeOff, SSLModeAutomatic, SSLModeCustom, SSLModeDNSChallenge:
		return true
	}
	return false
}

// WAFConfig is the desired state of the WAF, which is one route in front of every
// application route rather than a feature of each.
//
// Global, deliberately: a per-route WAF would put it into the route-feature model,
// its normalisation and the reconciler's determinism, for a first phase that only
// observes. The cost is that one noisy application's false positives share the one
// detection log, which is tolerable while nothing is blocked and is exactly why
// enforcement will need to be per application.
type WAFConfig struct {
	// Enabled is the operator's switch AND the module being present; the caller has
	// already decided both. False removes the route.
	Enabled bool
	// Hosts are the application hostnames the WAF inspects. The dashboard is left
	// out: its own API takes commands, scripts and credentials as ordinary input,
	// which a generic rule set reads as attacks, and an admin panel is not what the
	// WAF is here to watch. No hosts means no route.
	Hosts []string
}

// RouteFeature represents a middleware feature applied to a route.
// Config is kept as raw JSON so it can be validated against a typed schema
// via ParseFeatureConfig at the route-builder boundary.
type RouteFeature struct {
	Type    string          `json:"type"` // basic_auth, redirect, headers, ip_allowlist, rate_limit
	Config  json.RawMessage `json:"config"`
	Enabled bool            `json:"enabled"`
}

// RouteConfig describes a reverse-proxy route with optional TLS and middleware.
type RouteConfig struct {
	Hostname string `json:"hostname"`
	// Path is the public prefix this route answers on, always rooted ("/" for a
	// whole-host route). StripPath removes it before the upstream sees the
	// request — an app mounted at /api usually expects /users, not /api/users.
	//
	// InternalPath is prepended after the strip, for an app that insists on a base
	// path of its own (Grafana under /grafana). Empty prepends nothing. The two
	// compose in that order, which is the only order that makes sense: take off
	// what the public URL carries, then add what the app demands.
	Path         string `json:"path"`
	StripPath    bool   `json:"strip_path"`
	InternalPath string `json:"internal_path"`
	TargetURL    string `json:"target_url"`
	TLS          bool   `json:"tls"`
	ForceHTTPS   bool   `json:"force_https"`
	SSLMode      string `json:"ssl_mode"`     // automatic, custom, off, dns_challenge
	SSLProvider  string `json:"ssl_provider"` // e.g. cloudflare
	// CertPEM/KeyPEM carry the decrypted certificate for ssl_mode=custom. They are
	// pushed to Caddy in-band (load_pem) rather than written to disk, so the key
	// never touches the proxy's filesystem. Treat as secret: never log them.
	CertPEM         string          `json:"-"`
	KeyPEM          string          `json:"-"`
	Features        []RouteFeature  `json:"features"`          // middleware features
	AdvancedConfig  json.RawMessage `json:"advanced_config"`   // raw Caddy JSON for power users
	HealthCheckPath string          `json:"health_check_path"` // optional path Caddy probes for upstream liveness
}

// HostCertificate is a decrypted certificate bound to the hostname it serves.
// The proxy tags loaded certificates by hostname so it can tell which of them it
// owns, and Caddy then selects between them by SNI.
type HostCertificate struct {
	Hostname string
	CertPEM  string
	KeyPEM   string
}

// Decryptor unwraps the envelope-encrypted certificate PEM stored in the
// database. *crypto.Keyring satisfies it; the proxy package takes the narrow
// interface so it does not depend on the keyring implementation.
type Decryptor interface {
	Decrypt(ciphertext []byte) ([]byte, error)
}

// NetworkAttacher joins the proxy container to a project's Docker network.
//
// Apps live on per-project networks and are dialled by container name, so Caddy
// can only reach them if it has joined that network. The deploy worker does this
// when an app is deployed — but a container that is *recreated* (any compose up,
// an upgrade, a config change) loses every network it joined at runtime, and
// nothing put them back. Every app domain then answered 502 until each app was
// redeployed by hand. The reconciler re-asserts routes and certificates after a
// Caddy restart for exactly this reason; the networks belong with them.
type NetworkAttacher interface {
	ConnectContainerToNetwork(ctx context.Context, containerName, networkName string) error
}

// ProxyManager abstracts reverse proxy operations.
type ProxyManager interface {
	AddRoute(ctx context.Context, cfg RouteConfig) error

	// EnsureRoute makes the proxy's route for cfg.Hostname match cfg, reporting
	// whether anything had to change.
	//
	// Presence is not correctness. UpdateDomain writes the database and then pushes
	// the route; if that push fails the handler logs it, returns 200, and the OLD
	// route stays. Diffing on hostname alone — which the reconciler used to do —
	// sees a route with the right name and calls it correct, so the domain serves
	// stale configuration for ever. Compare the whole route instead.
	EnsureRoute(ctx context.Context, cfg RouteConfig) (changed bool, err error)
	// RemoveRoute takes the path as well as the host: a hostname no longer
	// identifies a single route, and deleting by host alone would take out a
	// sibling path's route along with the one asked for.
	RemoveRoute(ctx context.Context, hostname, path string) error
	SetupTLS(ctx context.Context, hostname string, sslMode, certPEM, keyPEM string) error
	ListRoutes(ctx context.Context) ([]RouteConfig, error)

	// SetDashboardRoute publishes Belune's own dashboard on a hostname, so it can
	// have a certificate at all: automatic HTTPS issues only for names that appear
	// in a host matcher, and the dashboard is otherwise served by a matcher-less
	// catch-all. An empty hostname clears it. Idempotent — the reconciler
	// re-asserts it every pass, because a Caddy restart drops it like everything
	// else pushed over the admin API.
	// sslMode decides whether a certificate is obtained for the hostname and
	// whether the route renders an HTTP→HTTPS redirect. It must: redirecting to
	// HTTPS on an ssl_mode=off dashboard would bounce the operator to a port with
	// no certificate and lock them out of their own panel.
	SetDashboardRoute(ctx context.Context, hostname, sslMode string) error

	// SyncWAF makes the proxy's WAF route match cfg, and does nothing when it
	// already does. Like routes, it is lost when Caddy restarts, so the reconciler
	// re-asserts it every pass.
	SyncWAF(ctx context.Context, cfg WAFConfig) error

	// SyncCertificates makes the proxy's loaded certificates match the given set,
	// keyed by hostname. Like routes, certificates pushed over the admin API are
	// lost when Caddy restarts, so the reconciler re-asserts them each pass.
	SyncCertificates(ctx context.Context, certs []HostCertificate) error

	// UsesInternalIssuer reports whether the proxy is *configured* to issue from
	// its own internal CA (Caddy's local_certs) rather than from a public ACME CA.
	//
	// This has to be read from the proxy's configuration, not inferred from the
	// certificate it serves: a production Caddy that has failed to get a Let's
	// Encrypt certificate also serves an internal one, and those two situations
	// must not look the same. One is a working dev box; the other is broken HTTPS.
	UsesInternalIssuer(ctx context.Context) (bool, error)

	// SyncAutoHTTPS declares the hostnames excluded from automatic HTTPS — skip
	// for ssl_mode=off (no certificate at all) and skipCertificates for
	// ssl_mode=custom (the operator supplies it) — and re-asserts the server-level
	// config automatic HTTPS depends on. The reconciler calls it every pass: a
	// restarted Caddy comes back up with only its static Caddyfile, so the HTTPS
	// listener and these lists have to be re-applied the same way routes are.
	SyncAutoHTTPS(ctx context.Context, skip, skipCertificates []string) error

	// SyncDNSPolicies makes the DNS-01 TLS automation policies match the given
	// set, leaving policies Belune does not own alone. An empty set removes ours.
	SyncDNSPolicies(ctx context.Context, policies []DNSPolicy) error
}
