package proxy

import (
	"encoding/json"
	"fmt"
	"net/netip"
	"sort"
	"strings"
)

// SettingTrustedProxies holds the proxies that sit in front of Caddy (a CDN or a
// load balancer) as a JSON array of CIDRs. Empty or absent means none, which is
// the default and the only safe one: a listed peer can claim ANY client address,
// so naming a proxy you do not control lets anyone choose theirs.
const SettingTrustedProxies = "trusted_proxy_cidrs"

const (
	// MaxTrustedProxies bounds the list. The largest published CDN set is 22
	// ranges; a list near this is a mistake, and Caddy matches it linearly.
	MaxTrustedProxies = 64
	// MinTrustedBitsV4 and V6 are how short a prefix may be. 0.0.0.0/0 would make
	// every caller's address believed, which undoes the attribution entirely; /8
	// still admits a private network such as 10.0.0.0/8, and Cloudflare's widest
	// ranges are /13 and /29.
	MinTrustedBitsV4 = 8
	MinTrustedBitsV6 = 16
)

// TrustedProxiesConfig is the desired set of trusted proxies, canonical and sorted.
type TrustedProxiesConfig struct {
	CIDRs []string
}

// NormalizeTrustedProxies validates an operator's list and returns it canonical:
// masked, IPv4-unmapped, deduplicated and sorted. The same input always gives the
// same output, because the result is compared against Caddy's config every
// reconcile pass and a write is a reload.
//
// It REJECTS rather than drops: someone who pasted a range wants to hear that it
// was refused, not to find out later that it was ignored. It uses net/netip, which
// is what Caddy parses ranges with, so a value accepted here is not one Caddy
// then refuses — and a bad value never reaches the proxy to be refused.
func NormalizeTrustedProxies(entries []string) ([]string, error) {
	if len(entries) > MaxTrustedProxies {
		return nil, fmt.Errorf("at most %d ranges are allowed, got %d", MaxTrustedProxies, len(entries))
	}
	seen := make(map[string]struct{}, len(entries))
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		e = strings.TrimSpace(e)
		if e == "" {
			return nil, fmt.Errorf("an empty range was given")
		}
		p, err := netip.ParsePrefix(e)
		if err != nil {
			a, aerr := netip.ParseAddr(e)
			if aerr != nil {
				return nil, fmt.Errorf("%q is not an IP address or CIDR range", clip(e))
			}
			// Checked before PrefixFrom, which would silently drop the zone.
			if a.Zone() != "" {
				return nil, fmt.Errorf("%q: an address with a zone is not allowed", clip(e))
			}
			p = netip.PrefixFrom(a, a.BitLen())
		}
		// An IPv4-mapped IPv6 prefix is an IPv4 one: ::ffff:10.0.0.0/104 and
		// 10.0.0.0/8 are the same range and must not be two entries.
		if p.Addr().Is4In6() {
			if p.Bits() < 96 {
				return nil, fmt.Errorf("%q is too wide to be an IPv4-mapped range", clip(e))
			}
			p = netip.PrefixFrom(p.Addr().Unmap(), p.Bits()-96)
		}
		p = p.Masked()
		min := MinTrustedBitsV6
		if p.Addr().Is4() {
			min = MinTrustedBitsV4
		}
		if p.Bits() < min {
			return nil, fmt.Errorf("%q is too wide: every address in it could claim any client IP, so the shortest allowed prefix is /%d", clip(e), min)
		}
		if _, dup := seen[p.String()]; dup {
			continue
		}
		seen[p.String()] = struct{}{}
		out = append(out, p.String())
	}
	sort.Strings(out)
	return out, nil
}

// clip bounds what an error echoes back from a request.
func clip(s string) string {
	if len(s) > 64 {
		return s[:64] + "…"
	}
	return s
}

// DecodeTrustedProxies reads the stored setting. A value that no longer
// validates (a bound tightened since it was saved) is an error, not an empty
// list: reading it as "none" would silently stop attributing visitors.
func DecodeTrustedProxies(raw string) ([]string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	var list []string
	if err := json.Unmarshal([]byte(raw), &list); err != nil {
		return nil, fmt.Errorf("decoding %s: %w", SettingTrustedProxies, err)
	}
	return NormalizeTrustedProxies(list)
}

// EncodeTrustedProxies renders a normalised list for storage.
func EncodeTrustedProxies(cidrs []string) string {
	if len(cidrs) == 0 {
		return ""
	}
	b, _ := json.Marshal(cidrs)
	return string(b)
}

// TrustedProxyPreset is a published set of proxy ranges an operator can adopt.
// It is never applied by default and never fetched at run time: a preset that
// changed underneath an install would change who may claim any address.
type TrustedProxyPreset struct {
	ID    string   `json:"id"`
	Name  string   `json:"name"`
	CIDRs []string `json:"cidrs"`
	// Source is where to re-check the list, and CheckedAt is the date it was copied.
	Source    string `json:"source"`
	CheckedAt string `json:"checked_at"`
}

// TrustedProxyPresets returns the presets shipped in the binary.
func TrustedProxyPresets() []TrustedProxyPreset {
	return []TrustedProxyPreset{{
		ID:   "cloudflare",
		Name: "Cloudflare",
		CIDRs: []string{
			"173.245.48.0/20", "103.21.244.0/22", "103.22.200.0/22", "103.31.4.0/22",
			"141.101.64.0/18", "108.162.192.0/18", "190.93.240.0/20", "188.114.96.0/20",
			"197.234.240.0/22", "198.41.128.0/17", "162.158.0.0/15", "104.16.0.0/13",
			"104.24.0.0/14", "172.64.0.0/13", "131.0.72.0/22",
			"2400:cb00::/32", "2606:4700::/32", "2803:f800::/32", "2405:b500::/32",
			"2405:8100::/32", "2a06:98c0::/29", "2c0f:f248::/32",
		},
		Source:    "https://www.cloudflare.com/ips/",
		CheckedAt: "2026-10-28",
	}}
}
