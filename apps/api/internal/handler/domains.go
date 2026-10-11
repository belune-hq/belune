package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/weiliang79/belune/internal/naming"
	"github.com/weiliang79/belune/internal/proxy"
	"github.com/weiliang79/belune/internal/store/generated"
)

// hostnameRegex validates RFC 1123 hostnames. Each label is 1–63 alphanumeric characters
// or internal hyphens; the full name must have at least one dot (or be "localhost").
var hostnameRegex = regexp.MustCompile(`^(([a-zA-Z0-9]([a-zA-Z0-9\-]{0,61}[a-zA-Z0-9])?\.)+[a-zA-Z]{2,}|localhost)$`)

// uuidToString converts a pgtype.UUID to its string representation.
func uuidToString(u pgtype.UUID) string {
	return uuid.UUID(u.Bytes).String()
}

var validFeatureTypes = map[string]bool{
	"basic_auth":   true,
	"redirect":     true,
	"headers":      true,
	"ip_allowlist": true,
	"rate_limit":   true,
}

var validSSLModes = map[string]bool{
	"automatic": true,
	"custom":    true,
	"off":       true,
}

// sslModeDNSChallenge is accepted by the database CHECK (and may exist on rows
// created before it was withdrawn) but can no longer be set: it needs a Caddy
// build carrying DNS provider modules, which the stock image does not have, so a
// domain using it would sit on "pending" forever. Rejecting it with a reason
// beats silently accepting a mode that cannot work.
const sslModeDNSChallenge = "dns_challenge"

// validateSSLMode returns a user-facing reason when the mode cannot be used.
func validateSSLMode(mode string) string {
	if mode == sslModeDNSChallenge {
		return "DNS challenge is not supported. Use Automatic, or upload a certificate and choose Custom."
	}
	if !validSSLModes[mode] {
		return "invalid ssl_mode: must be automatic, custom, or off"
	}
	return ""
}

// normalizeDomainPath puts a public path prefix into the one shape the database
// and Caddy both expect: rooted, and without a trailing slash unless it is the
// root itself.
//
// The empty string is the case that matters. `domains.path` is NOT NULL with a
// DEFAULT of '/', but sqlc always sends the column explicitly, so an unset field
// arrives as ” — which is not a default, it is a check-constraint violation at
// insert time. Every caller goes through here so that cannot happen.
func normalizeDomainPath(p string) string {
	p = strings.TrimSpace(p)
	if p == "" {
		return "/"
	}
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	if p != "/" {
		p = strings.TrimRight(p, "/")
	}
	return p
}

// A path prefix is matched literally by Caddy, so it may contain only what can
// legitimately appear in a URL path. No wildcards in particular: the route
// builder derives "/api" and "/api/*" itself, so an operator who wrote the star
// would end up with "/api/*/*", which matches nothing they expect.
var domainPathRegex = regexp.MustCompile(`^/[A-Za-z0-9\-._~/]*$`)

// validateDomainPath returns a human reason an already-normalised path is
// unusable, or "" if it is fine.
func validateDomainPath(p string) string {
	if len(p) > 255 {
		return "path is too long (maximum 255 characters)"
	}
	if !domainPathRegex.MatchString(p) {
		return "path must start with / and may contain only letters, digits and - . _ ~ / — no wildcards or query strings"
	}
	if strings.Contains(p, "//") {
		return "path must not contain an empty segment (//)"
	}
	return ""
}

// normalizeInternalPath shapes the prefix prepended before the request reaches
// the app. Empty means "prepend nothing", which is what every domain did before
// this existed — so unlike the public path, empty is legitimate here and must NOT
// become "/": prepending a bare "/" would rewrite "/users" into "//users".
func normalizeInternalPath(p string) string {
	p = strings.TrimSpace(p)
	if p == "" || p == "/" {
		return ""
	}
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	return strings.TrimRight(p, "/")
}

// validateInternalPath returns a human reason an already-normalised internal path
// is unusable, or "".
func validateInternalPath(p string) string {
	if p == "" {
		return ""
	}
	if len(p) > 255 {
		return "internal path is too long (maximum 255 characters)"
	}
	if !domainPathRegex.MatchString(p) {
		return "internal path must start with / and may contain only letters, digits and - . _ ~ / — no wildcards or query strings"
	}
	if strings.Contains(p, "//") {
		return "internal path must not contain an empty segment (//)"
	}
	return ""
}

