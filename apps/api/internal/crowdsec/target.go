package crowdsec

import (
	"errors"
	"fmt"
	"net/netip"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// Prefix-length floors. A range ban is for a scanner's /24; a /8 typed by mistake
// would ban a continent, and CrowdSec would carry it out.
const (
	minRangeBits4 = 16
	minRangeBits6 = 48
)

const (
	// MinBanDuration and MaxBanDuration bound a manual ban. There is no permanent
	// one: CrowdSec would accept 999999h, and a ban nobody remembers making is how
	// an address stays blocked for years after the reason is gone.
	MinBanDuration = time.Minute
	MaxBanDuration = 365 * 24 * time.Hour

	// maxReasonRunes bounds the free text that rides along on a decision.
	maxReasonRunes = 100

	// ReasonPrefixManual and ReasonPrefixWAF mark where a decision came from.
	// CrowdSec records every cscli-created decision with the same origin, so the
	// prefix on the reason is the only attribution that survives in the API.
	ReasonPrefixManual = "belune: "
	ReasonPrefixWAF    = "belune-waf: "
)

// ReasonSource says what created a decision, from the reason it was given.
func ReasonSource(reason, origin string) string {
	switch {
	case strings.HasPrefix(reason, ReasonPrefixWAF):
		return "waf"
	case strings.HasPrefix(reason, ReasonPrefixManual):
		return "dashboard"
	case origin == "cscli":
		return "cli"
	default:
		return "crowdsec"
	}
}

// unbannable are the ranges a ban must never name. In a Docker deployment the
// address Caddy sees for a request can be the bridge gateway or a proxy in front,
// and banning that bans everyone behind it; the rest cannot be a remote visitor.
var unbannable = mustPrefixes(
	"0.0.0.0/8", "10.0.0.0/8", "127.0.0.0/8", "169.254.0.0/16", "172.16.0.0/12",
	"192.168.0.0/16", "224.0.0.0/4", "240.0.0.0/4",
	"::/128", "::1/128", "fc00::/7", "fe80::/10", "ff00::/8",
)

func mustPrefixes(s ...string) []netip.Prefix {
	out := make([]netip.Prefix, len(s))
	for i, p := range s {
		out[i] = netip.MustParsePrefix(p)
	}
	return out
}

// ErrNotBannable is a target Belune refuses on principle; its message says why.
type ErrNotBannable struct{ Reason string }

func (e *ErrNotBannable) Error() string { return e.Reason }

// ParseTarget reads an address or a CIDR range. An IPv4-mapped IPv6 address is
// unmapped first: CrowdSec would store ::ffff:203.0.113.9 as an IPv6 ban that no
// IPv4 visitor ever matches, so the ban would exist and do nothing.
func ParseTarget(s string) (netip.Prefix, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return netip.Prefix{}, errors.New("an IP address or CIDR range is required")
	}
	if strings.Contains(s, "/") {
		p, err := netip.ParsePrefix(s)
		if err != nil {
			return netip.Prefix{}, fmt.Errorf("%q is not a valid CIDR range", s)
		}
		if p.Addr().Zone() != "" {
			return netip.Prefix{}, errors.New("a zone (%) is not allowed in an address")
		}
		if p.Addr().Is4In6() {
			return netip.Prefix{}, errors.New("write an IPv4-mapped range as plain IPv4")
		}
		return p.Masked(), nil
	}
	a, err := netip.ParseAddr(s)
	if err != nil || a.Zone() != "" {
		return netip.Prefix{}, fmt.Errorf("%q is not a valid IP address", s)
	}
	a = a.Unmap()
	return netip.PrefixFrom(a, a.BitLen()), nil
}

// CheckBannable refuses targets that must not be banned, whoever asks.
func CheckBannable(t netip.Prefix) error {
	for _, bad := range unbannable {
		if bad.Overlaps(t) {
			return &ErrNotBannable{Reason: fmt.Sprintf(
				"%s overlaps %s, a private, loopback or reserved range. Behind Docker the address Caddy sees can be a gateway or proxy shared by every visitor, so banning it would ban them all", t, bad)}
		}
	}
	if t.IsSingleIP() {
		return nil
	}
	floor := minRangeBits4
	if t.Addr().Is6() {
		floor = minRangeBits6
	}
	if t.Bits() < floor {
		return &ErrNotBannable{Reason: fmt.Sprintf("%s is wider than /%d, the widest range that can be banned", t, floor)}
	}
	return nil
}

var durationRe = regexp.MustCompile(`^([1-9][0-9]{0,5})([mhd])$`)

// ParseDuration reads a ban length: a whole number and one of m, h or d ("90m",
// "24h", "7d"), within MinBanDuration and MaxBanDuration.
func ParseDuration(s string) (time.Duration, error) {
	m := durationRe.FindStringSubmatch(strings.TrimSpace(s))
	if m == nil {
		return 0, errors.New(`duration must be a number and a unit, like "90m", "24h" or "7d"`)
	}
	n, _ := strconv.Atoi(m[1])
	unit := map[string]time.Duration{"m": time.Minute, "h": time.Hour, "d": 24 * time.Hour}[m[2]]
	d := time.Duration(n) * unit
	if d < MinBanDuration || d > MaxBanDuration {
		return 0, fmt.Errorf("duration must be between 1m and 365d")
	}
	return d, nil
}

// CleanReason makes free text safe to hand to cscli and to print into logs: control
// characters (a newline would put a forged line into the LAPI's log) are dropped,
// whitespace collapsed, the length bounded. Returns "" for text with nothing left.
func CleanReason(s string) string {
	var b strings.Builder
	space := false
	for _, r := range s {
		switch {
		case unicode.IsSpace(r):
			space = b.Len() > 0
		case unicode.IsPrint(r) && r != utf8.RuneError:
			if space {
				b.WriteByte(' ')
				space = false
			}
			b.WriteRune(r)
		}
	}
	out := b.String()
	if utf8.RuneCountInString(out) > maxReasonRunes {
		out = string([]rune(out)[:maxReasonRunes])
	}
	return strings.TrimSpace(out)
}
