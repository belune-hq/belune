package tracing

import (
	"bytes"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/belune-hq/belune/internal/pkg/logger"
	"github.com/belune-hq/belune/internal/pkg/loglevel"
)

func captureLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	buf := &bytes.Buffer{}
	prev := slog.Default()
	t.Cleanup(func() { slog.SetDefault(prev) })
	slog.SetDefault(slog.New(logger.NewConsoleHandlerWithColor(
		buf, &slog.HandlerOptions{Level: slog.LevelDebug}, false)))
	return buf
}

// The reason this exists: OTel's errors otherwise reach the standard log
// package, which slog bridges at Info, so an unreachable collector reports an
// export failure as information.
func TestThrottledHandler_LogsAtErrorLevel(t *testing.T) {
	buf := captureLogs(t)
	newThrottledHandler(time.Minute, time.Now).Handle(errors.New("traces export: no such host"))

	line := strings.SplitN(strings.TrimRight(buf.String(), "\n"), "\n", 2)[0]
	assert.Equal(t, loglevel.Error, loglevel.Detect(line, "stderr"),
		"an export failure must not be reported as Info: %q", line)
	assert.Contains(t, buf.String(), "no such host")
}

func TestThrottledHandler_FirstOccurrenceIsImmediate(t *testing.T) {
	buf := captureLogs(t)
	now := time.Now()
	h := newThrottledHandler(time.Minute, func() time.Time { return now })
	h.Handle(errors.New("boom"))
	assert.Contains(t, buf.String(), "boom",
		"the first occurrence must never be held back")
}

// An exporter pointed at a collector that is not there fails every ~10s, about
// 8,600 times a day. Unthrottled that would bury every other error.
func TestThrottledHandler_SuppressesRepeatsWithinWindow(t *testing.T) {
	buf := captureLogs(t)
	now := time.Now()
	h := newThrottledHandler(time.Minute, func() time.Time { return now })

	for i := 0; i < 50; i++ {
		h.Handle(errors.New("traces export: no such host"))
		now = now.Add(10 * time.Second) // stays inside the window for the first 6
	}

	got := strings.Count(buf.String(), "traces export: no such host")
	assert.Less(t, got, 15, "50 identical failures should not produce 50 lines")
	assert.Greater(t, got, 1, "the error should still be reported periodically")
}

func TestThrottledHandler_ReportsSuppressedCount(t *testing.T) {
	buf := captureLogs(t)
	now := time.Now()
	h := newThrottledHandler(time.Minute, func() time.Time { return now })

	h.Handle(errors.New("same"))
	for i := 0; i < 5; i++ {
		now = now.Add(time.Second)
		h.Handle(errors.New("same"))
	}
	now = now.Add(2 * time.Minute)
	h.Handle(errors.New("same"))

	assert.Contains(t, buf.String(), "repeated=5",
		"suppressed occurrences must be accounted for, not dropped")
}

// Suppressing a *different* error would hide a new failure behind an old one.
func TestThrottledHandler_DifferentErrorIsAlwaysReported(t *testing.T) {
	buf := captureLogs(t)
	now := time.Now()
	h := newThrottledHandler(time.Minute, func() time.Time { return now })

	h.Handle(errors.New("first failure"))
	now = now.Add(time.Second)
	h.Handle(errors.New("second, different failure"))

	assert.Contains(t, buf.String(), "second, different failure")
}

// Switching errors must not silently discard the run held back for the previous
// one — otherwise the count of what actually happened is lost.
func TestThrottledHandler_FlushesPendingCountOnChange(t *testing.T) {
	buf := captureLogs(t)
	now := time.Now()
	h := newThrottledHandler(time.Minute, func() time.Time { return now })

	h.Handle(errors.New("aaa"))
	for i := 0; i < 3; i++ {
		now = now.Add(time.Second)
		h.Handle(errors.New("aaa"))
	}
	h.Handle(errors.New("bbb"))

	assert.Contains(t, buf.String(), "repeated=3",
		"the suppressed run for the previous error should be flushed")
	assert.Contains(t, buf.String(), "bbb")
}

func TestThrottledHandler_NilErrorIsIgnored(t *testing.T) {
	buf := captureLogs(t)
	newThrottledHandler(time.Minute, time.Now).Handle(nil)
	assert.Empty(t, buf.String())
}

// The detail belongs on the message line, not tucked into an attribute of a
// generic message: which endpoint failed and how is the whole content of the
// line, and "otel error" beside an ERROR level says nothing.
func TestThrottledHandler_MessageCarriesTheDetail(t *testing.T) {
	buf := captureLogs(t)
	newThrottledHandler(time.Minute, time.Now).Handle(
		errors.New(`traces export: Post "http://jaeger:4318/v1/traces": no such host`))

	first := strings.SplitN(strings.TrimRight(buf.String(), "\n"), "\n", 2)[0]
	assert.Contains(t, first, "no such host",
		"the failure should be readable without expanding attributes: %q", first)
	assert.NotContains(t, first, "otel error",
		"a placeholder message adds a word and no information")
}