// checkHostTLSAgreement rejects a domain whose TLS choice contradicts the other
// paths already served on the same hostname, returning a human reason or "".
//
// Every path on a hostname is served by one certificate — Caddy selects it by
// SNI, which knows nothing about paths — so "shop.com/ is automatic but
// shop.com/api is off" is not a configuration, it is a contradiction. The
// reconciler warns and keeps the first when it meets this, but a warning in a log
// nobody reads is a poor substitute for refusing to create the state at all.
//
// excludeID skips the row being updated, which would otherwise conflict with
// itself.
func (h *Handler) checkHostTLSAgreement(ctx context.Context, hostname, sslMode string, certID, excludeID pgtype.UUID) string {
	siblings, err := h.queries.ListDomainsByHostname(ctx, hostname)
	if err != nil {
		// Not worth failing the request over — the reconciler still guards it.
		slog.Warn("domains: failed to check hostname TLS agreement", "hostname", hostname, "error", err)
		return ""
	}
	for _, s := range siblings {
		if excludeID.Valid && s.ID == excludeID {
			continue
		}
		if s.SslMode != sslMode {
			return fmt.Sprintf(
				"%s is already served with TLS mode %q on path %s. Every path on one hostname shares a certificate, so they must all use the same mode.",
				hostname, s.SslMode, s.Path,
			)
		}
		if sslMode == proxy.SSLModeCustom && s.CertificateID != certID {
			return fmt.Sprintf(
				"%s already serves a different certificate on path %s. Every path on one hostname shares a certificate.",
				hostname, s.Path,
			)
		}
	}
	return ""
}

//apidoc:tag applications/domains
//apidoc:title Get Domains
//apidoc:order 1
func (h *Handler) ListDomains(w http.ResponseWriter, r *http.Request) {
	applicationID := chi.URLParam(r, "applicationId")
	var applicationUUID pgtype.UUID
	if err := applicationUUID.Scan(applicationID); err != nil {
		writeError(w, http.StatusBadRequest, "invalid application id")
		return
	}

	if !h.canAccessApplication(r, applicationUUID) {
		writeError(w, http.StatusForbidden, "access denied")
		return
	}

	domains, err := h.queries.ListDomainsByApplicationWithFeatures(r.Context(), applicationUUID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list domains")
		return
	}

	writeJSON(w, http.StatusOK, domains)
}

// GetDomain returns one domain in the same shape ListDomains returns it.
//
// Exists for the polling case a list does not serve well: tls_status moves
// unknown -> pending -> active on its own schedule, so a script that adds a
// domain and waits for TLS has to re-read that one row. Without this it must
// fetch every domain on the application and filter, on every poll.
//
// Access is canAccessDomain, which resolves the DOMAIN's own owner rather than
// trusting the {applicationId} in the path — the same check UpdateDomain and
// RemoveDomain use.
//
//apidoc:tag applications/domains
//apidoc:title Get Domain
//apidoc:description One domain, in the shape the list returns. Useful for polling `tls_status`, which moves from `pending` to `active` on its own once a certificate issues.
//apidoc:order 2
func (h *Handler) GetDomain(w http.ResponseWriter, r *http.Request) {
	domainID := chi.URLParam(r, "domainId")
	var domainUUID pgtype.UUID
	if err := domainUUID.Scan(domainID); err != nil {
		writeError(w, http.StatusBadRequest, "invalid domain id")
		return
	}

	if !h.canAccessDomain(r, domainUUID) {
		writeError(w, http.StatusForbidden, "access denied")
		return
	}

	domain, err := h.queries.GetDomainWithFeatures(r.Context(), domainUUID)
	if err != nil {
		writeError(w, http.StatusNotFound, "domain not found")
		return
	}

	writeJSON(w, http.StatusOK, domain)
}

type addDomainRequest struct {
	Hostname       string          `json:"hostname"`
	Path           string          `json:"path,omitempty"`
	StripPath      bool            `json:"strip_path,omitempty"`
	InternalPath   string          `json:"internal_path,omitempty"`
	SSLEnabled     bool            `json:"ssl_enabled"`
	ContainerPort  *int32          `json:"container_port,omitempty"`
	ForceHTTPS     *bool           `json:"force_https,omitempty"`
	SSLMode        string          `json:"ssl_mode,omitempty"`
	SSLProvider    string          `json:"ssl_provider,omitempty"`
	CertificateID  string          `json:"certificate_id,omitempty"`
	AdvancedConfig json.RawMessage `json:"advanced_config,omitempty"`
}

