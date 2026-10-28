package metrics

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

var (
	clientIPDecisions = factory.NewCounterVec(prometheus.CounterOpts{
		Name: "belune_client_ip_requests_total",
		Help: "Requests by how the client address was resolved: proxy (trusted header from Caddy), proxy_no_header, proxy_bad_header, host, direct (peer unknown), rejected.",
	}, []string{"result"})

	auditLogsDropped = factory.NewCounter(prometheus.CounterOpts{
		Name: "belune_audit_logs_dropped_total",
		Help: "Audit rows that could not be written. Each one is an action with no record of it.",
	})

	smtpSendDuration = factory.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "belune_smtp_send_duration_seconds",
		Help:    "Duration of outbound SMTP sends by template and result.",
		Buckets: []float64{0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 15, 30},
	}, []string{"template", "result"})

	smtpSendTotal = factory.NewCounterVec(prometheus.CounterOpts{
		Name: "belune_smtp_send_total",
		Help: "Total outbound SMTP sends by template and result.",
	}, []string{"template", "result"})

	backupRunDuration = factory.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "belune_backup_run_duration_seconds",
		Help:    "Backup run durations by destination and result.",
		Buckets: []float64{1, 5, 10, 30, 60, 120, 300, 600},
	}, []string{"destination", "result"})

	backupRunTotal = factory.NewCounterVec(prometheus.CounterOpts{
		Name: "belune_backup_run_total",
		Help: "Total backup runs by destination and result.",
	}, []string{"destination", "result"})

	webhookDeliveryDuration = factory.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "belune_webhook_delivery_duration_seconds",
		Help:    "End-to-end webhook processing duration by source (github/gitlab/unknown).",
		Buckets: []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5},
	}, []string{"source"})

	passwordResetIssued = factory.NewCounter(prometheus.CounterOpts{
		Name: "belune_password_reset_issued_total",
		Help: "Total password reset tokens issued.",
	})

	passwordResetRedeemed = factory.NewCounter(prometheus.CounterOpts{
		Name: "belune_password_reset_redeemed_total",
		Help: "Total password reset tokens redeemed.",
	})

	invitationIssued = factory.NewCounter(prometheus.CounterOpts{
		Name: "belune_invitation_issued_total",
		Help: "Total user invitations issued.",
	})

	invitationAccepted = factory.NewCounter(prometheus.CounterOpts{
		Name: "belune_invitation_accepted_total",
		Help: "Total user invitations accepted.",
	})

	terminalSessionStarted = factory.NewCounter(prometheus.CounterOpts{
		Name: "belune_terminal_session_started_total",
		Help: "Total terminal sessions created.",
	})

	terminalSessionClosed = factory.NewCounter(prometheus.CounterOpts{
		Name: "belune_terminal_session_closed_total",
		Help: "Total terminal sessions closed.",
	})

	terminalSessionDuration = factory.NewHistogram(prometheus.HistogramOpts{
		Name:    "belune_terminal_session_duration_seconds",
		Help:    "Duration of terminal sessions.",
		Buckets: []float64{1, 5, 15, 30, 60, 120, 300, 600, 1800, 3600},
	})

	backupRotateTotal = factory.NewCounterVec(prometheus.CounterOpts{
		Name: "belune_backup_rotate_total",
		Help: "Total backup rotation runs by result.",
	}, []string{"result"})
)

// RecordSMTPSend observes the SMTP send duration and increments the counter.
func RecordSMTPSend(templateID string, err error, d time.Duration) {
	lbl := resultLabel(err)
	smtpSendDuration.WithLabelValues(templateID, lbl).Observe(d.Seconds())
	smtpSendTotal.WithLabelValues(templateID, lbl).Inc()
}

// RecordBackupRun observes the backup run duration and increments the counter.
// destination should be "local" or "remote".
func RecordBackupRun(destination string, err error, d time.Duration) {
	lbl := resultLabel(err)
	backupRunDuration.WithLabelValues(destination, lbl).Observe(d.Seconds())
	backupRunTotal.WithLabelValues(destination, lbl).Inc()
}

// RecordWebhookDelivery observes the end-to-end webhook processing duration.
// source is "github", "gitlab", or "unknown". The handler always returns 200
// regardless of outcome, so there is no result dimension on this metric.
func RecordWebhookDelivery(source string, d time.Duration) {
	webhookDeliveryDuration.WithLabelValues(source).Observe(d.Seconds())
}

// RecordPasswordResetIssued increments the password reset issued counter.
func RecordPasswordResetIssued() { passwordResetIssued.Inc() }

// RecordPasswordResetRedeemed increments the password reset redeemed counter.
func RecordPasswordResetRedeemed() { passwordResetRedeemed.Inc() }

// RecordInvitationIssued increments the invitation issued counter.
func RecordInvitationIssued() { invitationIssued.Inc() }

// RecordInvitationAccepted increments the invitation accepted counter.
func RecordInvitationAccepted() { invitationAccepted.Inc() }

// RecordTerminalSessionStarted increments the terminal session started counter.
func RecordTerminalSessionStarted() { terminalSessionStarted.Inc() }

// RecordTerminalSessionClosed increments the closed counter and observes the session duration.
func RecordTerminalSessionClosed(d time.Duration) {
	terminalSessionClosed.Inc()
	terminalSessionDuration.Observe(d.Seconds())
}

// RecordBackupRotate increments the backup rotation counter.
func RecordBackupRotate(err error) {
	backupRotateTotal.WithLabelValues(resultLabel(err)).Inc()
}

// RecordClientIPDecision counts how a request's client address was resolved.
func RecordClientIPDecision(result string) { clientIPDecisions.WithLabelValues(result).Inc() }

// RecordAuditLogDropped counts an audit row that failed to insert.
func RecordAuditLogDropped() { auditLogsDropped.Inc() }
