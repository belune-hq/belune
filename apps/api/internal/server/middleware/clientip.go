package middleware

import (
	"context"
	"net"
	"net/http"
	"strings"
)

type clientIPKey struct{}

// ContextWithClientIP carries the resolved client IP on the context for code
// that has no *http.Request — the MCP tools, which still must attribute an
// audit row to an address.
func ContextWithClientIP(ctx context.Context, ip string) context.Context {
	return context.WithValue(ctx, clientIPKey{}, ip)
}

// ClientIPFromContext returns the IP stored by ContextWithClientIP, or "".
func ClientIPFromContext(ctx context.Context) string {
	v, _ := ctx.Value(clientIPKey{}).(string)
	return v
}

// ClientIP extracts the real client IP from a request. It reads the leftmost
// value from X-Forwarded-For (populated by Caddy, which is the trusted proxy)
// and falls back to r.RemoteAddr when the header is absent.
func ClientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		// XFF is comma-separated; the leftmost entry is the originating client.
		if ip := strings.TrimSpace(strings.SplitN(xff, ",", 2)[0]); ip != "" {
			return ip
		}
	}
	// Strip the port component from RemoteAddr (format is "host:port" or "[host]:port").
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	return r.RemoteAddr
}