//apidoc:tag applications/domains
//apidoc:title Create Domain
//apidoc:order 3
func (h *Handler) AddDomain(w http.ResponseWriter, r *http.Request) {
	applicationID := chi.URLParam(r, "applicationId")
	var applicationUUID pgtype.UUID
	if err := applicationUUID.Scan(applicationID); err != nil {
		writeError(w, http.StatusBadRequest, "invalid application id")
		return
	}

	if !h.canAccessApplication(r, applicationUUID) {
		writeError(w, http.StatusForbidden, "access denied")
		return
	}

	var req addDomainRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	if req.Hostname == "" {
		writeError(w, http.StatusBadRequest, "hostname is required")
		return
	}
	if !hostnameRegex.MatchString(req.Hostname) {
		writeError(w, http.StatusBadRequest, "invalid hostname format")
		return
	}

	// Defaults
	sslMode := "automatic"
	if req.SSLMode != "" {
		if reason := validateSSLMode(req.SSLMode); reason != "" {
			writeError(w, http.StatusBadRequest, reason)
			return
		}
		sslMode = req.SSLMode
	}
	forceHTTPS := req.SSLEnabled
	if req.ForceHTTPS != nil {
		forceHTTPS = *req.ForceHTTPS
	}

	certUUID, err := parseCertificateID(sslMode, req.CertificateID)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	path := normalizeDomainPath(req.Path)
	if reason := validateDomainPath(path); reason != "" {
		writeError(w, http.StatusBadRequest, reason)
		return
	}
	internalPath := normalizeInternalPath(req.InternalPath)
	if reason := validateInternalPath(internalPath); reason != "" {
		writeError(w, http.StatusBadRequest, reason)
		return
	}
	if reason := h.checkHostTLSAgreement(r.Context(), req.Hostname, sslMode, certUUID, pgtype.UUID{}); reason != "" {
		writeError(w, http.StatusBadRequest, reason)
		return
	}

	params := generated.CreateDomainParams{
		ApplicationID:  applicationUUID,
		Hostname:       req.Hostname,
		SslEnabled:     req.SSLEnabled,
		ForceHttps:     forceHTTPS,
		SslMode:        sslMode,
		SslProvider:    pgtype.Text{String: req.SSLProvider, Valid: req.SSLProvider != ""},
		CertificateID:  certUUID,
		AdvancedConfig: req.AdvancedConfig,
		Path:           path,
		StripPath:      req.StripPath,
		InternalPath:   internalPath,
	}
	if req.ContainerPort != nil {
		params.ContainerPort = pgtype.Int4{Int32: *req.ContainerPort, Valid: true}
	}

	domain, err := h.queries.CreateDomain(r.Context(), params)
	if err != nil {
		// (hostname, path) is unique. Adding shop.com/api twice is a mistake the
		// operator can fix, so say what happened rather than returning a 500 that
		// reads as "the server is broken".
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			writeError(w, http.StatusConflict, fmt.Sprintf("%s%s is already configured", req.Hostname, strings.TrimSuffix(path, "/")))
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to add domain")
		return
	}

	// Add Caddy proxy route for this domain
	row, err := h.queries.GetApplicationWithProjectSlug(r.Context(), applicationUUID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to resolve project")
		return
	}

	reqPort := pgtype.Int4{}
	if req.ContainerPort != nil {
		reqPort = pgtype.Int4{Int32: *req.ContainerPort, Valid: true}
	}
	port := proxy.ResolveUpstreamPort(reqPort, row.ContainerPort)

	cert := h.domainCertificate(r.Context(), domain)

	containerName := naming.ContainerName(row.ProjectSlug, row.Slug, applicationID)
	if err := h.proxy.AddRoute(r.Context(), proxy.RouteConfig{
		Hostname:       req.Hostname,
		TargetURL:      fmt.Sprintf("http://%s:%d", containerName, port),
		TLS:            req.SSLEnabled,
		ForceHTTPS:     forceHTTPS,
		SSLMode:        sslMode,
		CertPEM:        cert.CertPEM,
		KeyPEM:         cert.KeyPEM,
		AdvancedConfig: req.AdvancedConfig,
	}); err != nil {
		slog.Error("failed to add proxy route for domain", "hostname", req.Hostname, "container", containerName, "error", err)
		if delErr := h.queries.DeleteDomain(r.Context(), domain.ID); delErr != nil {
			slog.Error("failed to rollback domain insert after proxy failure", "domain_id", domain.ID, "error", delErr)
		}
		writeError(w, http.StatusInternalServerError, "failed to configure proxy for domain")
		return
	}

	// Probe immediately so the TLS badge settles within seconds instead of
	// showing "unknown" until the next sweep.
	h.enqueueTLSProbe(uuidToString(domain.ID))

	h.audit(r, "add_domain", "domain", uuidToString(domain.ID), map[string]any{"hostname": req.Hostname})

	writeJSON(w, http.StatusCreated, domain)
}

