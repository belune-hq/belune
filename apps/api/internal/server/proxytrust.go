package server

import (
	"context"
	"fmt"
	"log/slog"
	"net/netip"
	"os"

	"github.com/belune-hq/belune/internal/config"
	"github.com/belune-hq/belune/internal/runtime"
	"github.com/belune-hq/belune/internal/server/middleware"
)

// caddySystemLabel is the belune-system label value on the Caddy container, which
// is how Belune finds it without depending on a name Compose derives from the
// project directory.
const caddySystemLabel = "caddy"

// newProxyTrust decides whose word to take about the client address, from what
// Docker says Caddy's addresses are.
func newProxyTrust(cfg *config.Config, rts runtime.Runtimes) *middleware.ProxyTrust {
	extra := parsePeerCIDRs(cfg.ProxyPeerCIDRs)

	var gateway netip.Addr
	if f, err := os.Open("/proc/net/route"); err == nil {
		gateway = middleware.DefaultGateway(f)
		_ = f.Close()
	}

	list := func(ctx context.Context) ([]netip.Addr, error) {
		rt, err := rts.Local(ctx)
		if err != nil {
			return nil, fmt.Errorf("local runtime: %w", err)
		}
		containers, err := rt.ListSystemContainers(ctx)
		if err != nil {
			return nil, err
		}
		var addrs []netip.Addr
		for _, c := range containers {
			if c.Labels["belune-system"] != caddySystemLabel || c.Status != "running" {
				continue
			}
			for _, s := range c.Addrs {
				if a, err := netip.ParseAddr(s); err == nil {
					addrs = append(addrs, a)
				}
			}
		}
		return addrs, nil
	}
	return middleware.NewProxyTrust(list, extra, gateway)
}

// minPeerBits is how short a PROXY_PEER_CIDRS prefix may be. Every address in an
// entry may claim any client IP, so this is the one knob that can undo the whole
// change: 0.0.0.0/0 would make every caller's header believed. /8 still admits a
// private network such as 10.0.0.0/8; nothing a proxy sits on is wider.
const (
	minPeerBitsV4 = 8
	minPeerBitsV6 = 16
)

// parsePeerCIDRs reads PROXY_PEER_CIDRS. An entry that is not an address or CIDR,
// or is wider than the bound, is dropped with a warning: ignoring one fails safe
// (less is trusted), and a refusal to start over a typo would be worse.
func parsePeerCIDRs(entries []string) []netip.Prefix {
	var out []netip.Prefix
	for _, c := range entries {
		p, err := netip.ParsePrefix(c)
		if err != nil {
			// A bare address is a /32 or /128.
			a, aerr := netip.ParseAddr(c)
			if aerr != nil {
				slog.Warn("client ip: ignoring PROXY_PEER_CIDRS entry that is not an address or CIDR", "entry", c)
				continue
			}
			p = netip.PrefixFrom(a, a.BitLen())
		}
		p = netip.PrefixFrom(p.Addr().Unmap(), p.Bits()).Masked()
		min := minPeerBitsV6
		if p.Addr().Is4() {
			min = minPeerBitsV4
		}
		if p.Bits() < min {
			slog.Warn("client ip: ignoring PROXY_PEER_CIDRS entry that is too wide; every address in it could claim any client IP",
				"entry", c, "min_prefix_bits", min)
			continue
		}
		out = append(out, p)
	}
	return out
}
