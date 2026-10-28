package middleware

import (
	"bufio"
	"context"
	"encoding/binary"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/belune-hq/belune/internal/pkg/metrics"
	"github.com/belune-hq/belune/internal/proxy"
)

// PeerClass is what the TCP peer of a request turned out to be.
//
// It has four values rather than three because "Belune could not ask" must stay
// distinct from "it is not there": the same rule the capability probe follows with
// available / unavailable / unknown. A Docker hiccup while Caddy restarts must not
// turn into a 403 on the whole dashboard.
type PeerClass int

const (
	// PeerOther is known not to be the proxy, the host, or loopback: a container
	// on a project network, or anything else that reached the API port directly.
	PeerOther PeerClass = iota
	// PeerProxy is the Caddy container. Only this peer's X-Belune-Client-IP is
	// believed.
	PeerProxy
	// PeerHost is loopback or the Docker gateway: the host itself, which is how the
	// published 127.0.0.1:8080 reaches the container. The installer, the updater's
	// health probe and the smoke drills arrive this way.
	PeerHost
	// PeerUnknown is a peer that matches nothing while Caddy's addresses could not
	// be determined. Served, and its header not believed.
	PeerUnknown
)

const (
	// proxyTTL is how long Caddy's addresses are believed before they are asked for
	// again. It bounds how long a released address can keep its trust.
	proxyTTL = 30 * time.Second
	// proxyRetry spaces lookups, so a peer that is not Caddy cannot make every
	// request cost a Docker call.
	proxyRetry   = 2 * time.Second
	proxyTimeout = 2 * time.Second
)

// ProxyLister returns the addresses the proxy connects from. An empty result with
// no error means the proxy was not found, which is not the same as having none.
type ProxyLister func(ctx context.Context) ([]netip.Addr, error)

// ProxyTrust decides whose word to take about who the client is.
//
// Two concerns, deliberately given different defaults when Caddy's addresses are
// not known:
//
//   - ATTRIBUTION: a peer that is the proxy is believed about the client, anyone
//     else is attributed by its own address. That is safe whether or not discovery
//     worked, because the peer address is always truthful. It never depends on a
//     lookup succeeding to be correct.
//   - ADMISSION: refusing a peer that is known not to be the proxy, the host or
//     loopback is hardening on top. It only refuses what it knows; while the
//     proxy's addresses are unknown it serves, because 403-ing the dashboard over
//     a failed Docker call would be a worse outage than the thing it prevents.
//
// ⚠️ Residual risk, accepted: containers on one Docker bridge share a layer-2
// segment, so a container with raw-socket code execution could ARP-spoof Caddy's
// address and be believed. The threat this closes is a deployed app, or an SSRF in
// one, opening an HTTP connection to the API — an HTTP primitive cannot forge its
// source address. A shared secret would also cover container RCE, at the cost of a
// secret lifecycle and a host update; revisit if that enters the threat model.
type ProxyTrust struct {
	list    ProxyLister
	extra   []netip.Prefix
	gateway netip.Addr
	now     func() time.Time

	refreshMu sync.Mutex // held while asking; a concurrent caller waits for the answer
	lastTry   time.Time  // under refreshMu

	mu    sync.RWMutex
	addrs map[netip.Addr]struct{}
	known bool
	// verified is whether the LAST lookup succeeded. A miss against a set that
	// could not be refreshed proves nothing about the peer, so it is not a refusal.
	verified bool
	lastOK   time.Time
}

// NewProxyTrust builds the decision. extra are always-trusted peers from
// configuration; gateway is the container's default-route gateway, or the zero
// Addr when there is none.
func NewProxyTrust(list ProxyLister, extra []netip.Prefix, gateway netip.Addr) *ProxyTrust {
	return &ProxyTrust{list: list, extra: extra, gateway: gateway, now: time.Now}
}

// Classify says what a peer is, asking for the proxy's addresses again when the
// ones held are stale or do not include it.
func (p *ProxyTrust) Classify(ctx context.Context, peer netip.Addr) PeerClass {
	peer = peer.Unmap()
	// Configured peers come first, so an operator can name the gateway or loopback
	// as the proxy: that is the dev stack, where the API runs on the host and
	// Caddy's connections arrive from the Docker bridge.
	for _, pf := range p.extra {
		if pf.Contains(peer) {
			return PeerProxy
		}
	}
	if peer.IsLoopback() || (p.gateway.IsValid() && peer == p.gateway) {
		return PeerHost
	}

	in, age := p.lookup(peer)
	if in && age > proxyTTL {
		// Believed on the strength of an answer that has gone stale: ask again
		// before honouring it, so a released address does not keep its trust.
		p.refresh(ctx)
		in, _ = p.lookup(peer)
	} else if !in {
		// Not in the set held. It may be a restarted Caddy on a new address, so ask
		// before refusing anyone.
		p.refresh(ctx)
		in, _ = p.lookup(peer)
	}
	if in {
		return PeerProxy
	}
	return p.classifyMiss()
}