type updateDomainRequest struct {
	Hostname string `json:"hostname"`
	// Pointers, so "field absent" and "field set to root" stay distinguishable. A
	// client that does not know about paths must leave a domain's path alone, not
	// silently move it back to the root.
	Path           *string         `json:"path,omitempty"`
	StripPath      *bool           `json:"strip_path,omitempty"`
	InternalPath   *string         `json:"internal_path,omitempty"`
	SSLEnabled     bool            `json:"ssl_enabled"`
	ContainerPort  *int32          `json:"container_port,omitempty"`
	ForceHTTPS     bool            `json:"force_https"`
	SSLMode        string          `json:"ssl_mode"`
	SSLProvider    string          `json:"ssl_provider,omitempty"`
	CertificateID  string          `json:"certificate_id,omitempty"`
	AdvancedConfig json.RawMessage `json:"advanced_config,omitempty"`
}

//apidoc:tag applications/domains
//apidoc:title Update Domain
//apidoc:order 4
func (h *Handler) UpdateDomain(w http.ResponseWriter, r *http.Request) {
	domainID := chi.URLParam(r, "domainId")
	var domainUUID pgtype.UUID
	if err := domainUUID.Scan(domainID); err != nil {
		writeError(w, http.StatusBadRequest, "invalid domain id")
		return
	}

	if !h.canAccessDomain(r, domainUUID) {
		writeError(w, http.StatusForbidden, "access denied")
		return
	}

	var req updateDomainRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	if req.Hostname == "" {
		writeError(w, http.StatusBadRequest, "hostname is required")
		return
	}
	if !hostnameRegex.MatchString(req.Hostname) {
		writeError(w, http.StatusBadRequest, "invalid hostname format")
		return
	}
	if req.SSLMode != "" {
		if reason := validateSSLMode(req.SSLMode); reason != "" {
			writeError(w, http.StatusBadRequest, reason)
			return
		}
	}
	if req.SSLMode == "" {
		req.SSLMode = "automatic"
	}

	certUUID, err := parseCertificateID(req.SSLMode, req.CertificateID)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	// Fetch existing domain to detect hostname changes
	oldDomain, err := h.queries.GetDomain(r.Context(), domainUUID)
	if err != nil {
		writeError(w, http.StatusNotFound, "domain not found")
		return
	}

	path, stripPath := oldDomain.Path, oldDomain.StripPath
	internalPath := oldDomain.InternalPath
	if req.Path != nil {
		path = normalizeDomainPath(*req.Path)
		if reason := validateDomainPath(path); reason != "" {
			writeError(w, http.StatusBadRequest, reason)
			return
		}
	}
	if req.StripPath != nil {
		stripPath = *req.StripPath
	}
	if req.InternalPath != nil {
		internalPath = normalizeInternalPath(*req.InternalPath)
		if reason := validateInternalPath(internalPath); reason != "" {
			writeError(w, http.StatusBadRequest, reason)
			return
		}
	}
	// Excludes this row: a domain must not be found to contradict itself.
	if reason := h.checkHostTLSAgreement(r.Context(), req.Hostname, req.SSLMode, certUUID, domainUUID); reason != "" {
		writeError(w, http.StatusBadRequest, reason)
		return
	}

	params := generated.UpdateDomainParams{
		ID:             domainUUID,
		Hostname:       req.Hostname,
		SslEnabled:     req.SSLEnabled,
		ForceHttps:     req.ForceHTTPS,
		SslMode:        req.SSLMode,
		SslProvider:    pgtype.Text{String: req.SSLProvider, Valid: req.SSLProvider != ""},
		CertificateID:  certUUID,
		AdvancedConfig: req.AdvancedConfig,
		// Carried over unless the request says otherwise. The update statement
		// writes path unconditionally, so defaulting here would quietly move a
		// domain back to the root every time anyone edited its port or TLS mode.
		Path:         path,
		StripPath:    stripPath,
		InternalPath: internalPath,
	}
	if req.ContainerPort != nil {
		params.ContainerPort = pgtype.Int4{Int32: *req.ContainerPort, Valid: true}
	}

	domain, err := h.queries.UpdateDomain(r.Context(), params)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to update domain")
		return
	}

	// Rebuild proxy route: remove old hostname (which may equal new), then
	// AddRoute below installs the fresh config. Failure here is logged but
	// non-fatal — a stale route is preferable to a 500 on the update endpoint.
	if err := h.proxy.RemoveRoute(r.Context(), oldDomain.Hostname, oldDomain.Path); err != nil {
		slog.Warn("update domain: failed to remove stale proxy route", "hostname", oldDomain.Hostname, "path", oldDomain.Path, "error", err)
	}

	// Resolve container name and port for the proxy route
	row, err := h.queries.GetApplicationWithProjectSlug(r.Context(), domain.ApplicationID)
	if err != nil {
		slog.Error("failed to resolve application for proxy update", "error", err)
		writeJSON(w, http.StatusOK, domain)
		return
	}

	reqPort := pgtype.Int4{}
	if req.ContainerPort != nil {
		reqPort = pgtype.Int4{Int32: *req.ContainerPort, Valid: true}
	}
	port := proxy.ResolveUpstreamPort(reqPort, row.ContainerPort)

	// Load route features for this domain
	features := h.loadRouteFeatures(r, domainUUID)
	cert := h.domainCertificate(r.Context(), domain)

	containerName := naming.ContainerName(row.ProjectSlug, row.Slug, uuidToString(domain.ApplicationID))
	if err := h.proxy.AddRoute(r.Context(), proxy.RouteConfig{
		Hostname:       req.Hostname,
		TargetURL:      fmt.Sprintf("http://%s:%d", containerName, port),
		TLS:            req.SSLEnabled,
		ForceHTTPS:     req.ForceHTTPS,
		SSLMode:        req.SSLMode,
		SSLProvider:    req.SSLProvider,
		CertPEM:        cert.CertPEM,
		KeyPEM:         cert.KeyPEM,
		Features:       features,
		AdvancedConfig: req.AdvancedConfig,
	}); err != nil {
		slog.Error("failed to update proxy route", "hostname", req.Hostname, "error", err)
	}

	h.enqueueTLSProbe(domainID)

	h.audit(r, "update_domain", "domain", domainID, map[string]any{"hostname": req.Hostname})

	writeJSON(w, http.StatusOK, domain)
}

