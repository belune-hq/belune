package app

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/belune-hq/belune/internal/pkg/logger"
)

// redisSlogLogger adapts go-redis's Logging interface onto slog. go-redis writes
// its internal logs straight to stderr by default — with their own timestamp and
// no level — so they sit outside every level filter, colour, and module label the
// rest of the process uses. Routing them through slog puts them back in line, and
// transient connectivity churn (a redis restart's "discarding bad PubSub
// connection: EOF") drops to Debug so a healthy boot stays quiet; anything else
// go-redis logs comes through at Warn (it is never fatal — the client retries).
type redisSlogLogger struct{}

func (redisSlogLogger) Printf(_ context.Context, format string, v ...any) {
	msg := fmt.Sprintf(format, v...)
	if logger.IsTransientConnError(msg) {
		slog.Debug(msg)
		return
	}
	slog.Warn(msg)
}
