package logger_test

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/belune-hq/belune/internal/pkg/logger"
)

// captureHandler records the last handled record's output as JSON text.
type captureHandler struct {
	buf bytes.Buffer
}

func newCapture() (*captureHandler, slog.Handler) {
	c := &captureHandler{}
	return c, slog.NewTextHandler(&c.buf, &slog.HandlerOptions{Level: slog.LevelDebug})
}

func handlerWithCapture() (*captureHandler, slog.Handler) {
	c, inner := newCapture()
	return c, logger.NewRedactHandler(inner)
}

func TestRedactHandler_SensitiveKey(t *testing.T) {
	c, h := handlerWithCapture()
	l := slog.New(h)
	l.Info("test", "password", "supersecret")

	out := c.buf.String()
	if strings.Contains(out, "supersecret") {
		t.Errorf("sensitive value not redacted; got: %s", out)
	}
	if !strings.Contains(out, "[REDACTED]") {
		t.Errorf("expected [REDACTED] in output; got: %s", out)
	}
}

func TestRedactHandler_TokenKey(t *testing.T) {
	c, h := handlerWithCapture()
	l := slog.New(h)
	l.Info("deploy", "git_token", "ghp_abc123")

	out := c.buf.String()
	if strings.Contains(out, "ghp_abc123") {
		t.Errorf("token not redacted; got: %s", out)
	}
}

func TestRedactHandler_CredentialPattern_InMessage(t *testing.T) {
	c, h := handlerWithCapture()
	l := slog.New(h)
	l.Error("clone failed", "error", "https://mytoken@github.com/org/repo: 403")

	out := c.buf.String()
	if strings.Contains(out, "mytoken") {
		t.Errorf("credential in message not redacted; got: %s", out)
	}
}

func TestRedactHandler_NonSensitiveValue_Unchanged(t *testing.T) {
	c, h := handlerWithCapture()
	l := slog.New(h)
	l.Info("deploy", "application_id", "abc-123", "status", "running")

	out := c.buf.String()
	if !strings.Contains(out, "abc-123") {
		t.Errorf("non-sensitive value was unexpectedly redacted; got: %s", out)
	}
	if !strings.Contains(out, "running") {
		t.Errorf("non-sensitive value was unexpectedly redacted; got: %s", out)
	}
}

func TestRedactHandler_GroupAttrs(t *testing.T) {
	c, h := handlerWithCapture()
	l := slog.New(h)
	l.LogAttrs(context.Background(), slog.LevelInfo, "group test",
		slog.Group("creds",
			slog.String("username", "alice"),
			slog.String("password", "hunter2"),
		),
	)

	out := c.buf.String()
	if strings.Contains(out, "hunter2") {
		t.Errorf("password inside group not redacted; got: %s", out)
	}
	if !strings.Contains(out, "alice") {
		t.Errorf("non-sensitive group value unexpectedly redacted; got: %s", out)
	}
}

func TestRedactHandler_Enabled_Delegated(t *testing.T) {
	_, h := handlerWithCapture()
	// The inner text handler is set to LevelDebug, so all levels should be enabled.
	if !h.Enabled(context.Background(), slog.LevelDebug) {
		t.Error("Enabled should delegate to inner handler")
	}
}