// parseCertificateID validates the certificate selection against the SSL mode.
// Only ssl_mode=custom serves an uploaded certificate, and it cannot do so
// without one; any other mode clears the reference, so a domain switched away
// from custom stops pinning a certificate against deletion.
func parseCertificateID(sslMode, certificateID string) (pgtype.UUID, error) {
	var id pgtype.UUID
	if sslMode != proxy.SSLModeCustom {
		return id, nil
	}
	if certificateID == "" {
		return id, fmt.Errorf("ssl_mode custom requires certificate_id")
	}
	if err := id.Scan(certificateID); err != nil {
		return pgtype.UUID{}, fmt.Errorf("invalid certificate_id")
	}
	return id, nil
}

// domainCertificate returns the decrypted PEM pair a domain serves. A lookup or
// decrypt failure is logged and treated as "no certificate": SetupTLS then
// reports a TLS failure for that hostname rather than failing the whole request
// and leaving the domain unrouted.
func (h *Handler) domainCertificate(ctx context.Context, domain generated.Domain) proxy.HostCertificate {
	cert, err := proxy.ResolveCertificate(ctx, h.queries, h.cfg.Keyring, domain)
	if err != nil {
		slog.Error("failed to resolve domain certificate", "hostname", domain.Hostname, "error", err)
	}
	return cert
}

