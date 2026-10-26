package crowdsec

import (
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseTarget(t *testing.T) {
	cases := []struct {
		in, want string
		wantErr  string
	}{
		{"203.0.113.9", "203.0.113.9/32", ""},
		{" 203.0.113.9 ", "203.0.113.9/32", ""},
		{"2001:db8::1", "2001:db8::1/128", ""},
		// Mapped to plain IPv4, or the ban exists and no IPv4 visitor ever matches it.
		{"::ffff:203.0.113.9", "203.0.113.9/32", ""},
		{"203.0.113.0/24", "203.0.113.0/24", ""},
		// Host bits are masked off rather than refused.
		{"203.0.113.77/24", "203.0.113.0/24", ""},
		{"2001:db8:abcd::/48", "2001:db8:abcd::/48", ""},
		{"", "", "required"},
		{"not an ip", "", "not a valid IP"},
		{"203.0.113.9/33", "", "not a valid CIDR"},
		{"fe80::1%eth0", "", "not a valid IP"},
		{"::ffff:203.0.113.0/120", "", "IPv4-mapped"},
		{"203.0.113.9 --bypass-allowlist", "", "not a valid IP"},
	}
	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			got, err := ParseTarget(c.in)
			if c.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), c.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, c.want, got.String())
		})
	}
}

func TestCheckBannable(t *testing.T) {
	refused := []string{
		"10.1.2.3", "172.17.0.1", "192.168.1.1", "127.0.0.1", "169.254.1.1", "0.0.0.0",
		"224.0.0.1", "::1", "fd00::1", "fe80::1", "::",
		// A public-looking range that swallows a private one is still refused.
		"0.0.0.0/8",
		// Too wide to be a considered decision.
		"203.0.0.0/8", "203.0.0.0/15", "2001:db8::/32",
	}
	for _, in := range refused {
		t.Run("refuse "+in, func(t *testing.T) {
			p, err := ParseTarget(in)
			require.NoError(t, err)
			var nb *ErrNotBannable
			require.ErrorAs(t, CheckBannable(p), &nb)
		})
	}

	allowed := []string{"203.0.113.9", "198.51.100.0/24", "203.0.0.0/16", "2001:db8:abcd::/48", "2001:db8::1", "100.64.0.1"}
	for _, in := range allowed {
		t.Run("allow "+in, func(t *testing.T) {
			p, err := ParseTarget(in)
			require.NoError(t, err)
			assert.NoError(t, CheckBannable(p))
		})
	}
}

func TestParseDuration(t *testing.T) {
	ok := map[string]time.Duration{
		"1m": time.Minute, "90m": 90 * time.Minute, "24h": 24 * time.Hour,
		"7d": 7 * 24 * time.Hour, "365d": 365 * 24 * time.Hour,
	}
	for in, want := range ok {
		got, err := ParseDuration(in)
		require.NoError(t, err, in)
		assert.Equal(t, want, got, in)
	}
	// Each of these is something cscli itself would have accepted.
	for _, in := range []string{"", "0m", "0s", "-5m", "366d", "999999h", "abc", "1.5h", "1h30m", "10", "1w", "08m", "7 d"} {
		_, err := ParseDuration(in)
		assert.Error(t, err, "%q must be refused", in)
	}
}

func TestCleanReason(t *testing.T) {
	assert.Equal(t, "scanner", CleanReason("  scanner \n"))
	// A newline would put a forged line into the local API's log.
	assert.Equal(t, "bad reason --ip 1.1.1.1", CleanReason("bad\nreason\t--ip 1.1.1.1"))
	assert.Equal(t, "ab", CleanReason("a\x00\x1bb"))
	assert.Equal(t, "", CleanReason("\n\n \x00"))
	long := CleanReason(strings.Repeat("é", 300))
	assert.Len(t, []rune(long), maxReasonRunes)
}

func TestReasonSource(t *testing.T) {
	assert.Equal(t, "waf", ReasonSource("belune-waf: attack-sqli: 12 requests in 10m", "cscli"))
	assert.Equal(t, "dashboard", ReasonSource("belune: scanner", "cscli"))
	assert.Equal(t, "cli", ReasonSource("added by hand", "cscli"))
	assert.Equal(t, "crowdsec", ReasonSource("crowdsecurity/ssh-bf", "crowdsec"))
	// "belune: " has to be the START of the reason: someone with cscli can write
	// anything, but a reason that merely mentions it is not Belune's.
	assert.Equal(t, "cli", ReasonSource("not belune: really", "cscli"))
}

func TestPrefixHelpers(t *testing.T) {
	ip := netip.MustParsePrefix("203.0.113.9/32")
	assert.Equal(t, "--ip=203.0.113.9", targetFlag(ip))
	assert.Equal(t, "203.0.113.9", targetValue(ip))
	rng := netip.MustParsePrefix("203.0.113.0/24")
	assert.Equal(t, "--range=203.0.113.0/24", targetFlag(rng))
	assert.Equal(t, "203.0.113.0/24", targetValue(rng))
	assert.Equal(t, "90m", formatMinutes(90*time.Minute))
	assert.Equal(t, "10080m", formatMinutes(7*24*time.Hour))
}
