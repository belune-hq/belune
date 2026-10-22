package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/belune-hq/belune/internal/notify"
	"github.com/belune-hq/belune/internal/proxy"
	"github.com/belune-hq/belune/internal/store/generated"
	"github.com/belune-hq/belune/internal/tlsstatus"
)

// TLS status values. The state machine and the probe itself live in the
// tlsstatus package, shared with the handler that reports the dashboard's own
// certificate — there must be exactly one definition of what "active" means.
const (
	TLSStatusUnknown  = tlsstatus.StatusUnknown
	TLSStatusDisabled = tlsstatus.StatusDisabled
	TLSStatusPending  = tlsstatus.StatusPending
	TLSStatusActive   = tlsstatus.StatusActive
	TLSStatusExpiring = tlsstatus.StatusExpiring
	TLSStatusExpired  = tlsstatus.StatusExpired
	TLSStatusFailed   = tlsstatus.StatusFailed
)

// HandleTLSStatusSweep probes every domain and records what the proxy actually
// serves for it. Registered @every 1m; domain create/update also enqueues a
// one-shot so the badge moves within seconds rather than up to a minute.
func (h *TaskHandler) HandleTLSStatusSweep(ctx context.Context) {
	domains, err := h.Queries.ListDomainsForTLSProbe(ctx)
	if err != nil {
		slog.Warn("tls probe: failed to list domains", "error", err)
		return
	}

	for _, d := range domains {
		h.probeDomain(ctx, d)
	}
}

// probeDomain probes one domain and persists the result, notifying on a
// transition into a state the operator needs to act on.
func (h *TaskHandler) probeDomain(ctx context.Context, d generated.ListDomainsForTLSProbeRow) {
	// Whether the proxy issues from its own CA decides what an internal certificate
	// means: the finished state on a dev box, or public HTTPS that has not worked
	// on a real one. Best-effort — if the proxy cannot say, assume a public CA is
	// expected, which is the conservative reading.
	internalIssuer := false
	if h.Proxy != nil {
		if v, err := h.Proxy.UsesInternalIssuer(ctx); err == nil {
			internalIssuer = v
		}
	}

	recordedErr := d.TlsError.String

	// Check DNS before the handshake: a hostname pointing somewhere else can
	// never get a certificate, and saying so is far more useful than a generic
	// "pending" that never resolves.
	//
	// Only a fatal finding (the record does not exist) may override the recorded
	// ACME error and so decide the status. An advisory one — "resolves somewhere
	// that isn't us" — is what a proxy in front of us looks like, and issuance
	// through one works, so it is surfaced without condemning the domain.
	var advisory string
	// A wildcard has no record of its own name to resolve; the apex and each
	// subdomain are what DNS holds, and the probe below covers the certificate.
	if d.SslMode != proxy.SSLModeOff && !strings.HasPrefix(d.Hostname, "*.") {
		if msg, fatal := h.checkDNS(ctx, d.Hostname); msg != "" {
			if fatal {
				recordedErr = msg
			} else {
				advisory = msg
			}
		}
	}

	probeName := tlsstatus.ProbeName(d.Hostname)
	leaf, dialErr := tlsstatus.Probe(ctx, h.Config.CaddyTLSProbeAddr, probeName)
	res := tlsstatus.Derive(d.SslMode, leaf, probeName, dialErr, recordedErr, time.Now(), internalIssuer)

	// The advisory is persisted to its own column, never to tls_error. Writing it
	// to tls_error made it indistinguishable from a real ACME failure when the
	// next sweep read it back as recordedErr, which escalated the domain to
	// "failed" — so a proxied domain, which always resolves to the proxy rather
	// than to us, went red one minute into a perfectly healthy issuance.
	// A live certificate settles the question, so drop the suspicion once serving.
	if res.Status == tlsstatus.StatusActive || res.Status == tlsstatus.StatusExpiring {
		advisory = ""
	}

	var notAfter pgtype.Timestamptz
	if !res.NotAfter.IsZero() {
		notAfter = pgtype.Timestamptz{Time: res.NotAfter, Valid: true}
	}
	// Keyed by hostname: this settles every row serving the name, not just the one
	// the sweep happened to pick as its representative.
	if err := h.Queries.UpdateDomainTLSStatus(ctx, generated.UpdateDomainTLSStatusParams{
		Hostname:    d.Hostname,
		TlsStatus:   res.Status,
		TlsIssuer:   pgtype.Text{String: res.Issuer, Valid: res.Issuer != ""},
		TlsNotAfter: notAfter,
		TlsError:    pgtype.Text{String: res.Error, Valid: res.Error != ""},
		TlsAdvisory: pgtype.Text{String: advisory, Valid: advisory != ""},
	}); err != nil {
		slog.Warn("tls probe: failed to record status", "hostname", d.Hostname, "error", err)
		return
	}

	if res.Status != d.TlsStatus {
		slog.Info("tls status changed", "hostname", d.Hostname, "from", d.TlsStatus, "to", res.Status, "error", res.Error)
		h.notifyTLSTransition(ctx, d.Hostname, res)
	}
}

// notifyTLSTransition tells admins when a domain enters a state that needs
// human action. Only transitions notify, so a domain that stays broken does not
// re-notify every minute.
func (h *TaskHandler) notifyTLSTransition(ctx context.Context, hostname string, res tlsstatus.ProbeResult) {
	if h.Notifier == nil && h.NotifyChannels == nil {
		return
	}
	var title, body string
	switch res.Status {
	case TLSStatusFailed:
		title = "TLS certificate failed"
		body = fmt.Sprintf("%s could not get a certificate: %s", hostname, res.Error)
	case TLSStatusExpired:
		title = "TLS certificate expired"
		body = fmt.Sprintf("The certificate for %s has expired. HTTPS is broken for this domain.", hostname)
	case TLSStatusExpiring:
		title = "TLS certificate expiring"
		body = fmt.Sprintf("The certificate for %s expires %s.", hostname, res.NotAfter.Format("2 Jan 2006"))
	default:
		return
	}

	notifType := "tls." + res.Status

	// Provider channels fire once per transition, not once per admin.
	h.dispatchToChannels(ctx, notify.Event{
		Type: notifType, Title: title, Body: body, Link: "/certificates", OccurredAt: time.Now(),
	})

	if h.Notifier == nil {
		return
	}
	admins, err := h.Queries.ListAdminUserIDs(ctx)
	if err != nil {
		slog.Warn("tls probe: failed to list admins for notification", "error", err)
		return
	}
	for _, id := range admins {
		h.Notifier.Notify(formatUUID(id), notifType, title, body, "/certificates")
	}
}

// TLSProbePayload targets a single domain for an immediate re-probe.
type TLSProbePayload struct {
	DomainID string `json:"domain_id"`
}

// HandleTLSProbeTask re-probes one domain now: enqueued when a domain is created
// or updated, and by the UI's "Recheck" action.
func (h *TaskHandler) HandleTLSProbeTask(ctx context.Context, payload []byte) error {
	var p TLSProbePayload
	if err := json.Unmarshal(payload, &p); err != nil {
		return fmt.Errorf("unmarshal tls probe payload: %w", err)
	}
	var id pgtype.UUID
	if err := id.Scan(p.DomainID); err != nil {
		return fmt.Errorf("invalid domain id: %w", err)
	}

	domains, err := h.Queries.ListDomainsForTLSProbe(ctx)
	if err != nil {
		return err
	}
	for _, d := range domains {
		if d.ID == id {
			h.probeDomain(ctx, d)
			return nil
		}
	}
	return nil
}