// loadRouteFeatures fetches route features from DB and converts to proxy.RouteFeature slice.
func (h *Handler) loadRouteFeatures(r *http.Request, domainID pgtype.UUID) []proxy.RouteFeature {
	dbFeatures, err := h.queries.ListRouteFeaturesByDomain(r.Context(), domainID)
	if err != nil {
		slog.Warn("failed to load route features", "domain_id", domainID, "error", err)
		return nil
	}
	var features []proxy.RouteFeature
	for _, f := range dbFeatures {
		features = append(features, proxy.RouteFeature{
			Type:    f.FeatureType,
			Config:  json.RawMessage(f.Config),
			Enabled: f.Enabled,
		})
	}
	return features
}

// --- Route Feature CRUD ---

//apidoc:tag applications/domains
//apidoc:title Get Route Features
//apidoc:order 6
func (h *Handler) ListRouteFeatures(w http.ResponseWriter, r *http.Request) {
	domainID := chi.URLParam(r, "domainId")
	var domainUUID pgtype.UUID
	if err := domainUUID.Scan(domainID); err != nil {
		writeError(w, http.StatusBadRequest, "invalid domain id")
		return
	}

	if !h.canAccessDomain(r, domainUUID) {
		writeError(w, http.StatusForbidden, "access denied")
		return
	}

	features, err := h.queries.ListRouteFeaturesByDomain(r.Context(), domainUUID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list route features")
		return
	}

	writeJSON(w, http.StatusOK, features)
}

type upsertRouteFeatureRequest struct {
	FeatureType string          `json:"feature_type"`
	Config      json.RawMessage `json:"config"`
	Enabled     bool            `json:"enabled"`
}

