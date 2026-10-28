package proxy

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNormalizeTrustedProxies(t *testing.T) {
	got, err := NormalizeTrustedProxies([]string{
		" 10.1.2.3/8 ",           // masked to its network
		"172.20.0.5",             // a bare address is a host route
		"10.0.0.0/8",             // a duplicate of the first, after masking
		"::ffff:192.168.0.0/112", // the same range as 192.168.0.0/16
		"2001:db8:ffff::1/32",
	})
	require.NoError(t, err)
	assert.Equal(t, []string{"10.0.0.0/8", "172.20.0.5/32", "192.168.0.0/16", "2001:db8::/32"}, got)
}

func TestNormalizeTrustedProxies_IsDeterministic(t *testing.T) {
	a, _ := NormalizeTrustedProxies([]string{"192.168.0.0/16", "10.0.0.0/8"})
	b, _ := NormalizeTrustedProxies([]string{"10.0.0.0/8", "192.168.0.0/16", "10.0.0.0/8"})
	assert.Equal(t, a, b, "order and duplicates are not drift: a difference is a Caddy reload")
}

func TestNormalizeTrustedProxies_RefusesWhatCouldNotBeTrusted(t *testing.T) {
	for name, in := range map[string]string{
		"the whole internet":              "0.0.0.0/0",
		"just under the bound":            "0.0.0.0/7",
		"every v6 address":                "::/0",
		"a wide v6 range":                 "fd00::/8",
		"wide v4-mapped":                  "::ffff:0:0/80",
		"not an address":                  "cloudflare",
		"a url":                           "https://example.com",
		"empty":                           "",
		"a zone":                          "fe80::1%eth0",
		"a list in one entry":             "10.0.0.0/8,192.168.0.0/16",
		"an interior newline":             "10.0.0.0/8\n11.0.0.0/8",
		"a quote that would end a string": `10.0.0.0/8"]`,
	} {
		t.Run(name, func(t *testing.T) {
			_, err := NormalizeTrustedProxies([]string{in})
			assert.Error(t, err)
		})
	}
}

func TestNormalizeTrustedProxies_BoundsTheListAndTheErrorEcho(t *testing.T) {
	many := make([]string, MaxTrustedProxies+1)
	for i := range many {
		many[i] = "10.0.0.1"
	}
	_, err := NormalizeTrustedProxies(many)
	assert.Error(t, err)

	_, err = NormalizeTrustedProxies([]string{strings.Repeat("x", 10_000)})
	require.Error(t, err)
	assert.Less(t, len(err.Error()), 200, "a request must not be able to put 10 KB in an error")
}

func TestTheCloudflarePresetPassesItsOwnValidation(t *testing.T) {
	p := TrustedProxyPresets()[0]
	got, err := NormalizeTrustedProxies(p.CIDRs)
	require.NoError(t, err, "a preset the API would refuse is a button that cannot work")
	assert.Len(t, got, len(p.CIDRs), "and none of it is dropped as a duplicate")
}

func TestTrustedProxiesStorageRoundTrips(t *testing.T) {
	in := []string{"10.0.0.0/8", "192.168.0.0/16"}
	out, err := DecodeTrustedProxies(EncodeTrustedProxies(in))
	require.NoError(t, err)
	assert.Equal(t, in, out)

	none, err := DecodeTrustedProxies("")
	require.NoError(t, err)
	assert.Empty(t, none)
	assert.Empty(t, EncodeTrustedProxies(nil))

	_, err = DecodeTrustedProxies("not json")
	assert.Error(t, err, "a corrupt row is an error, not 'no proxies'")
	_, err = DecodeTrustedProxies(`["0.0.0.0/0"]`)
	assert.Error(t, err, "and so is one that no longer validates")
}
