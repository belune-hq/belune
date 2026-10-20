package worker

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/belune-hq/belune/internal/pkg/logger"
	"github.com/belune-hq/belune/internal/pkg/loglevel"
)

// asynq's own logger writes "asynq: pid=... INFO: Scheduler starting" straight
// to stderr, outside our format and outside the viewer's level detection.
// Routing it through slog is what keeps one format for the whole process.
func TestAsynqLoggerRoutesThroughSlog(t *testing.T) {
	buf := &bytes.Buffer{}
	prev := slog.Default()
	t.Cleanup(func() { slog.SetDefault(prev) })
	slog.SetDefault(slog.New(logger.NewConsoleHandlerWithColor(
		buf, &slog.HandlerOptions{Level: slog.LevelDebug}, false)))

	var l asynqLogger
	l.Info("Scheduler starting")

	line := strings.TrimRight(buf.String(), "\n")
	assert.Contains(t, line, "Scheduler starting")
	assert.NotContains(t, line, "asynq: pid=",
		"asynq's own prefix means it bypassed our handler")
	assert.Equal(t, loglevel.Info, loglevel.Detect(line, "stderr"),
		"a routed line must still be level-detectable: %q", line)
}

func TestAsynqLoggerLevelMapping(t *testing.T) {
	for _, tc := range []struct {
		name string
		emit func(asynqLogger)
		want loglevel.Level
	}{
		{"debug", func(l asynqLogger) { l.Debug("d") }, loglevel.Debug},
		{"info", func(l asynqLogger) { l.Info("i") }, loglevel.Info},
		{"warn", func(l asynqLogger) { l.Warn("w") }, loglevel.Warning},
		{"error", func(l asynqLogger) { l.Error("e") }, loglevel.Error},
		// Fatal has no slog level; it must not be silently downgraded to Info.
		{"fatal", func(l asynqLogger) { l.Fatal("f") }, loglevel.Error},
	} {
		t.Run(tc.name, func(t *testing.T) {
			buf := &bytes.Buffer{}
			prev := slog.Default()
			t.Cleanup(func() { slog.SetDefault(prev) })
			slog.SetDefault(slog.New(logger.NewConsoleHandlerWithColor(
				buf, &slog.HandlerOptions{Level: slog.LevelDebug}, false)))

			tc.emit(asynqLogger{})
			line := strings.TrimRight(buf.String(), "\n")
			assert.Equal(t, tc.want, loglevel.Detect(line, "stderr"), "line %q", line)
		})
	}
}