//apidoc:tag applications/domains
//apidoc:title Upsert Route Feature
//apidoc:order 7
func (h *Handler) UpsertRouteFeature(w http.ResponseWriter, r *http.Request) {
	domainID := chi.URLParam(r, "domainId")
	var domainUUID pgtype.UUID
	if err := domainUUID.Scan(domainID); err != nil {
		writeError(w, http.StatusBadRequest, "invalid domain id")
		return
	}

	if !h.canAccessDomain(r, domainUUID) {
		writeError(w, http.StatusForbidden, "access denied")
		return
	}

	var req upsertRouteFeatureRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	if !validFeatureTypes[req.FeatureType] {
		// rate_limit stays in validFeatureTypes but is deliberately not advertised
		// here: it falls through to ParseFeatureConfig, whose message explains it
		// is withdrawn until 0.2.0 rather than claiming the type is unknown.
		writeError(w, http.StatusBadRequest, "invalid feature_type: must be basic_auth, redirect, headers, or ip_allowlist")
		return
	}

	if _, err := proxy.ParseFeatureConfig(req.FeatureType, req.Config); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	feature, err := h.queries.UpsertRouteFeature(r.Context(), generated.UpsertRouteFeatureParams{
		DomainID:    domainUUID,
		FeatureType: req.FeatureType,
		Config:      req.Config,
		Enabled:     req.Enabled,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to upsert route feature")
		return
	}

	// Rebuild proxy route with updated features
	h.rebuildDomainRoute(r, domainUUID)

	writeJSON(w, http.StatusOK, feature)
}

//apidoc:tag applications/domains
//apidoc:title Delete Route Feature
//apidoc:order 8
func (h *Handler) DeleteRouteFeature(w http.ResponseWriter, r *http.Request) {
	featureID := chi.URLParam(r, "featureId")
	var featureUUID pgtype.UUID
	if err := featureUUID.Scan(featureID); err != nil {
		writeError(w, http.StatusBadRequest, "invalid feature id")
		return
	}

	domainID := chi.URLParam(r, "domainId")
	var domainUUID pgtype.UUID
	if err := domainUUID.Scan(domainID); err != nil {
		writeError(w, http.StatusBadRequest, "invalid domain id")
		return
	}

	if !h.canAccessDomain(r, domainUUID) {
		writeError(w, http.StatusForbidden, "access denied")
		return
	}

	// ⚠️ Keyed on the domain as well as the feature — see the query. The gate
	// above authorizes the {domainId} in the path; nothing tied this
	// {featureId} to it, so a caller could name a domain it may reach and ANY
	// feature id and delete it. That is a cross-project destructive write: it
	// strips an ip_allowlist or basic_auth control off another project's live
	// public route. Reproduced with a project-pinned token before this.
	//
	// 404 on no rows, so "belongs to another domain" and "does not exist" are
	// the same answer — the id is the only thing protecting it.
	deleted, err := h.queries.DeleteRouteFeature(r.Context(), generated.DeleteRouteFeatureParams{
		ID:       featureUUID,
		DomainID: domainUUID,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to delete route feature")
		return
	}
	if deleted == 0 {
		writeError(w, http.StatusNotFound, "route feature not found")
		return
	}

	// Rebuild proxy route with updated features
	h.rebuildDomainRoute(r, domainUUID)

	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

// rebuildDomainRoute removes and re-adds the proxy route for a domain with current config + features.
func (h *Handler) rebuildDomainRoute(r *http.Request, domainID pgtype.UUID) {
	domain, err := h.queries.GetDomain(r.Context(), domainID)
	if err != nil {
		slog.Warn("rebuildDomainRoute: failed to get domain", "error", err)
		return
	}

	row, err := h.queries.GetApplicationWithProjectSlug(r.Context(), domain.ApplicationID)
	if err != nil {
		slog.Warn("rebuildDomainRoute: failed to resolve application", "error", err)
		return
	}

	port := proxy.ResolveUpstreamPort(domain.ContainerPort, row.ContainerPort)

	features := h.loadRouteFeatures(r, domainID)
	cert := h.domainCertificate(r.Context(), domain)

	appIDStr := uuidToString(domain.ApplicationID)
	containerName := naming.ContainerName(row.ProjectSlug, row.Slug, appIDStr)

	if err := h.proxy.RemoveRoute(r.Context(), domain.Hostname, domain.Path); err != nil {
		slog.Warn("rebuildDomainRoute: failed to remove proxy route", "hostname", domain.Hostname, "path", domain.Path, "error", err)
	}
	if err := h.proxy.AddRoute(r.Context(), proxy.RouteConfig{
		Hostname:       domain.Hostname,
		Path:           domain.Path,
		StripPath:      domain.StripPath,
		TargetURL:      fmt.Sprintf("http://%s:%d", containerName, port),
		TLS:            domain.SslEnabled,
		ForceHTTPS:     domain.ForceHttps,
		SSLMode:        domain.SslMode,
		SSLProvider:    domain.SslProvider.String,
		CertPEM:        cert.CertPEM,
		KeyPEM:         cert.KeyPEM,
		Features:       features,
		AdvancedConfig: domain.AdvancedConfig,
	}); err != nil {
		slog.Warn("rebuildDomainRoute: failed to add proxy route", "hostname", domain.Hostname, "error", err)
	}
}

//apidoc:tag applications/domains
func (h *Handler) RemoveDomain(w http.ResponseWriter, r *http.Request) {
	domainID := chi.URLParam(r, "domainId")
	var domainUUID pgtype.UUID
	if err := domainUUID.Scan(domainID); err != nil {
		writeError(w, http.StatusBadRequest, "invalid domain id")
		return
	}

	// Owner-only: shared access grants full operational use of the project's
	// domains, but not the right to remove one.
	if !h.isDomainOwner(r, domainUUID) {
		writeError(w, http.StatusForbidden, "access denied")
		return
	}

	// Fetch domain to get hostname for proxy removal
	domain, err := h.queries.GetDomain(r.Context(), domainUUID)
	if err != nil {
		writeError(w, http.StatusNotFound, "domain not found")
		return
	}

	// Remove Caddy proxy route first — if it fails, don't delete from DB
	if err := h.proxy.RemoveRoute(r.Context(), domain.Hostname, domain.Path); err != nil {
		slog.Error("failed to remove proxy route for domain", "hostname", domain.Hostname, "path", domain.Path, "error", err)
		writeError(w, http.StatusInternalServerError, "failed to remove proxy route for domain")
		return
	}

	// Delete from database
	if err := h.queries.DeleteDomain(r.Context(), domainUUID); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to remove domain")
		return
	}

	h.audit(r, "remove_domain", "domain", domainID, map[string]any{"hostname": domain.Hostname})

	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}
