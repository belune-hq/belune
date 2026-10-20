package caddy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sort"

	"github.com/belune-hq/belune/internal/pkg/metrics"
	"github.com/belune-hq/belune/internal/proxy"
)

// SetupTLS error handling: transport-level failures are wrapped with
// ErrCaddyUnreachable so the service layer can tell "Caddy is down" apart
// from "Caddy rejected the config".

// certsPath is Caddy's in-band certificate store. Certificates written here are
// held in memory: the PEM never lands on the proxy's filesystem, and — like
// routes — the whole set is lost when Caddy restarts, so the reconciler
// re-asserts it on every pass.
//
// Addressed one level above load_pem deliberately: when the key is absent Caddy
// answers GET .../certificates with a 200 and a null body, but GET
// .../certificates/load_pem with a 400 "invalid traversal path".
const certsPath = "/config/apps/tls/certificates"

// tlsPEMCert is one entry of Caddy's load_pem array. Tags let us recognise the
// certificates Belune owns; Caddy itself selects among them by SNI/SAN.
type tlsPEMCert struct {
	Certificate string   `json:"certificate"`
	Key         string   `json:"key"`
	Tags        []string `json:"tags,omitempty"`
}

func (c *Client) SetupTLS(ctx context.Context, hostname, sslMode, certPEM, keyPEM string) (err error) {
	defer func() { metrics.RecordCaddyCall("setup_tls", err) }()
	switch sslMode {
	case "", "automatic":
		// Nothing to do: srv0 listens on :443 and the route's host matcher is
		// enough for Caddy's automatic HTTPS to obtain a certificate itself.
		slog.Debug("TLS will be auto-provisioned by Caddy", "hostname", hostname)
		return nil

	case "dns_challenge":
		// Withdrawn, and rejected at the API. Reachable only for a domain created
		// before that: the stock caddy image carries no DNS provider modules, so a
		// policy referencing one would be refused here anyway.
		return fmt.Errorf("DNS challenge is not supported; switch this domain to Automatic or Custom")

	case proxy.SSLModeCustom:
		if certPEM == "" || keyPEM == "" {
			return fmt.Errorf("custom SSL mode requires a certificate to be selected")
		}
		return c.loadPEMCert(ctx, proxy.HostCertificate{Hostname: hostname, CertPEM: certPEM, KeyPEM: keyPEM})

	case proxy.SSLModeOff:
		slog.Debug("TLS disabled for hostname", "hostname", hostname)
		return nil

	default:
		return fmt.Errorf("unknown ssl_mode: %s", sslMode)
	}
}

// loadPEMCert appends one certificate to Caddy's in-band store, replacing any
// previous entry for the same hostname.
func (c *Client) loadPEMCert(ctx context.Context, cert proxy.HostCertificate) error {
	current, err := c.listPEMCerts(ctx)
	if err != nil {
		return err
	}

	next := make([]tlsPEMCert, 0, len(current)+1)
	for _, existing := range current {
		if !hasTag(existing, cert.Hostname) {
			next = append(next, existing)
		}
	}
	next = append(next, tlsPEMCert{
		Certificate: cert.CertPEM,
		Key:         cert.KeyPEM,
		Tags:        []string{cert.Hostname},
	})

	return c.writePEMCerts(ctx, next)
}

// SyncCertificates makes Caddy's loaded certificate set match the desired one.
// It is a full replace rather than a diff-and-patch: the set is small, and a
// single write is atomic from Caddy's perspective, so no request can observe a
// window where a live domain's certificate is missing.
func (c *Client) SyncCertificates(ctx context.Context, certs []proxy.HostCertificate) (err error) {
	defer func() { metrics.RecordCaddyCall("sync_certificates", err) }()

	current, err := c.listPEMCerts(ctx)
	if err != nil {
		return err
	}

	desired := make([]tlsPEMCert, 0, len(certs))
	for _, cert := range certs {
		if cert.CertPEM == "" || cert.KeyPEM == "" {
			continue
		}
		desired = append(desired, tlsPEMCert{
			Certificate: cert.CertPEM,
			Key:         cert.KeyPEM,
			Tags:        []string{cert.Hostname},
		})
	}
	sort.Slice(desired, func(i, j int) bool { return desired[i].Tags[0] < desired[j].Tags[0] })

	// Caddy reloads its whole TLS app on any write here, dropping and re-issuing
	// nothing but still doing real work, so skip the write when nothing changed.
	if pemCertsEqual(current, desired) {
		return nil
	}
	return c.writePEMCerts(ctx, desired)
}

