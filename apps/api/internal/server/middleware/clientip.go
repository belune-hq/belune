package middleware

import (
	"context"
	"net"
	"net/http"
	"net/netip"
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

// ClientIP is the address of the client that made this request, as a canonical
// IP string, or "" when there is none to give.
//
// It reads r.RemoteAddr and nothing else — never a header. ProxyTrust.Middleware
// is what puts the real visitor there when, and only when, the request came from
// the Caddy container; on every other path RemoteAddr is the TCP peer, which can
// be wrong about who the visitor was but cannot be chosen by them. Empty rather
// than a raw string for an unparseable value: a caller stores it in a column, and
// a value the column cannot hold costs the whole row.
func ClientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	ip, ok := ParseClientIP(host)
	if !ok {
		return ""
	}
	return ip.String()
}

// ParseClientIP accepts a plain IPv4 or IPv6 address and nothing else: no port,
// no zone, no list, no whitespace. The result is unmapped, so ::ffff:1.2.3.4 and
// 1.2.3.4 are one client, and it never exceeds 39 bytes.
func ParseClientIP(s string) (netip.Addr, bool) {
	ip, err := netip.ParseAddr(s)
	if err != nil || ip.Zone() != "" || ip.IsUnspecified() {
		return netip.Addr{}, false
	}
	return ip.Unmap(), true
}
