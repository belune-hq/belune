package logger_test

import (
	"bytes"
	"log"
	"log/slog"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/belune-hq/belune/internal/pkg/logger"
	"github.com/belune-hq/belune/internal/pkg/loglevel"
)

func newConsole(level slog.Level) (*bytes.Buffer, *slog.Logger) {
	buf := &bytes.Buffer{}
	h := logger.NewConsoleHandler(buf, &slog.HandlerOptions{Level: level})
	return buf, slog.New(h)
}

func TestConsoleHandler_LineShape(t *testing.T) {
	buf, log := newConsole(slog.LevelDebug)
	log.Info("deploy finished")

	line := strings.TrimRight(buf.String(), "\n")
	// "2026-07-22 10:00:04 INFO  [logger.console_handler] deploy finished"
	re := regexp.MustCompile(`^\d{4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2} INFO +\[[a-z0-9._]+\] deploy finished$`)
	assert.Regexp(t, re, line)
	assert.NotContains(t, line, "T", "the timestamp must be 24-hour space-separated, not RFC3339")
}

// The module is derived from the call site, so a log emitted from this test
// file must name this package rather than a generic placeholder.
func TestConsoleHandler_ModuleComesFromCallSite(t *testing.T) {
	buf, log := newConsole(slog.LevelDebug)
	log.Info("hello")

	m := regexp.MustCompile(`\[([^\]]+)\]`).FindStringSubmatch(buf.String())
	require.NotNil(t, m, "no [module] field in %q", buf.String())
	assert.Contains(t, m[1], "logger",
		"module should name the calling package, got %q", m[1])
	assert.NotEqual(t, "app", m[1], "app is the fallback for a missing PC")
}

func TestConsoleHandler_AttrsOnContinuationLine(t *testing.T) {
	buf, log := newConsole(slog.LevelDebug)
	log.Error("reconcile failed", "app_id", "10d7ec5f", "error", "connection refused")

	lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	require.Len(t, lines, 2, "attributes belong on their own line: %q", buf.String())
	assert.Contains(t, lines[0], "ERROR")
	assert.Contains(t, lines[0], "reconcile failed")
	assert.NotContains(t, lines[0], "app_id", "attrs must not crowd the message line")

	assert.True(t, strings.HasPrefix(lines[1], " "), "continuation line must be indented")
	assert.Contains(t, lines[1], "app_id=10d7ec5f")
	// Values with spaces are quoted so key=value stays parseable.
	assert.Contains(t, lines[1], `error="connection refused"`)
}

func TestConsoleHandler_NoAttrsMeansNoSecondLine(t *testing.T) {
	buf, log := newConsole(slog.LevelDebug)
	log.Info("plain")
	assert.Equal(t, 1, strings.Count(buf.String(), "\n"),
		"a record without attributes must be a single line")
}

func TestConsoleHandler_RespectsLevel(t *testing.T) {
	buf, log := newConsole(slog.LevelWarn)
	log.Info("should not appear")
	log.Warn("should appear")
	assert.NotContains(t, buf.String(), "should not appear")
	assert.Contains(t, buf.String(), "should appear")
}

func TestConsoleHandler_GroupsAndWithAttrs(t *testing.T) {
	buf, log := newConsole(slog.LevelDebug)
	log.With("req", "abc").WithGroup("db").Info("query", "rows", 3)

	out := buf.String()
	assert.Contains(t, out, "req=abc")
	assert.Contains(t, out, "db.rows=3", "grouped keys should be dotted")
}

// Two loggers derived from one parent must not see each other's attributes.
// Appending to a shared backing array is the classic way to get this wrong.
func TestConsoleHandler_DerivedLoggersDoNotBleed(t *testing.T) {
	buf, log := newConsole(slog.LevelDebug)
	parent := log.With("base", "1")
	a := parent.With("only_a", "yes")
	b := parent.With("only_b", "yes")

	buf.Reset()
	b.Info("b line")
	assert.NotContains(t, buf.String(), "only_a",
		"attributes from a sibling logger leaked in")

	buf.Reset()
	a.Info("a line")
	assert.NotContains(t, buf.String(), "only_b")
	_ = b
}

func TestConsoleHandler_ConcurrentWritesStayWhole(t *testing.T) {
	buf := &bytes.Buffer{}
	log := slog.New(logger.NewConsoleHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); log.Info("concurrent", "k", "v") }()
	}
	wg.Wait()

	lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	require.Len(t, lines, 100, "50 records x (message + attrs) lines")
	for _, l := range lines {
		if strings.HasPrefix(l, " ") {
			continue // continuation
		}
		assert.Regexp(t, `^\d{4}-\d{2}-\d{2} `, l, "interleaved write corrupted a line")
	}
}

