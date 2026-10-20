package email_test

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/belune-hq/belune/internal/config"
	"github.com/belune-hq/belune/internal/service/email"
)

// logOnlyCfg returns a config with no SMTP host (log-only mode).
func logOnlyCfg() *config.Config {
	return &config.Config{
		SMTPHost:      "",
		SMTPPort:      587,
		SMTPFromEmail: "noreply@example.com",
		SMTPFromName:  "Belune",
		SMTPTLSMode:   "starttls",
		PublicBaseURL: "https://belune.example.com",
	}
}

func TestSend_LogOnly(t *testing.T) {
	svc, err := email.New(logOnlyCfg())
	require.NoError(t, err)
	err = svc.Send(context.Background(), email.Message{
		To:       "user@example.com",
		Subject:  "Test",
		TextBody: "Hello",
	})
	require.NoError(t, err, "log-only mode must not return an error")
}

func TestTemplates_PasswordReset(t *testing.T) {
	svc, err := email.New(logOnlyCfg())
	require.NoError(t, err)

	vars := map[string]any{
		"FirstName": "Alice",
		"ResetURL":  "https://belune.example.com/reset-password?token=abc123",
	}

	subject, textBody, htmlBody, err := svc.Render("password_reset", vars)
	require.NoError(t, err)

	assert.Equal(t, "Reset your password", subject)
	assert.Contains(t, textBody, "Alice")
	assert.Contains(t, textBody, "https://belune.example.com/reset-password?token=abc123")
	assert.Contains(t, textBody, "30 minutes")
	assert.Contains(t, htmlBody, "Reset your password")
	assert.Contains(t, htmlBody, "https://belune.example.com/reset-password?token=abc123")
	assert.Contains(t, htmlBody, "no-referrer")

	// Verify round-trip through log-only send doesn't error.
	err = svc.SendTemplate(context.Background(), "password_reset", "alice@example.com", vars)
	require.NoError(t, err)
}

func TestTemplates_UserInvitation(t *testing.T) {
	svc, err := email.New(logOnlyCfg())
	require.NoError(t, err)

	vars := map[string]any{
		"Role":      "operator",
		"InviteURL": "https://belune.example.com/accept-invite?token=xyz789",
	}

	subject, textBody, htmlBody, err := svc.Render("user_invitation", vars)
	require.NoError(t, err)

	assert.Equal(t, "You've been invited to Belune", subject)
	assert.Contains(t, textBody, "operator")
	assert.Contains(t, textBody, "https://belune.example.com/accept-invite?token=xyz789")
	assert.Contains(t, textBody, "7 days")
	assert.Contains(t, htmlBody, "operator")
	assert.Contains(t, htmlBody, "https://belune.example.com/accept-invite?token=xyz789")
	assert.Contains(t, htmlBody, "no-referrer")

	err = svc.SendTemplate(context.Background(), "user_invitation", "new@example.com", vars)
	require.NoError(t, err)
}

func TestTemplates_AlertDeployFailed(t *testing.T) {
	svc, err := email.New(logOnlyCfg())
	require.NoError(t, err)

	vars := map[string]any{
		"AppName":      "my-app",
		"ProjectName":  "my-project",
		"DeploymentID": "deploy-uuid-123",
		"FailedAt":     "2026-05-06T12:00:00Z",
		"ErrorMessage": "container exited with code 1",
	}

	subject, textBody, htmlBody, err := svc.Render("alert_deploy_failed", vars)
	require.NoError(t, err)

	assert.Equal(t, "Deployment failed", subject)
	assert.Contains(t, textBody, "my-app")
	assert.Contains(t, textBody, "my-project")
	assert.Contains(t, textBody, "deploy-uuid-123")
	assert.Contains(t, textBody, "container exited with code 1")
	assert.Contains(t, htmlBody, "my-app")
	assert.Contains(t, htmlBody, "deploy-uuid-123")
	assert.Contains(t, htmlBody, "container exited with code 1")
}

