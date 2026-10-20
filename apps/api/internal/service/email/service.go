// Package email provides an async-safe email sending service with template support.
// When SMTP_HOST is empty, messages are written to slog instead of dialed.
package email

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
	"time"

	mail "github.com/wneessen/go-mail"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/belune-hq/belune/internal/config"
	"github.com/belune-hq/belune/internal/pkg/metrics"
	"github.com/belune-hq/belune/internal/pkg/tracing"
)

// Message represents a single outbound email.
type Message struct {
	To       string
	Subject  string
	TextBody string
	HTMLBody string
	Headers  map[string]string
}

// Service sends emails via SMTP or logs them when no SMTP host is configured.
type Service struct {
	cfg      *config.Config
	baseURL  *url.URL // parsed PUBLIC_BASE_URL; nil if absent/unparseable
	registry map[string]*templateDef
	resolver SMTPResolver // optional; supplies DB-backed SMTP config per send
}

// SMTPConfig is the effective mail-server configuration for a single send. It is
// resolved per-send so changes made in the admin UI take effect without a
// restart.
type SMTPConfig struct {
	Host      string
	Port      int
	User      string
	Password  string
	FromEmail string
	FromName  string
	TLSMode   string
}

// SMTPResolver supplies the current effective SMTPConfig (DB settings merged over
// env defaults). Implemented in the service layer and injected via SetResolver;
// when absent, the env config is used.
type SMTPResolver interface {
	ResolveSMTP(ctx context.Context) (SMTPConfig, error)
}

// SetResolver installs the DB-backed SMTP resolver. Call once at startup on the
// shared Service instance; all senders (worker, handler) then pick up live
// settings.
func (s *Service) SetResolver(r SMTPResolver) { s.resolver = r }

// Configured reports whether the effective (DB-or-env) SMTP has a host set —
// i.e. whether a real send would happen rather than falling into log-only mode.
func (s *Service) Configured(ctx context.Context) bool {
	return s.effectiveSMTP(ctx).Host != ""
}

// effectiveSMTP returns the config to use for a send: the resolver's value when
// present (falling back to env on error), otherwise the env config.
func (s *Service) effectiveSMTP(ctx context.Context) SMTPConfig {
	if s.resolver != nil {
		if c, err := s.resolver.ResolveSMTP(ctx); err == nil {
			return c
		} else {
			slog.WarnContext(ctx, "smtp: resolve settings failed, using env config", "error", err)
		}
	}
	return s.envSMTP()
}

func (s *Service) envSMTP() SMTPConfig {
	return SMTPConfig{
		Host:      s.cfg.SMTPHost,
		Port:      s.cfg.SMTPPort,
		User:      s.cfg.SMTPUser,
		Password:  s.cfg.SMTPPassword,
		FromEmail: s.cfg.SMTPFromEmail,
		FromName:  s.cfg.SMTPFromName,
		TLSMode:   s.cfg.SMTPTLSMode,
	}
}

// New constructs an email Service from cfg. Returns an error if the embedded
// templates cannot be parsed — this indicates a broken binary and the caller
// should log and exit.
func New(cfg *config.Config) (*Service, error) {
	reg, err := loadTemplates()
	if err != nil {
		return nil, err
	}

	svc := &Service{cfg: cfg, registry: reg}

	// PUBLIC_BASE_URL gates sending (email links must be absolute). Parse it
	// independent of SMTP config: SMTP may be configured later through the admin
	// UI, while the base URL stays an env-level concern.
	if cfg.PublicBaseURL != "" {
		if u, err := url.Parse(cfg.PublicBaseURL); err == nil && u.Host != "" {
			svc.baseURL = u
		} else {
			slog.Warn("PUBLIC_BASE_URL is not a valid absolute URL — email sending disabled until it is fixed",
				"public_base_url", cfg.PublicBaseURL)
		}
	}

	return svc, nil
}

// PublicURL returns the configured PUBLIC_BASE_URL. Returns "" if not set.
func (s *Service) PublicURL() string {
	return s.cfg.PublicBaseURL
}

// Render executes a named template with vars and returns (subject, textBody, htmlBody).
// Useful for tests and future debugging surfaces.
func (s *Service) Render(templateID string, vars any) (subject, textBody, htmlBody string, err error) {
	return renderTemplate(s.registry, templateID, vars)
}

