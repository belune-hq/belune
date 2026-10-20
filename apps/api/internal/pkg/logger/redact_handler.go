// Package logger provides slog helpers for structured, safe log output.
package logger

import (
	"context"
	"log/slog"
	"strings"

	"github.com/belune-hq/belune/internal/pkg/redact"
)

// sensitiveKeyFragments are substrings that, when found (case-insensitively)
// in an attribute key, cause the value to be replaced with [REDACTED].
var sensitiveKeyFragments = []string{
	"password", "passwd", "secret", "token", "credential",
	"apikey", "api_key", "auth", "bearer", "jwt",
	"encryption", "private", "cert", "key",
}

// RedactHandler wraps any slog.Handler and scrubs sensitive data from every
// log record before passing it to the inner handler. It operates on:
//   - Attribute keys that contain a sensitive fragment (value replaced)
//   - String attribute values that match the redact.Error patterns
//   - The log message itself (redact.Error patterns)
//   - Group attributes, recursively
type RedactHandler struct {
	inner slog.Handler
}

// NewRedactHandler wraps inner with credential-scrubbing logic.
func NewRedactHandler(inner slog.Handler) slog.Handler {
	return &RedactHandler{inner: inner}
}

func (h *RedactHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.inner.Enabled(ctx, level)
}

func (h *RedactHandler) Handle(ctx context.Context, r slog.Record) error {
	r2 := slog.NewRecord(r.Time, r.Level, redact.Error(r.Message), r.PC)
	r.Attrs(func(a slog.Attr) bool {
		r2.AddAttrs(redactAttr(a))
		return true
	})
	return h.inner.Handle(ctx, r2)
}

func (h *RedactHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	redacted := make([]slog.Attr, len(attrs))
	for i, a := range attrs {
		redacted[i] = redactAttr(a)
	}
	return &RedactHandler{inner: h.inner.WithAttrs(redacted)}
}

func (h *RedactHandler) WithGroup(name string) slog.Handler {
	return &RedactHandler{inner: h.inner.WithGroup(name)}
}

// redactAttr returns a copy of a with sensitive data replaced.
func redactAttr(a slog.Attr) slog.Attr {
	if isSensitiveKey(a.Key) {
		return slog.String(a.Key, "[REDACTED]")
	}
	switch a.Value.Kind() {
	case slog.KindString:
		return slog.String(a.Key, redact.Error(a.Value.String()))
	case slog.KindGroup:
		children := a.Value.Group()
		out := make([]slog.Attr, len(children))
		for i, child := range children {
			out[i] = redactAttr(child)
		}
		return slog.Attr{Key: a.Key, Value: slog.GroupValue(out...)}
	}
	return a
}

func isSensitiveKey(key string) bool {
	lower := strings.ToLower(key)
	for _, fragment := range sensitiveKeyFragments {
		if strings.Contains(lower, fragment) {
			return true
		}
	}
	return false
}