func TestTemplates_AlertDeployFailed_NoError(t *testing.T) {
	// ErrorMessage omitted — conditional block must not render the error row.
	svc, err := email.New(logOnlyCfg())
	require.NoError(t, err)

	vars := map[string]any{
		"AppName":      "my-app",
		"ProjectName":  "my-project",
		"DeploymentID": "deploy-uuid-123",
		"FailedAt":     "2026-05-06T12:00:00Z",
		"ErrorMessage": "",
	}

	_, textBody, htmlBody, err := svc.Render("alert_deploy_failed", vars)
	require.NoError(t, err)

	assert.NotContains(t, textBody, "Error:")
	// The conditional error row must be absent when ErrorMessage is empty.
	assert.NotContains(t, htmlBody, ">Error<")
}

func TestTemplates_AlertBuildFailed(t *testing.T) {
	svc, err := email.New(logOnlyCfg())
	require.NoError(t, err)

	vars := map[string]any{
		"AppName":      "my-app",
		"ProjectName":  "my-project",
		"BuildID":      "build-uuid-456",
		"FailedAt":     "2026-05-06T12:00:00Z",
		"ErrorMessage": "dockerfile parse error",
	}

	subject, textBody, htmlBody, err := svc.Render("alert_build_failed", vars)
	require.NoError(t, err)

	assert.Equal(t, "Build failed", subject)
	assert.Contains(t, textBody, "my-app")
	assert.Contains(t, textBody, "build-uuid-456")
	assert.Contains(t, textBody, "dockerfile parse error")
	assert.Contains(t, htmlBody, "build-uuid-456")
	assert.Contains(t, htmlBody, "dockerfile parse error")
}

func TestTemplates_AlertQuotaThreshold(t *testing.T) {
	svc, err := email.New(logOnlyCfg())
	require.NoError(t, err)

	vars := map[string]any{
		"ProjectName":      "my-project",
		"QuotaType":        "applications",
		"UsagePercent":     82,
		"ThresholdPercent": 80,
	}

	subject, textBody, htmlBody, err := svc.Render("alert_quota_threshold", vars)
	require.NoError(t, err)

	assert.Equal(t, "Quota threshold reached", subject)
	assert.Contains(t, textBody, "my-project")
	assert.Contains(t, textBody, "applications")
	assert.Contains(t, textBody, "82")
	assert.Contains(t, textBody, "80")
	assert.Contains(t, htmlBody, "my-project")
	assert.Contains(t, htmlBody, "82")
}

func TestTemplates_UnknownTemplate(t *testing.T) {
	svc, err := email.New(logOnlyCfg())
	require.NoError(t, err)
	err = svc.SendTemplate(context.Background(), "nonexistent_template", "user@example.com", nil)
	require.Error(t, err)
	assert.True(t, strings.Contains(err.Error(), "unknown template"), "expected unknown template error, got: %s", err)
}

func TestNew_WarnOnMissingPublicBaseURL(t *testing.T) {
	cfg := &config.Config{
		SMTPHost:      "smtp.example.com",
		SMTPPort:      587,
		SMTPFromEmail: "noreply@example.com",
		SMTPFromName:  "Test",
		SMTPTLSMode:   "starttls",
		PublicBaseURL: "", // missing
	}
	svc, err := email.New(cfg)
	require.NoError(t, err)
	require.NotNil(t, svc)

	// SMTP configured but PUBLIC_BASE_URL missing: sending fails loudly (no
	// crash, but a real error) so the misconfiguration surfaces.
	err = svc.Send(context.Background(), email.Message{
		To:      "user@example.com",
		Subject: "Test",
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "PUBLIC_BASE_URL")
}

func TestNew_WarnOnInvalidPublicBaseURL(t *testing.T) {
	cfg := &config.Config{
		SMTPHost:      "smtp.example.com",
		SMTPPort:      587,
		SMTPFromEmail: "noreply@example.com",
		SMTPFromName:  "Test",
		SMTPTLSMode:   "starttls",
		PublicBaseURL: "not-a-url",
	}
	svc, err := email.New(cfg)
	require.NoError(t, err)
	require.NotNil(t, svc)

	// SMTP is configured but PUBLIC_BASE_URL is invalid: sending must fail loudly
	// rather than silently no-op, so a "send test" can't report a false success.
	err = svc.Send(context.Background(), email.Message{
		To:      "user@example.com",
		Subject: "Test",
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "PUBLIC_BASE_URL")
}
