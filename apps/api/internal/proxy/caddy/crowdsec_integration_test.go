package caddy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/belune-hq/belune/internal/crowdsec"
	"github.com/belune-hq/belune/internal/proxy"
)

// These drive a REAL Caddy with the CrowdSec bouncer and a REAL CrowdSec local API,
// because what they assert is what no mock can: that a ban placed through Belune's
// own commands is enforced by Caddy's bouncer, that lifting it (or allowlisting the
// address) restores traffic, and that the config Belune writes is one Caddy loads.
//
//	BELUNE_CADDY_INTEGRATION=1 CADDY_ADMIN_URL=http://127.0.0.1:12019 \
//	CADDY_HTTP_URL=http://127.0.0.1:18088 \
//	CROWDSEC_CSCLI=/path/to/cscli CROWDSEC_CONFIG=/path/to/config.yaml \
//	CROWDSEC_LAPI_URL=http://127.0.0.1:18080 \
//	  go test ./internal/proxy/caddy/ -run RealCaddy_CrowdSec -v
//
// The local API is driven by running cscli on this machine against its config
// instead of through `docker exec`, which is the only difference from production.
// The ban target is 127.0.0.2: the whole of 127/8 is loopback, so a client that
// binds that source address is a different visitor from the one on 127.0.0.1.
// (Service.Ban would refuse a loopback target, by design; these call the
// Controller, which is below that guard.)

const csAppHost = "cs-e2e.belune.local"

type localCscli struct{ bin, config string }

func (l localCscli) Run(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, l.bin, append([]string{"-c", l.config}, args...)...)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return "", &crowdsec.CommandError{Command: args[0] + " " + args[1], ExitCode: ee.ExitCode(), Stderr: errb.String()}
		}
		return "", err
	}
	return out.String(), nil
}

type csRig struct {
	c       *Client
	ctl     *crowdsec.Controller
	httpURL string
	apiURL  string
	key     string
}

func newCrowdSecRig(t *testing.T) *csRig {
	t.Helper()
	c := liveClient(t)
	bin, cfg, api := os.Getenv("CROWDSEC_CSCLI"), os.Getenv("CROWDSEC_CONFIG"), os.Getenv("CROWDSEC_LAPI_URL")
	if bin == "" || cfg == "" || api == "" {
		t.Skip("set CROWDSEC_CSCLI, CROWDSEC_CONFIG and CROWDSEC_LAPI_URL to run against a real CrowdSec local API")
	}
	ctx := context.Background()

	rig := &csRig{c: c, ctl: crowdsec.NewController(localCscli{bin, cfg}), httpURL: os.Getenv("CADDY_HTTP_URL"), apiURL: api}
	if rig.httpURL == "" {
		rig.httpURL = "http://127.0.0.1:80"
	}

	app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "ok")
	}))
	t.Cleanup(app.Close)
	upstreamHost := os.Getenv("CADDY_UPSTREAM_HOST")
	if upstreamHost == "" {
		upstreamHost = "127.0.0.1"
	}
	u, err := url.Parse(app.URL)
	require.NoError(t, err)

	c.InitCatchAll(ctx)
	require.NoError(t, c.AddRoute(ctx, proxy.RouteConfig{
		Hostname: csAppHost, TargetURL: "http://" + upstreamHost + ":" + u.Port(), SSLMode: proxy.SSLModeOff,
	}))

	// Start from a clean local API, and finish by leaving one.
	require.NoError(t, rig.ctl.DeleteBouncer(ctx, crowdsec.BouncerName))
	rig.key, err = rig.ctl.RegisterBouncer(ctx, crowdsec.BouncerName)
	require.NoError(t, err)

	t.Cleanup(func() {
		_ = c.SyncCrowdSec(ctx, proxy.CrowdSecConfig{})
		_ = c.RemoveRoute(ctx, csAppHost, "/")
		_ = rig.ctl.DeleteBouncer(ctx, crowdsec.BouncerName)
		for _, ip := range []string{"127.0.0.2/32", "127.0.0.3/32"} {
			p := mustPrefix(ip)
			_ = rig.ctl.Unban(ctx, p)
			_ = rig.ctl.Unallow(ctx, p)
		}
	})
	return rig
}

func (r *csRig) config() proxy.CrowdSecConfig {
	return proxy.CrowdSecConfig{Enabled: true, APIURL: r.apiURL, APIKey: r.key, Hosts: []string{csAppHost}}
}