// listPEMCerts reads the currently loaded certificates. A fresh or restarted
// Caddy has no certificates object at all, which is an empty set, not an error.
func (c *Client) listPEMCerts(ctx context.Context) ([]tlsPEMCert, error) {
	url := c.adminURL + certsPath
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		if isTransportError(err) {
			return nil, fmt.Errorf("%w: %v", ErrCaddyUnreachable, err)
		}
		return nil, fmt.Errorf("list certificates: %w", err)
	}
	defer resp.Body.Close()

	// "No certificates" arrives in three different shapes depending on how much of
	// the TLS app exists, and none of them is an error:
	//
	//   - the tls app is absent entirely (a stock Caddy with no TLS config, i.e.
	//     production): 400 "invalid traversal path" — the path cannot even be
	//     walked;
	//   - the tls app exists but has no certificates object: 200 with a null body;
	//   - 404, for completeness.
	//
	// Development has a tls app (local_certs creates one), which hid the 400 case
	// entirely — and because SyncCertificates bails on this error before reaching
	// the write, uploaded certificates were never pushed to a production Caddy.
	if resp.StatusCode == http.StatusNotFound {
		return nil, nil
	}
	if resp.StatusCode >= 400 {
		respBody, _ := io.ReadAll(resp.Body)
		if resp.StatusCode == http.StatusBadRequest && bytes.Contains(respBody, []byte("invalid traversal path")) {
			return nil, nil
		}
		return nil, fmt.Errorf("list certificates: HTTP %d: %s", resp.StatusCode, string(respBody))
	}

	// Absent key → literal `null`; present but empty → {"load_pem": []}.
	var wrapper struct {
		LoadPEM []tlsPEMCert `json:"load_pem"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&wrapper); err != nil {
		return nil, fmt.Errorf("decode certificates: %w", err)
	}
	return wrapper.LoadPEM, nil
}

// writePEMCerts replaces the whole load_pem array.
func (c *Client) writePEMCerts(ctx context.Context, certs []tlsPEMCert) error {
	if certs == nil {
		certs = []tlsPEMCert{}
	}
	if err := c.putOrPatchConfig(ctx, certsPath, map[string]any{"load_pem": certs}); err != nil {
		return fmt.Errorf("write certificates: %w", err)
	}
	slog.Debug("caddy: certificates loaded", "count", len(certs))
	return nil
}

func hasTag(cert tlsPEMCert, tag string) bool {
	for _, t := range cert.Tags {
		if t == tag {
			return true
		}
	}
	return false
}

// pemCertsEqual compares two certificate sets. Both sides are tag-sorted by the
// caller, so a positional comparison is sufficient.
func pemCertsEqual(a, b []tlsPEMCert) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].Certificate != b[i].Certificate || a[i].Key != b[i].Key {
			return false
		}
		if len(a[i].Tags) != len(b[i].Tags) {
			return false
		}
		for j := range a[i].Tags {
			if a[i].Tags[j] != b[i].Tags[j] {
				return false
			}
		}
	}
	return true
}

// automationPath is where Caddy keeps its issuance policy. It is absent on a
// stock/production Caddy (which uses the default public ACME issuers) and only
// materialises when something — local_certs, in our case — configures one.
const automationPath = "/config/apps/tls/automation"

// UsesInternalIssuer reports whether Caddy is configured to issue certificates
// from its own internal CA, which is what `local_certs` sets up in development.
//
// Read from the configuration, never inferred from the certificate on the wire:
// a production Caddy whose ACME issuance has failed *also* serves an internal
// certificate, and conflating the two would report broken public HTTPS as a
// perfectly healthy local setup — precisely the lie the TLS status pipeline
// exists to prevent.
func (c *Client) UsesInternalIssuer(ctx context.Context) (used bool, err error) {
	defer func() { metrics.RecordCaddyCall("uses_internal_issuer", err) }()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.adminURL+automationPath, nil)
	if err != nil {
		return false, fmt.Errorf("create request: %w", err)
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		if isTransportError(err) {
			return false, fmt.Errorf("%w: %v", ErrCaddyUnreachable, err)
		}
		return false, fmt.Errorf("get tls automation: %w", err)
	}
	defer resp.Body.Close()

	// No automation configured is the production case: Caddy falls back to its
	// default public issuers. Caddy expresses "absent" as a 400 on the traversal
	// or as a 200 with a null body, depending on how much of the tls app exists —
	// the same shapes listPEMCerts has to cope with. Neither is an error.
	if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusBadRequest {
		return false, nil
	}
	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(resp.Body)
		return false, fmt.Errorf("get tls automation: HTTP %d: %s", resp.StatusCode, string(body))
	}

	var automation struct {
		Policies []struct {
			Issuers []struct {
				Module string `json:"module"`
			} `json:"issuers"`
		} `json:"policies"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&automation); err != nil {
		return false, fmt.Errorf("decode tls automation: %w", err)
	}
	for _, p := range automation.Policies {
		for _, iss := range p.Issuers {
			if iss.Module == "internal" {
				return true, nil
			}
		}
	}
	return false, nil
}
