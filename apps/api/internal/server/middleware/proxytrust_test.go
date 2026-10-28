package middleware

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/belune-hq/belune/internal/proxy"
)

var (
	caddyAddr   = netip.MustParseAddr("172.20.0.5")
	gatewayAddr = netip.MustParseAddr("172.20.0.1")
)

// lister is a ProxyLister whose answer a test can change, and which counts asks.
type lister struct {
	addrs []netip.Addr
	err   error
	calls atomic.Int32
}

func (l *lister) list(context.Context) ([]netip.Addr, error) {
	l.calls.Add(1)
	return l.addrs, l.err
}

type rig struct {
	t     *testing.T
	trust *ProxyTrust
	l     *lister
	clock time.Time
	// seen is the RemoteAddr the handler behind the middleware received.
	seen string
}

func newRig(t *testing.T, addrs ...netip.Addr) *rig {
	t.Helper()
	l := &lister{addrs: addrs}
	r := &rig{t: t, l: l, clock: time.Unix(1_700_000_000, 0)}
	r.trust = NewProxyTrust(l.list, nil, gatewayAddr)
	r.trust.now = func() time.Time { return r.clock }
	return r
}

// do sends one request from peer carrying the given headers.
func (r *rig) do(peer, path string, hdr map[string]string) *httptest.ResponseRecorder {
	r.t.Helper()
	h := r.trust.Middleware(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		r.seen = req.RemoteAddr
		assert.Empty(r.t, req.Header.Get(proxy.ClientIPHeader), "the header must not outlive the decision")
		w.WriteHeader(http.StatusOK)
	}))
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.RemoteAddr = peer + ":4444"
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	r.seen = ""
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func clientOf(r *rig) string {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = r.seen
	return ClientIP(req)
}

func TestProxyTrust_ThePeerThatIsCaddyIsBelievedAboutTheClient(t *testing.T) {
	r := newRig(t, caddyAddr)
	rec := r.do(caddyAddr.String(), "/api/x", map[string]string{proxy.ClientIPHeader: "203.0.113.9"})
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "203.0.113.9", clientOf(r))
}

func TestProxyTrust_NoOtherHeaderIsEverRead(t *testing.T) {
	r := newRig(t, caddyAddr)
	// Through Caddy, with every client-controlled header forged and no trusted one:
	// the answer is the proxy's own address, not any of the claims.
	r.do(caddyAddr.String(), "/", map[string]string{
		"X-Forwarded-For": "198.51.100.1", "X-Real-IP": "198.51.100.2", "True-Client-IP": "198.51.100.3",
	})
	assert.Equal(t, caddyAddr.String(), clientOf(r))
}

func TestProxyTrust_AHeaderFromAnotherPeerIsRefusedAndNeverBelieved(t *testing.T) {
	r := newRig(t, caddyAddr)
	app := "172.30.0.9" // a container on a project network
	rec := r.do(app, "/api/auth/login", map[string]string{proxy.ClientIPHeader: "203.0.113.9"})
	assert.Equal(t, http.StatusForbidden, rec.Code, "the forged header must not make a direct caller look like it came through Caddy")
	assert.Empty(t, r.seen, "the handler must not run")
}

func TestProxyTrust_HealthzIsOpenToAnyone(t *testing.T) {
	r := newRig(t, caddyAddr)
	rec := r.do("172.30.0.9", "/healthz", nil)
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "172.30.0.9", clientOf(r), "and its caller is still attributed by its own address")
}

func TestProxyTrust_TheHostIsNotRefused(t *testing.T) {
	r := newRig(t, caddyAddr)
	for _, peer := range []string{"127.0.0.1", gatewayAddr.String()} {
		rec := r.do(peer, "/api/auth/login", map[string]string{proxy.ClientIPHeader: "203.0.113.9"})
		assert.Equal(t, http.StatusOK, rec.Code, peer)
		assert.Equal(t, peer, clientOf(r), "the host's header is not believed either: %s", peer)
	}
}

func TestProxyTrust_UnknownIsServedNotRefused(t *testing.T) {
	t.Run("the lookup fails", func(t *testing.T) {
		r := newRig(t)
		r.l.err = errors.New("docker: connection refused")
		rec := r.do("172.30.0.9", "/api/x", map[string]string{proxy.ClientIPHeader: "203.0.113.9"})
		assert.Equal(t, http.StatusOK, rec.Code, "a Docker hiccup must not take the dashboard down")
		assert.Equal(t, "172.30.0.9", clientOf(r), "and the header is still not believed")
	})
	t.Run("there is no Caddy container to find", func(t *testing.T) {
		r := newRig(t) // an empty answer with no error
		rec := r.do("172.30.0.9", "/api/x", nil)
		assert.Equal(t, http.StatusOK, rec.Code)
	})
}

func TestProxyTrust_ATransientErrorKeepsTheLastGoodSet(t *testing.T) {
	r := newRig(t, caddyAddr)
	require.Equal(t, http.StatusOK, r.do(caddyAddr.String(), "/", nil).Code)

	r.clock = r.clock.Add(proxyTTL + time.Second)
	r.l.err = errors.New("docker: timeout")
	r.do(caddyAddr.String(), "/", map[string]string{proxy.ClientIPHeader: "203.0.113.9"})
	assert.Equal(t, "203.0.113.9", clientOf(r), "Caddy is still Caddy while Docker is down")
	// …and a stranger is still not let in just because the lookup is failing: the
	// set is known, so this is PeerOther, not Unknown.
	assert.Equal(t, http.StatusForbidden, r.do("172.30.0.9", "/api/x", nil).Code)
}

