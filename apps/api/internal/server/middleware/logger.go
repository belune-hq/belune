package middleware

import (
	"bufio"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"time"

	chiMiddleware "github.com/go-chi/chi/v5/middleware"

	"github.com/belune-hq/belune/internal/pkg/redact"
)

// Logger logs each HTTP request with method, path, status, duration, and request ID.
// It relies on Chi's RequestID middleware having run first.
//
// The request itself is the message rather than a set of attributes under the
// word "request". Every line said the same thing — a log viewer showed fourteen
// consecutive rows reading "request", with the only content that differed pushed
// onto a continuation line. The message is the column a reader scans, so it
// should carry what happened.
//
// status and duration_ms stay as attributes as well as appearing in the message.
// They are the two fields worth filtering or alerting on, and unlike deployed
// applications — whose traffic the Caddy access-log tailer writes to
// request_logs — Belune's own API requests are recorded nowhere else, so
// LOG_FORMAT=json has to remain useful for them.
func Logger(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		ww := &responseWriter{ResponseWriter: w, statusCode: http.StatusOK}

		next.ServeHTTP(ww, r)

		elapsed := time.Since(start)
		// Redacted: some routes carry a credential in the path itself, and this
		// line is shipped wherever stdout goes.
		path := redact.Path(r.URL.Path)
		msg := fmt.Sprintf("%s %s %d %s", r.Method, path, ww.statusCode, formatLatency(elapsed))

		attrs := []any{
			"status", ww.statusCode,
			"duration_ms", elapsed.Milliseconds(),
			"request_id", chiMiddleware.GetReqID(r.Context()),
		}

		// A 500 is not information. Logging every response at Info put server
		// errors below the level filters meant to surface them — the same way
		// stderr-means-error once mislabelled healthy output.
		if ww.statusCode >= http.StatusInternalServerError {
			slog.Error(msg, attrs...)
			return
		}
		slog.Info(msg, attrs...)
	})
}

// formatLatency renders a duration at about two significant figures.
// time.Duration.String() prints full nanosecond precision — "1.000054247s",
// "926.143242ms" — which is a dozen characters of noise on a line whose job is
// to be skimmed, and precision no HTTP timing justifies.
func formatLatency(d time.Duration) string {
	switch {
	case d >= time.Second:
		return fmt.Sprintf("%.1fs", d.Seconds())
	case d >= time.Millisecond:
		return fmt.Sprintf("%.0fms", float64(d)/float64(time.Millisecond))
	case d >= time.Microsecond:
		return fmt.Sprintf("%.0fµs", float64(d)/float64(time.Microsecond))
	default:
		return d.String()
	}
}

type responseWriter struct {
	http.ResponseWriter
	statusCode int
}

func (rw *responseWriter) WriteHeader(code int) {
	rw.statusCode = code
	rw.ResponseWriter.WriteHeader(code)
}

func (rw *responseWriter) Flush() {
	if f, ok := rw.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Hijack lets the WebSocket handler take over the connection. Without this
// method the wrapped ResponseWriter doesn't satisfy http.Hijacker and the
// WS upgrade fails.
func (rw *responseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	h, ok := rw.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, fmt.Errorf("underlying ResponseWriter does not implement http.Hijacker")
	}
	return h.Hijack()
}
