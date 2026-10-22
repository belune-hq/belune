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
	"strings"

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

	case proxy.SSLModeDNSChallenge:
		// Nothing to push per hostname: issuance is configured by the automation
		// policy SyncDNSPolicies owns, which the route's host matcher then selects.
		// If the policy is absent the reconciler reports the domain as degraded.
		slog.Debug("TLS will be issued over the DNS challenge", "hostname", hostname)
		return nil

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

// SyncDNSPolicies makes the DNS-01 automation policies in Caddy match the given
// set, leaving every policy Belune does not own untouched (development's
// local_certs issuer lives in the same list).
//
// Read-modify-write on the policies array, skipped when nothing changed: any
// write here reloads Caddy's whole TLS app, and the reconciler calls this every
// pass. Ours go first so a specific DNS policy is not shadowed by a catch-all.
//
// Unlike the config writers around it this returns Caddy's refusal. doConfig only
// logs a 4xx, which suits a best-effort route push but would hide the one failure
// that matters here: Caddy rejecting a policy (an unknown provider field) leaves
// the domain on HTTP-01 for a name HTTP-01 can never validate.
func (c *Client) SyncDNSPolicies(ctx context.Context, policies []proxy.DNSPolicy) (err error) {
	defer func() { metrics.RecordCaddyCall("sync_dns_policies", err) }()

	automation, err := c.readAutomation(ctx)
	if err != nil {
		return err
	}

	var foreign []json.RawMessage
	var currentOurs []json.RawMessage
	if raw, ok := automation["policies"]; ok {
		var existing []json.RawMessage
		if err := json.Unmarshal(raw, &existing); err != nil {
			return fmt.Errorf("decode tls policies: %w", err)
		}
		for _, p := range existing {
			if isOurPolicy(p) {
				currentOurs = append(currentOurs, p)
			} else {
				foreign = append(foreign, p)
			}
		}
	}

	desired := make([]json.RawMessage, 0, len(policies))
	for _, p := range policies {
		raw, err := json.Marshal(p.CaddyPolicy())
		if err != nil {
			return fmt.Errorf("marshal dns policy: %w", err)
		}
		desired = append(desired, raw)
	}

	if sameJSONSet(currentOurs, desired) {
		return nil
	}
	// Nothing of ours to remove and nothing to add: do not create an automation
	// object on a Caddy that never had one.
	if len(desired) == 0 && len(currentOurs) == 0 {
		return nil
	}

	automation["policies"] = mustMarshal(append(desired, foreign...))
	return c.writeAutomation(ctx, automation)
}

// readAutomation returns the tls automation object as raw members, or an empty
// map when Caddy has none — the production case, in either of the shapes the
// reader above also accepts.
func (c *Client) readAutomation(ctx context.Context) (map[string]json.RawMessage, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.adminURL+automationPath, nil)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		if isTransportError(err) {
			return nil, fmt.Errorf("%w: %v", ErrCaddyUnreachable, err)
		}
		return nil, fmt.Errorf("get tls automation: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusBadRequest {
		return map[string]json.RawMessage{}, nil
	}
	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("get tls automation: HTTP %d: %s", resp.StatusCode, string(body))
	}
	out := map[string]json.RawMessage{}
	// A null body (the tls app exists, automation does not) decodes to the empty map.
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("decode tls automation: %w", err)
	}
	if out == nil {
		out = map[string]json.RawMessage{}
	}
	return out, nil
}

// writeAutomation replaces the automation object. PUT creates it where absent
// (including the parent tls app, as the certificates writer relies on); an
// existing one answers 409 and is replaced with PATCH.
func (c *Client) writeAutomation(ctx context.Context, automation map[string]json.RawMessage) error {
	status, err := c.doConfig(ctx, http.MethodPut, automationPath, automation)
	if err == nil && status == http.StatusConflict {
		status, err = c.doConfig(ctx, http.MethodPatch, automationPath, automation)
	}
	if err != nil {
		return fmt.Errorf("write tls automation: %w", err)
	}
	switch {
	case status == 0:
		return fmt.Errorf("%w: write tls automation", ErrCaddyUnreachable)
	case status >= 400:
		return fmt.Errorf("write tls automation: HTTP %d (see the caddy config update warning above)", status)
	}
	return nil
}

// isOurPolicy reports whether a policy carries Belune's @id prefix.
func isOurPolicy(raw json.RawMessage) bool {
	var p struct {
		ID string `json:"@id"`
	}
	return json.Unmarshal(raw, &p) == nil && strings.HasPrefix(p.ID, proxy.PolicyIDPrefix)
}

// sameJSONSet compares two policy lists by canonical JSON, order-insensitively.
// Caddy returns members re-encoded with its own key order, so raw bytes differ
// from ours for the same policy.
func sameJSONSet(a, b []json.RawMessage) bool {
	if len(a) != len(b) {
		return false
	}
	canon := func(list []json.RawMessage) []string {
		out := make([]string, 0, len(list))
		for _, raw := range list {
			var v any
			if json.Unmarshal(raw, &v) != nil {
				out = append(out, string(raw))
				continue
			}
			norm, _ := json.Marshal(v) // map keys marshal sorted
			out = append(out, string(norm))
		}
		sort.Strings(out)
		return out
	}
	ca, cb := canon(a), canon(b)
	for i := range ca {
		if ca[i] != cb[i] {
			return false
		}
	}
	return true
}

func mustMarshal(v any) json.RawMessage {
	raw, err := json.Marshal(v)
	if err != nil {
		panic(err) // only plain maps and RawMessages reach here
	}
	return raw
}