// get requests the application from source address src and returns the status.
func (r *csRig) get(t *testing.T, src, host string) int {
	t.Helper()
	d := &net.Dialer{LocalAddr: &net.TCPAddr{IP: net.ParseIP(src)}, Timeout: 5 * time.Second}
	client := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{DialContext: d.DialContext, DisableKeepAlives: true}}
	req, err := http.NewRequest(http.MethodGet, r.httpURL+"/", nil)
	require.NoError(t, err)
	req.Host = host
	resp, err := client.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode
}

// eventually polls get until it returns want. The bouncer asks the local API for
// changes on a timer (bouncerTickerInterval), so an effect is never immediate.
func (r *csRig) eventually(t *testing.T, src, host string, want int, what string) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	var got int
	for time.Now().Before(deadline) {
		if got = r.get(t, src, host); got == want {
			return
		}
		time.Sleep(300 * time.Millisecond)
	}
	t.Fatalf("%s: wanted %d from %s, still getting %d after 20s", what, want, src, got)
}

func (r *csRig) waitForFirstPull(t *testing.T) {
	t.Helper()
	require.Eventually(t, func() bool {
		b, found, err := r.ctl.Bouncer(context.Background(), crowdsec.BouncerName)
		return err == nil && found && !b.LastPull.IsZero()
	}, 20*time.Second, 300*time.Millisecond, "the bouncer never pulled the ban list: Caddy is not talking to the local API")
}

func TestRealCaddy_CrowdSec_AcceptsTheConfigAndKeepsItFirst(t *testing.T) {
	rig := newCrowdSecRig(t)
	ctx := context.Background()

	require.NoError(t, rig.c.SyncWAF(ctx, proxy.WAFConfig{}), "start without a WAF")
	require.NoError(t, rig.c.SyncCrowdSec(ctx, rig.config()), "Caddy refused the bouncer's config")

	routes, err := rig.c.fetchRawRoutes(ctx)
	require.NoError(t, err)
	require.NotEmpty(t, routes)
	var first struct {
		ID string `json:"@id"`
	}
	require.NoError(t, jsonUnmarshal(routes[0], &first))
	assert.Equal(t, crowdsecRouteID, first.ID)

	// And it survives the reorder the next AddRoute does.
	require.NoError(t, rig.c.AddRoute(ctx, proxy.RouteConfig{Hostname: "other-" + csAppHost, TargetURL: "http://127.0.0.1:9", SSLMode: proxy.SSLModeOff}))
	t.Cleanup(func() { _ = rig.c.RemoveRoute(ctx, "other-"+csAppHost, "/") })
	routes, err = rig.c.fetchRawRoutes(ctx)
	require.NoError(t, err)
	require.NoError(t, jsonUnmarshal(routes[0], &first))
	assert.Equal(t, crowdsecRouteID, first.ID, "an application route must never end up in front of the bouncer")

	// Unchanged: the next pass writes nothing.
	require.NoError(t, rig.c.SyncCrowdSec(ctx, rig.config()))
}

func TestRealCaddy_CrowdSec_ABanBlocksAndLiftingItRestores(t *testing.T) {
	rig := newCrowdSecRig(t)
	ctx := context.Background()
	require.NoError(t, rig.c.SyncCrowdSec(ctx, rig.config()))
	rig.waitForFirstPull(t)

	require.Equal(t, http.StatusOK, rig.get(t, "127.0.0.2", csAppHost))

	banned := mustPrefix("127.0.0.2/32")
	require.NoError(t, rig.ctl.Ban(ctx, banned, time.Hour, "belune: integration test"))
	rig.eventually(t, "127.0.0.2", csAppHost, http.StatusForbidden, "a ban is enforced")
	assert.Equal(t, http.StatusOK, rig.get(t, "127.0.0.1", csAppHost), "a different visitor is not affected")

	ds, err := rig.ctl.Decisions(ctx)
	require.NoError(t, err)
	var found bool
	for _, d := range ds {
		found = found || (d.Value == "127.0.0.2" && d.Reason == "belune: integration test" && d.ExpiresAt.After(time.Now().Add(50*time.Minute)))
	}
	assert.True(t, found, "the listing Belune shows is the ban CrowdSec holds: %+v", ds)

	require.NoError(t, rig.ctl.Unban(ctx, banned))
	rig.eventually(t, "127.0.0.2", csAppHost, http.StatusOK, "lifting a ban restores access")
}

