package server

import (
	"net/netip"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestParsePeerCIDRs(t *testing.T) {
	got := parsePeerCIDRs([]string{
		"172.20.0.5",       // a bare address is a host route
		"192.168.215.7/24", // masked to its network
		"10.0.0.0/8",       // the widest a private network needs
		"0.0.0.0/0",        // would believe every caller's header
		"0.0.0.0/7",
		"::/0",
		"fd00::/8", // too wide for v6: below the /16 bound
		"fd00::/16",
		"not-an-address",
	})
	want := []netip.Prefix{
		netip.MustParsePrefix("172.20.0.5/32"),
		netip.MustParsePrefix("192.168.215.0/24"),
		netip.MustParsePrefix("10.0.0.0/8"),
		netip.MustParsePrefix("fd00::/16"),
	}
	assert.Equal(t, want, got)
}
