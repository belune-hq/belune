package service

import (
	"context"
	"errors"

	"github.com/belune-hq/belune/internal/notify"
	"github.com/belune-hq/belune/internal/service/email"
)

// NewNotifyRegistry builds a notify.Registry wired to the app's SMTP mailer, so
// the email notification provider reuses the same delivery path as the rest of
// the app. Constructed in both the worker and the HTTP handler wiring; the
// registry is stateless, so separate instances are equivalent.
func NewNotifyRegistry(emailSvc *email.Service) *notify.Registry {
	return notify.NewRegistry(emailMailer{svc: emailSvc})
}

// emailMailer adapts *email.Service to notify.Mailer.
type emailMailer struct{ svc *email.Service }

func (m emailMailer) Send(ctx context.Context, msg notify.MailMessage) error {
	if m.svc == nil {
		return errors.New("email: no mailer configured")
	}
	return m.svc.Send(ctx, email.Message{
		To:       msg.To,
		Subject:  msg.Subject,
		TextBody: msg.TextBody,
	})
}

func (m emailMailer) SendWithConfig(ctx context.Context, smtp notify.MailSMTP, msg notify.MailMessage) error {
	if m.svc == nil {
		return errors.New("email: no mailer configured")
	}
	return m.svc.SendWithConfig(ctx, email.SMTPConfig{
		Host:      smtp.Host,
		Port:      smtp.Port,
		User:      smtp.User,
		Password:  smtp.Password,
		FromEmail: smtp.FromEmail,
		FromName:  smtp.FromName,
		TLSMode:   smtp.TLSMode,
	}, email.Message{
		To:       msg.To,
		Subject:  msg.Subject,
		TextBody: msg.TextBody,
	})
}

func (m emailMailer) Configured(ctx context.Context) bool {
	return m.svc != nil && m.svc.Configured(ctx)
}
