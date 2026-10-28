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
	var extra []netip.Prefix
	for _, c := range cfg.ProxyPeerCIDRs {
		p, err := netip.ParsePrefix(c)
		if err != nil {
			// A bare address is a /32 or /128; anything else is a typo, and an
			// ignored entry fails safe (less trusted), so say so and move on.
			a, aerr := netip.ParseAddr(c)
			if aerr != nil {
				slog.Warn("client ip: ignoring PROXY_PEER_CIDRS entry that is not an address or CIDR", "entry", c)
				continue
			}
			p = netip.PrefixFrom(a, a.BitLen())
		}
		extra = append(extra, p.Masked())
	}

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
