package worker

import (
	"context"
	"net"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/belune-hq/belune/internal/config"
	"github.com/belune-hq/belune/internal/pkg/netutil"
)

func TestIsPublicIP(t *testing.T) {
	// A private or loopback autodetect must disable the DNS precheck rather than
	// flag every domain as misconfigured.
	assert.False(t, netutil.IsPublicIP(net.ParseIP("192.168.1.10")))
	assert.False(t, netutil.IsPublicIP(net.ParseIP("10.0.0.5")))
	assert.False(t, netutil.IsPublicIP(net.ParseIP("127.0.0.1")))
	assert.True(t, netutil.IsPublicIP(net.ParseIP("203.0.113.7")))
}

// An unsubstituted placeholder in BELUNE_PUBLIC_IP (the install block ships
// BELUNE_PUBLIC_IP=VPS_IP for the operator to replace) must not become the
// baseline every domain is compared against: it matches nothing, so every domain
// would be reported as resolving "not to this server (VPS_IP)". A garbage
// baseline is worse than none, so it is ignored.
func TestPublicIPRejectsNonIPConfig(t *testing.T) {
	tests := []struct {
		name       string
		configured string
		want       string
	}{
		{name: "unsubstituted placeholder is ignored", configured: "VPS_IP", want: ""},
		{name: "a hostname is not an IP", configured: "belune.example.com", want: ""},
		{name: "empty falls through to autodetection", configured: "", want: ""},
		{name: "a real IPv4 is honoured", configured: "203.0.113.7", want: "203.0.113.7"},
		{name: "a real IPv6 is honoured", configured: "2001:db8::1", want: "2001:db8::1"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Reset the memoised autodetect so a previous case cannot leak into
			// this one, and so autodetection (which would reach the network) is
			// short-circuited to "unknown".
			publicIPOnce = sync.Once{}
			publicIPValue = ""
			publicIPOnce.Do(func() {})

			h := &TaskHandler{Config: &config.Config{PublicIP: tt.configured}}
			assert.Equal(t, tt.want, h.publicIP(context.Background()))
		})
	}
}