// The whole point of the format change: the platform log viewer must still be
// able to read a level back off the line. Without this the console format is a
// regression — every line, errors included, would render as Info.
func TestConsoleHandler_OutputIsLevelDetectable(t *testing.T) {
	cases := []struct {
		name string
		emit func(*slog.Logger)
		want loglevel.Level
	}{
		{"info", func(l *slog.Logger) { l.Info("started") }, loglevel.Info},
		{"warn", func(l *slog.Logger) { l.Warn("slow") }, loglevel.Warning},
		{"error", func(l *slog.Logger) { l.Error("boom") }, loglevel.Error},
		{"debug", func(l *slog.Logger) { l.Debug("detail") }, loglevel.Debug},
		// The case the tier exists for: an informational line whose text
		// contains a failure word must not be promoted to Error.
		{"info mentioning failure",
			func(l *slog.Logger) { l.Info("health check failed, retrying") },
			loglevel.Info},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			buf, log := newConsole(slog.LevelDebug)
			tc.emit(log)
			line := strings.SplitN(strings.TrimRight(buf.String(), "\n"), "\n", 2)[0]
			assert.Equal(t, tc.want, loglevel.Detect(line, "stdout"),
				"line was %q", line)
		})
	}
}

func TestConsoleHandler_ColorOffByDefaultForNonTerminal(t *testing.T) {
	// A bytes.Buffer is not a terminal, which is the same situation as Docker
	// capturing the container's stdout through a pipe.
	buf, log := newConsole(slog.LevelDebug)
	log.Error("boom", "k", "v")
	assert.NotContains(t, buf.String(), "\x1b[",
		"escape codes must never reach a captured stream")
}

func TestConsoleHandler_ColorLayout(t *testing.T) {
	buf := &bytes.Buffer{}
	log := slog.New(logger.NewConsoleHandlerWithColor(
		buf, &slog.HandlerOptions{Level: slog.LevelDebug}, true))
	log.Error("reconcile failed", "k", "v")

	out := buf.String()
	assert.Contains(t, out, "\x1b[37m", "timestamp should be white")
	assert.Contains(t, out, "\x1b[95m[", "module should be violet")
	assert.Contains(t, out, "\x1b[31m", "an error line should be red")

	// Every opened colour is closed, or the terminal bleeds into later output.
	assert.Equal(t, strings.Count(out, "\x1b[0m"),
		strings.Count(out, "\x1b[")-strings.Count(out, "\x1b[0m"),
		"unbalanced colour start/reset pairs in %q", out)
}

func TestConsoleHandler_ColorPerLevel(t *testing.T) {
	for _, tc := range []struct {
		name string
		emit func(*slog.Logger)
		want string
	}{
		{"debug", func(l *slog.Logger) { l.Debug("x") }, "\x1b[36m"},
		{"info", func(l *slog.Logger) { l.Info("x") }, "\x1b[32m"},
		{"warn", func(l *slog.Logger) { l.Warn("x") }, "\x1b[33m"},
		{"error", func(l *slog.Logger) { l.Error("x") }, "\x1b[31m"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			buf := &bytes.Buffer{}
			l := slog.New(logger.NewConsoleHandlerWithColor(
				buf, &slog.HandlerOptions{Level: slog.LevelDebug}, true))
			tc.emit(l)
			assert.Contains(t, buf.String(), tc.want)
		})
	}
}

// Colour must not cost us the level: a coloured line still has to be readable
// by the viewer, in case anyone forces LOG_COLOR=always somewhere captured.
func TestConsoleHandler_ColoredLineStillLevelDetectable(t *testing.T) {
	buf := &bytes.Buffer{}
	log := slog.New(logger.NewConsoleHandlerWithColor(
		buf, &slog.HandlerOptions{Level: slog.LevelDebug}, true))
	log.Info("health check failed, retrying")

	line := strings.SplitN(strings.TrimRight(buf.String(), "\n"), "\n", 2)[0]
	assert.Equal(t, loglevel.Info, loglevel.Detect(line, "stdout"),
		"coloured line lost its level: %q", line)
}

// The continuation line is identified by leading whitespace, so its indent must
// not be preceded by an escape sequence.
func TestConsoleHandler_ColoredContinuationStaysIndented(t *testing.T) {
	buf := &bytes.Buffer{}
	log := slog.New(logger.NewConsoleHandlerWithColor(
		buf, &slog.HandlerOptions{Level: slog.LevelDebug}, true))
	log.Error("boom", "app_id", "abc")

	lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	require.Len(t, lines, 2)
	assert.True(t, strings.HasPrefix(lines[1], " "),
		"continuation must start with whitespace, got %q", lines[1])
}

// slog.SetDefault also routes the standard log package through this handler,
// and those records carry no program counter. They must not be labelled "app",
// which is a real package: "[app] traces export: ... no such host" reads as
// though Belune's own startup code raised it, when it is OpenTelemetry's
// exporter logging via log.Printf.
func TestConsoleHandler_StdlogBridgeIsNotMistakenForAppPackage(t *testing.T) {
	buf := &bytes.Buffer{}
	prev := slog.Default()
	t.Cleanup(func() { slog.SetDefault(prev) })
	slog.SetDefault(slog.New(logger.NewConsoleHandlerWithColor(
		buf, &slog.HandlerOptions{Level: slog.LevelDebug}, false)))

	log.Printf("traces export: no such host")

	out := buf.String()
	assert.Contains(t, out, "[stdlog]",
		"records with no call site should be attributed to the stdlib bridge")
	assert.NotContains(t, out, "[app]",
		"the fallback must not collide with the internal/app package")
}
