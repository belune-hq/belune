package worker

import (
	"fmt"
	"log/slog"

	"github.com/belune-hq/belune/internal/pkg/logger"
)

// asynqLogger adapts asynq's logging interface onto slog.
//
// Without it asynq writes its own format straight to stderr:
//
//	asynq: pid=234659 2026/07/22 02:58:13.003782 INFO: Scheduler starting
//
// which carries its own timestamp and level and so sits outside every level
// filter, colour and module label the rest of the process uses — and, because
// the platform log viewer reads severity off the line, arrives there as plain
// Info regardless of what asynq actually meant.
//
// asynq's levels map straight across. Fatal has no slog equivalent; it is
// logged at Error rather than exiting, because asynq calls it for conditions
// the server itself then handles.
type asynqLogger struct{}

func (asynqLogger) Debug(args ...any) { slog.Debug(fmt.Sprint(args...)) }
func (asynqLogger) Info(args ...any)  { slog.Info(fmt.Sprint(args...)) }
func (asynqLogger) Warn(args ...any)  { slog.Warn(fmt.Sprint(args...)) }

// Error downgrades transient Redis-connectivity failures to Warn. asynq logs at
// Error every time it cannot reach Redis, which on a full-stack restart or host
// reboot is a normal few-second window (see logger.IsTransientConnError): the
// worker's first dequeues and janitor sweeps fail with "connection refused" /
// "no such host" until redis is up, then recover on retry. Logging those at Error
// makes a healthy boot look broken. A genuine, sustained outage still surfaces —
// as a stream of Warns rather than silence — and every other error stays Error.
func (asynqLogger) Error(args ...any) {
	msg := fmt.Sprint(args...)
	if logger.IsTransientConnError(msg) {
		slog.Warn(msg)
		return
	}
	slog.Error(msg)
}

func (asynqLogger) Fatal(args ...any) { slog.Error(fmt.Sprint(args...)) }