// classifyMiss decides what a peer that is not in the held set is. It is "other"
// — refusable — only when the set rules it out on a recent, successful lookup.
// A set that is missing, could not be refreshed, or has aged past twice its TTL
// says nothing about this peer: Caddy may have moved while Docker was away, and
// refusing then would 403 the dashboard for a lookup that never happened.
func (p *ProxyTrust) classifyMiss() PeerClass {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if !p.known || !p.verified || p.now().Sub(p.lastOK) > 2*proxyTTL {
		return PeerUnknown
	}
	return PeerOther
}

func (p *ProxyTrust) lookup(peer netip.Addr) (in bool, age time.Duration) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	_, in = p.addrs[peer]
	return in, p.now().Sub(p.lastOK)
}

// refresh asks for the proxy's addresses. At most one caller asks at a time and at
// most once per proxyRetry; a caller that arrives while another is asking WAITS
// for that answer rather than carrying on with the set it had. Returning at once
// with the stale set is how a burst of parallel requests after Caddy moves would
// all miss and be refused, while the one that won the lookup got through.
//
// A failed or empty answer replaces nothing it cannot replace: a transient error
// keeps the last good set (marked unverified), and an answer of "no proxy" makes
// the state unknown, not empty.
func (p *ProxyTrust) refresh(ctx context.Context) {
	if p.list == nil {
		return
	}
	p.refreshMu.Lock()
	defer p.refreshMu.Unlock()
	if p.now().Sub(p.lastTry) < proxyRetry {
		return
	}
	p.lastTry = p.now()

	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), proxyTimeout)
	defer cancel()
	found, err := p.list(ctx)
	p.mu.Lock()
	defer p.mu.Unlock()
	if err != nil {
		slog.Warn("client ip: could not list the proxy's addresses", "error", err)
		p.verified = false
		return
	}
	p.verified = true
	if len(found) == 0 {
		p.addrs, p.known = nil, false
		return
	}
	set := make(map[netip.Addr]struct{}, len(found))
	for _, a := range found {
		set[a.Unmap()] = struct{}{}
	}
	p.addrs, p.known, p.lastOK = set, true, p.now()
}

// Middleware resolves the client address once, up front, and leaves it in
// r.RemoteAddr — which is what httprate.KeyByIP and every ClientIP caller read, so
// none of them has to know any of this exists.
//
// The header is removed from every request, so nothing downstream can read a value
// this decision did not vouch for.
func (p *ProxyTrust) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hdr := r.Header.Get(proxy.ClientIPHeader)
		r.Header.Del(proxy.ClientIPHeader)

		host, port, err := net.SplitHostPort(r.RemoteAddr)
		peer, perr := netip.ParseAddr(host)
		if err != nil || perr != nil {
			metrics.RecordClientIPDecision("direct")
			next.ServeHTTP(w, r)
			return
		}

		switch p.Classify(r.Context(), peer) {
		case PeerProxy:
			switch ip, ok := ParseClientIP(hdr); {
			case ok:
				r.RemoteAddr = net.JoinHostPort(ip.String(), port)
				metrics.RecordClientIPDecision("proxy")
			case hdr == "":
				// A dashboard route that does not set the header: attributed to the
				// proxy, which is wrong but truthful, and counted so it is found.
				metrics.RecordClientIPDecision("proxy_no_header")
			default:
				metrics.RecordClientIPDecision("proxy_bad_header")
			}
		case PeerOther:
			if r.URL.Path != "/healthz" {
				metrics.RecordClientIPDecision("rejected")
				slog.Warn("client ip: refused a request that did not come through the proxy",
					"peer", peer.String(), "path", r.URL.Path)
				// Not http.Error: it would label this JSON body text/plain. The handler
				// package's writeError is out of reach (it imports this package).
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusForbidden)
				_, _ = io.WriteString(w, `{"error":"forbidden"}`+"\n")
				return
			}
			metrics.RecordClientIPDecision("direct")
		case PeerHost:
			metrics.RecordClientIPDecision("host")
		default:
			metrics.RecordClientIPDecision("direct")
		}
		next.ServeHTTP(w, r)
	})
}

// DefaultGateway reads the container's default-route gateway from a Linux
// /proc/net/route. A connection to the published port of a container arrives
// from the bridge gateway, not from loopback, so the host is recognised by it.
// The zero Addr is returned when there is no default route or it cannot be read.
func DefaultGateway(route io.Reader) netip.Addr {
	sc := bufio.NewScanner(route)
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		// Iface Destination Gateway Flags ...; the default route is destination 0.
		if len(f) < 4 || f[1] != "00000000" {
			continue
		}
		flags, err := strconv.ParseUint(f[3], 16, 32)
		if err != nil || flags&0x2 == 0 { // RTF_GATEWAY
			continue
		}
		gw, err := strconv.ParseUint(f[2], 16, 32)
		if err != nil || gw == 0 {
			continue
		}
		// /proc/net/route stores the address in host byte order, little-endian.
		var b [4]byte
		binary.LittleEndian.PutUint32(b[:], uint32(gw))
		return netip.AddrFrom4(b)
	}
	return netip.Addr{}
}