// SendTemplate renders the named template with vars and sends it to addr.
// Always called from the async email task — never from a hot path.
func (s *Service) SendTemplate(ctx context.Context, templateID, addr string, vars any) error {
	ctx, span := tracing.Tracer().Start(ctx, "email.send",
		trace.WithAttributes(
			attribute.String("email.template", templateID),
			attribute.String("email.recipient_hash", hashRecipient(addr)),
		),
	)
	defer span.End()

	subject, textBody, htmlBody, err := renderTemplate(s.registry, templateID, vars)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return err
	}

	msg := Message{
		To:       addr,
		Subject:  subject,
		TextBody: textBody,
		HTMLBody: htmlBody,
	}

	eff := s.effectiveSMTP(ctx)

	// Only instrument when SMTP is actually configured; log-only sends are
	// cheap no-ops that would skew duration histograms with near-zero values.
	if eff.Host == "" {
		return s.sendWith(ctx, eff, msg)
	}

	start := time.Now()
	sendErr := s.sendWith(ctx, eff, msg)
	metrics.RecordSMTPSend(templateID, sendErr, time.Since(start))
	if sendErr != nil {
		span.RecordError(sendErr)
		span.SetStatus(codes.Error, sendErr.Error())
	}
	return sendErr
}

// Send delivers msg using the effective SMTP config (DB settings over env). If
// no SMTP host is configured, the rendered message is written to slog at INFO
// level (dev/log-only fallback).
func (s *Service) Send(ctx context.Context, msg Message) error {
	return s.sendWith(ctx, s.effectiveSMTP(ctx), msg)
}

// SendWithConfig delivers msg using an explicit SMTP config, bypassing the
// resolver. Used by the settings "send test" endpoint to exercise unsaved form
// values.
func (s *Service) SendWithConfig(ctx context.Context, cfg SMTPConfig, msg Message) error {
	return s.sendWith(ctx, cfg, msg)
}

func (s *Service) sendWith(ctx context.Context, eff SMTPConfig, msg Message) error {
	if eff.Host == "" {
		slog.InfoContext(ctx, "email (log-only mode)",
			"to", msg.To,
			"subject", msg.Subject,
			"text_body", msg.TextBody,
		)
		return nil
	}

	// SMTP is configured but PUBLIC_BASE_URL is not — links would be broken, so we
	// can't send. Return an error rather than a silent no-op: otherwise a "send
	// test" would report success and a channel would be marked delivered while
	// nothing was ever dialed.
	if s.baseURL == nil {
		return fmt.Errorf("email: PUBLIC_BASE_URL is not configured — set it so links resolve")
	}

	m := mail.NewMsg()
	if err := m.FromFormat(eff.FromName, eff.FromEmail); err != nil {
		return fmt.Errorf("email: set from: %w", err)
	}
	if err := m.To(msg.To); err != nil {
		return fmt.Errorf("email: set to: %w", err)
	}
	m.Subject(msg.Subject)
	if msg.TextBody != "" {
		m.SetBodyString(mail.TypeTextPlain, msg.TextBody)
	}
	if msg.HTMLBody != "" {
		m.AddAlternativeString(mail.TypeTextHTML, msg.HTMLBody)
	}
	for k, v := range msg.Headers {
		m.SetGenHeader(mail.Header(k), v)
	}

	client, err := s.newClient(eff)
	if err != nil {
		return fmt.Errorf("email: create client: %w", err)
	}

	dialCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	_, smtpSpan := tracing.Tracer().Start(ctx, "email.smtp.send")
	defer smtpSpan.End()

	if err := client.DialAndSendWithContext(dialCtx, m); err != nil {
		smtpSpan.RecordError(err)
		smtpSpan.SetStatus(codes.Error, err.Error())
		return fmt.Errorf("email: send: %w", err)
	}

	slog.InfoContext(ctx, "email sent", "to", msg.To, "subject", msg.Subject)
	return nil
}

// hashRecipient returns a short SHA-256 hex digest of addr for trace attributes.
// The full address is never embedded in traces so PII stays out of telemetry.
func hashRecipient(addr string) string {
	sum := sha256.Sum256([]byte(strings.ToLower(strings.TrimSpace(addr))))
	return hex.EncodeToString(sum[:8])
}

func (s *Service) newClient(eff SMTPConfig) (*mail.Client, error) {
	port := eff.Port
	if port == 0 {
		port = 587
	}
	opts := []mail.Option{
		mail.WithPort(port),
		mail.WithTimeout(30 * time.Second),
	}

	if eff.User != "" {
		opts = append(opts, mail.WithUsername(eff.User))
	}
	if eff.Password != "" {
		opts = append(opts, mail.WithPassword(eff.Password))
	}

	switch eff.TLSMode {
	case "tls":
		opts = append(opts, mail.WithSSL())
	case "none":
		opts = append(opts, mail.WithTLSPolicy(mail.NoTLS))
	default: // "starttls" or unset
		opts = append(opts, mail.WithTLSPolicy(mail.TLSMandatory))
	}

	return mail.NewClient(eff.Host, opts...)
}