func TestProxyTrust_ARestartedCaddyOnANewAddressIsFoundOnTheFirstMiss(t *testing.T) {
	r := newRig(t, caddyAddr)
	require.Equal(t, http.StatusOK, r.do(caddyAddr.String(), "/", nil).Code)

	moved := netip.MustParseAddr("172.20.0.77")
	r.l.addrs = []netip.Addr{moved}
	r.clock = r.clock.Add(proxyRetry + time.Second)
	rec := r.do(moved.String(), "/api/x", map[string]string{proxy.ClientIPHeader: "203.0.113.9"})
	assert.Equal(t, http.StatusOK, rec.Code, "the dashboard must not 403 until the next scheduled refresh")
	assert.Equal(t, "203.0.113.9", clientOf(r))
}

func TestProxyTrust_AReleasedAddressStopsBeingTrustedWithinTheTTL(t *testing.T) {
	r := newRig(t, caddyAddr)
	require.Equal(t, http.StatusOK, r.do(caddyAddr.String(), "/", nil).Code)

	// Caddy is gone and its address now belongs to something else.
	r.l.addrs = []netip.Addr{netip.MustParseAddr("172.20.0.99")}
	r.clock = r.clock.Add(proxyTTL + time.Second)
	rec := r.do(caddyAddr.String(), "/api/x", map[string]string{proxy.ClientIPHeader: "203.0.113.9"})
	assert.Equal(t, http.StatusForbidden, rec.Code)
}

func TestProxyTrust_AStrangerCannotMakeEveryRequestCostALookup(t *testing.T) {
	r := newRig(t, caddyAddr)
	r.do(caddyAddr.String(), "/", nil)
	base := r.l.calls.Load()
	for i := 0; i < 50; i++ {
		r.do("172.30.0.9", "/api/x", nil)
	}
	assert.LessOrEqual(t, r.l.calls.Load()-base, int32(1))
}

func TestProxyTrust_ConfiguredPeersAreProxies(t *testing.T) {
	r := newRig(t)
	r.trust.extra = []netip.Prefix{netip.MustParsePrefix("192.168.215.0/24")}
	r.do("192.168.215.7", "/", map[string]string{proxy.ClientIPHeader: "203.0.113.9"})
	assert.Equal(t, "203.0.113.9", clientOf(r))
}

func TestProxyTrust_ABadHeaderIsNotApplied(t *testing.T) {
	// Every one of these is something an audit column or a rate-limit key must
	// never receive, sent by the peer that IS believed.
	for name, v := range map[string]string{
		"a list":        "203.0.113.9, 198.51.100.1",
		"with a port":   "203.0.113.9:443",
		"a zone":        "fe80::1%eth0",
		"unspecified":   "0.0.0.0",
		"a name":        "evil.example",
		"overlong":      strings.Repeat("a", 4096),
		"a newline":     "203.0.113.9\nX: y",
		"empty padding": "  ",
	} {
		t.Run(name, func(t *testing.T) {
			r := newRig(t, caddyAddr)
			r.do(caddyAddr.String(), "/", map[string]string{proxy.ClientIPHeader: v})
			assert.Equal(t, caddyAddr.String(), clientOf(r))
		})
	}
}

func TestClientIP_IsAlwaysAColumnSizedAddressOrEmpty(t *testing.T) {
	for in, want := range map[string]string{
		"203.0.113.9:80":                "203.0.113.9",
		"[2001:db8::1]:80":              "2001:db8::1",
		"[::ffff:203.0.113.9]:80":       "203.0.113.9",
		"203.0.113.9":                   "203.0.113.9",
		"":                              "",
		"@":                             "", // a unix socket peer
		strings.Repeat("9", 200) + ":1": "",
	} {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.RemoteAddr = in
		req.Header.Set("X-Forwarded-For", "198.51.100.1")
		req.Header.Set("X-Real-IP", "198.51.100.2")
		req.Header.Set("True-Client-IP", "198.51.100.3")
		got := ClientIP(req)
		assert.Equal(t, want, got, in)
		assert.LessOrEqual(t, len(got), 45)
	}
}

func TestDefaultGateway(t *testing.T) {
	route := "Iface\tDestination\tGateway\tFlags\tRefCnt\tUse\tMetric\tMask\tMTU\tWindow\tIRTT\n" +
		"eth0\t00000000\t01D714AC\t0003\t0\t0\t0\t00000000\t0\t0\t0\n" +
		"eth0\t00D714AC\t00000000\t0001\t0\t0\t0\x00FFFFFF\t0\t0\t0\n"
	assert.Equal(t, "172.20.215.1", DefaultGateway(strings.NewReader(route)).String())
	assert.False(t, DefaultGateway(strings.NewReader("Iface\tDestination\n")).IsValid())
	assert.False(t, DefaultGateway(strings.NewReader("eth0\t00000000\t00000000\t0001\n")).IsValid(), "no gateway flag")
}