func TestRealCaddy_CrowdSec_AllowlistingLiftsABanAndPreventsTheNext(t *testing.T) {
	rig := newCrowdSecRig(t)
	ctx := context.Background()
	require.NoError(t, rig.c.SyncCrowdSec(ctx, rig.config()))
	rig.waitForFirstPull(t)

	admin := mustPrefix("127.0.0.3/32")
	require.NoError(t, rig.ctl.Ban(ctx, admin, time.Hour, "belune: oops"))
	rig.eventually(t, "127.0.0.3", csAppHost, http.StatusForbidden, "the admin has been banned")

	// This is the login hook: allowlisting the address lifts the ban on it.
	require.NoError(t, rig.ctl.Allow(ctx, admin, "login: admin@example.com", 30*24*time.Hour))
	rig.eventually(t, "127.0.0.3", csAppHost, http.StatusOK, "logging in restores a banned admin")

	// ...and nothing Belune does can ban them again while the entry stands.
	err := rig.ctl.Ban(ctx, admin, time.Hour, "belune: again")
	assert.ErrorIs(t, err, crowdsec.ErrAllowlisted)
	assert.ErrorIs(t, rig.ctl.Ban(ctx, mustPrefix("127.0.0.0/24"), time.Hour, "belune: range"), crowdsec.ErrAllowlisted,
		"a range that contains an allowlisted address is refused too")
	assert.Equal(t, http.StatusOK, rig.get(t, "127.0.0.3", csAppHost))
}

func TestRealCaddy_CrowdSec_TheDashboardIsOutsideTheBouncer(t *testing.T) {
	rig := newCrowdSecRig(t)
	ctx := context.Background()
	require.NoError(t, rig.c.SyncCrowdSec(ctx, rig.config()))
	rig.waitForFirstPull(t)

	require.NoError(t, rig.ctl.Ban(ctx, mustPrefix("127.0.0.2/32"), time.Hour, "belune: integration test"))
	rig.eventually(t, "127.0.0.2", csAppHost, http.StatusForbidden, "the application host is banned")

	// A host the route does not name is served by the catch-all (the dashboard).
	// The upstream may not be running here, so what is asserted is only that the
	// answer is not the bouncer's.
	got := rig.get(t, "127.0.0.2", "dashboard.belune.local")
	assert.NotEqual(t, http.StatusForbidden, got, "a banned address must still be able to reach the dashboard that lifts the ban")
}

func TestRealCaddy_CrowdSec_AnUnreachableLocalAPIFailsOpen(t *testing.T) {
	rig := newCrowdSecRig(t)
	ctx := context.Background()

	cfg := rig.config()
	cfg.APIURL = "http://127.0.0.1:1" // nothing listens here
	require.NoError(t, rig.c.SyncCrowdSec(ctx, cfg), "Caddy must accept a bouncer whose local API is down: a CrowdSec outage is not a reason to refuse every other route change")
	assert.Equal(t, http.StatusOK, rig.get(t, "127.0.0.2", csAppHost), "and traffic must flow")

	// A wrong key is the same: a bouncer that cannot authenticate admits everyone.
	cfg = rig.config()
	cfg.APIKey = "not-a-registered-key"
	require.NoError(t, rig.c.SyncCrowdSec(ctx, cfg))
	assert.Equal(t, http.StatusOK, rig.get(t, "127.0.0.2", csAppHost))
}

func mustPrefix(s string) netip.Prefix { return netip.MustParsePrefix(s) }

func jsonUnmarshal(b []byte, v any) error { return json.Unmarshal(b, v) }

// A wildcard application host covers the dashboard when the dashboard is served on
// a name under it. Without the exemption a banned admin could not reach the page
// that lifts the ban, on exactly the installs that use wildcard certificates.
func TestRealCaddy_CrowdSec_AWildcardDoesNotCoverTheDashboard(t *testing.T) {
	rig := newCrowdSecRig(t)
	ctx := context.Background()

	cfg := rig.config()
	cfg.Hosts = []string{"*.cs-wild.belune.local"}
	cfg.Exempt = []string{"dash.cs-wild.belune.local"}
	require.NoError(t, rig.c.SyncCrowdSec(ctx, cfg), "Caddy refused the exemption")
	rig.waitForFirstPull(t)

	require.NoError(t, rig.ctl.Ban(ctx, mustPrefix("127.0.0.2/32"), time.Hour, "belune: integration test"))
	rig.eventually(t, "127.0.0.2", "shop.cs-wild.belune.local", http.StatusForbidden, "a name the wildcard covers is banned")

	assert.NotEqual(t, http.StatusForbidden, rig.get(t, "127.0.0.2", "dash.cs-wild.belune.local"),
		"the dashboard sits under the wildcard and must not be behind the bouncer")
}
