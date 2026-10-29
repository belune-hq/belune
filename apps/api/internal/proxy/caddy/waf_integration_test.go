package caddy

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/belune-hq/belune/internal/proxy"
	"github.com/belune-hq/belune/internal/waf"
)

// These drive a REAL Caddy that has the WAF module, because what they assert is
// what no mock can: that Caddy accepts the directives, that the WAF in front of an
// application cannot hurt its traffic, and that Caddy's real log output is what the
// parser and the redaction were written against.
//
// Run against a Caddy built with coraza-caddy (the published belune-caddy:2 has it):
//
//	BELUNE_CADDY_INTEGRATION=1 CADDY_HTTP_URL=http://127.0.0.1:80 \
//	  go test ./internal/proxy/caddy/ -run RealCaddy_WAF -v
//
// The application is a server in this process, so Caddy has to be able to reach it:
// CADDY_UPSTREAM_HOST is the address Caddy dials to get here (default 127.0.0.1,
// which is right for a Caddy on the same host and wrong for one in a container).
// CADDY_STDERR_FILE, when set, is where that Caddy's stderr is captured; the
// assertions about its log lines run only then.

const wafAppHost = "waf-e2e.belune.local"

type wafRig struct {
	c       *Client
	httpURL string
	// seen is what the application received, by request path.
	seen chan seenRequest
}

type seenRequest struct {
	path    string
	bytes   int64
	txValue string
}

func newWAFRig(t *testing.T) *wafRig {
	t.Helper()
	c := liveClient(t)
	ctx := context.Background()

	rig := &wafRig{c: c, httpURL: os.Getenv("CADDY_HTTP_URL"), seen: make(chan seenRequest, 64)}
	if rig.httpURL == "" {
		rig.httpURL = "http://127.0.0.1:80"
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/sse", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fl := w.(http.Flusher)
		for i := 0; i < 4; i++ {
			_, _ = fmt.Fprintf(w, "data: %d\n\n", i)
			fl.Flush()
			time.Sleep(300 * time.Millisecond)
		}
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		n, _ := io.Copy(io.Discard, r.Body)
		rig.seen <- seenRequest{path: r.URL.Path, bytes: n, txValue: r.Header.Get(waf.TxHeader)}
		_, _ = io.WriteString(w, "ok")
	})
	app := httptest.NewServer(mux)
	t.Cleanup(app.Close)

	upstreamHost := os.Getenv("CADDY_UPSTREAM_HOST")
	if upstreamHost == "" {
		upstreamHost = "127.0.0.1"
	}
	u, err := url.Parse(app.URL)
	require.NoError(t, err)

	c.InitCatchAll(ctx)
	// Belune's own logging config, not Caddy's default: the access logger is excluded
	// from the default log, and the WAF's rule matches have to keep reaching it —
	// that stream is what the recorder reads.
	require.NoError(t, c.ConfigureAccessLogs(ctx))
	require.NoError(t, c.AddRoute(ctx, proxy.RouteConfig{
		Hostname: wafAppHost, TargetURL: "http://" + upstreamHost + ":" + u.Port(), SSLMode: proxy.SSLModeOff,
	}))
	t.Cleanup(func() {
		_ = c.SyncWAF(ctx, proxy.WAFConfig{})
		_ = c.RemoveRoute(ctx, wafAppHost, "/")
	})
	return rig
}

func (r *wafRig) do(t *testing.T, method, path string, body io.Reader, header http.Header) (*http.Response, error) {
	t.Helper()
	req, err := http.NewRequest(method, r.httpURL+path, body)
	require.NoError(t, err)
	req.Host = wafAppHost
	for k, v := range header {
		req.Header[k] = v
	}
	return (&http.Client{Timeout: 30 * time.Second}).Do(req)
}

func (r *wafRig) nextSeen(t *testing.T) seenRequest {
	t.Helper()
	select {
	case s := <-r.seen:
		return s
	case <-time.After(10 * time.Second):
		t.Fatal("the application never received the request")
		return seenRequest{}
	}
}

func TestRealCaddy_WAF_AcceptsTheRouteAndKeepsItFirst(t *testing.T) {
	rig := newWAFRig(t)
	ctx := context.Background()

	require.NoError(t, rig.c.SyncWAF(ctx, proxy.WAFConfig{Enabled: true, Hosts: []string{wafAppHost}}),
		"Caddy refused the WAF route: the module or the directives did not load")

	routes, err := rig.c.fetchRawRoutes(ctx)
	require.NoError(t, err)
	var first struct {
		ID string `json:"@id"`
	}
	require.NoError(t, json.Unmarshal(routes[0], &first))
	assert.Equal(t, wafRouteID, first.ID, "it has to run before every application route")

	// An unrelated route change reorders the array; the WAF route must still be first.
	require.NoError(t, rig.c.reorderRoutes(ctx))
	routes, _ = rig.c.fetchRawRoutes(ctx)
	require.NoError(t, json.Unmarshal(routes[0], &first))
	assert.Equal(t, wafRouteID, first.ID)

	// And Caddy agrees a second sync has nothing to do.
	listed, err := rig.c.ListRoutes(ctx)
	require.NoError(t, err)
	for _, r := range listed {
		assert.NotEmpty(t, r.TargetURL, "an application route; the WAF route must not be listed as one")
	}
}

// The reason the directives are what they are. Detection-only must not be able to
// break an application, and measured against the module's own defaults it can.
func TestRealCaddy_WAF_CannotHurtTheApplicationBehindIt(t *testing.T) {
	rig := newWAFRig(t)
	ctx := context.Background()
	require.NoError(t, rig.c.SyncWAF(ctx, proxy.WAFConfig{Enabled: true, Hosts: []string{wafAppHost}}))

	t.Run("an attack is detected, never blocked", func(t *testing.T) {
		resp, err := rig.do(t, "GET", "/search?q=%3Cscript%3Ealert(1)%3C/script%3E&id=1%27%20OR%20%271%27=%271", nil, nil)
		require.NoError(t, err)
		resp.Body.Close()
		assert.Equal(t, http.StatusOK, resp.StatusCode)
		rig.nextSeen(t)
	})

	t.Run("bodies of every size arrive whole", func(t *testing.T) {
		// Over about 1 MiB a request body was measured to stall and arrive truncated
		// when the module was left on its defaults, even in DetectionOnly. This fails
		// with the module's own 12.5 MiB body limit and no body-gating rules (checked
		// by editing wafDirectives), and passes with either protection on its own.
		for _, size := range []int64{1 << 10, 100 << 10, 129 << 10, 2 << 20, 20 << 20} {
			resp, err := rig.do(t, "POST", "/upload", io.LimitReader(zeroReader{}, size),
				http.Header{"Content-Type": {"application/x-www-form-urlencoded"}})
			require.NoError(t, err, "a %d byte body", size)
			resp.Body.Close()
			assert.Equal(t, http.StatusOK, resp.StatusCode, "%d bytes", size)
			assert.Equal(t, size, rig.nextSeen(t).bytes, "the application received all %d bytes", size)
		}
	})

	t.Run("a chunked body of unknown length arrives whole", func(t *testing.T) {
		const size = 5 << 20
		req, err := http.NewRequest("POST", rig.httpURL+"/upload", io.LimitReader(zeroReader{}, size))
		require.NoError(t, err)
		req.Host = wafAppHost
		req.ContentLength = -1 // chunked
		resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
		require.NoError(t, err)
		resp.Body.Close()
		assert.EqualValues(t, size, rig.nextSeen(t).bytes)
	})

	t.Run("a stream is not buffered", func(t *testing.T) {
		resp, err := rig.do(t, "GET", "/sse", nil, nil)
		require.NoError(t, err)
		defer resp.Body.Close()

		start := time.Now()
		sc := bufio.NewScanner(resp.Body)
		var arrivals []time.Duration
		for sc.Scan() {
			if strings.HasPrefix(sc.Text(), "data:") {
				arrivals = append(arrivals, time.Since(start))
			}
		}
		require.Len(t, arrivals, 4)
		assert.Less(t, arrivals[0], 250*time.Millisecond, "the first event is not held back until the stream ends")
		assert.Greater(t, arrivals[3]-arrivals[0], 600*time.Millisecond, "events arrive spread out, as they were sent")
	})

	t.Run("the internal header never reaches the application, and cannot be supplied", func(t *testing.T) {
		resp, err := rig.do(t, "GET", "/headers", nil, http.Header{waf.TxHeader: {"spoofed"}})
		require.NoError(t, err)
		resp.Body.Close()
		assert.Empty(t, rig.nextSeen(t).txValue)
	})
}

// With Caddy's stderr captured, the rule-match lines are checked against the code
// that was written to read them.
func TestRealCaddy_WAF_RealLinesParseAndRedact(t *testing.T) {
	stderrFile := os.Getenv("CADDY_STDERR_FILE")
	if stderrFile == "" {
		t.Skip("set CADDY_STDERR_FILE to the Caddy's captured stderr to check its log lines")
	}
	rig := newWAFRig(t)
	ctx := context.Background()
	require.NoError(t, rig.c.SyncWAF(ctx, proxy.WAFConfig{Enabled: true, Hosts: []string{wafAppHost}}))

	offset := func() int64 {
		fi, err := os.Stat(stderrFile)
		require.NoError(t, err)
		return fi.Size()
	}
	before := offset()

	// Two ways a secret reaches Caddy's line. Every line carries the request URI, so a
	// token in the query string is in all of them. And [data] carries the matched
	// value — only the variable that tripped the rule, not the whole body — so a secret
	// is there when the field that matched contains it.
	const secret = "hunter2-REAL-PASSWORD"
	resp, err := rig.do(t, "POST", "/login?next=%3Cscript%3Ealert(1)%3C/script%3E&token=s3cr3t-in-the-uri",
		strings.NewReader("password=not-matched-so-not-logged&x=<script>alert('"+secret+"')</script>"),
		http.Header{"Content-Type": {"application/x-www-form-urlencoded"}})
	require.NoError(t, err)
	resp.Body.Close()
	rig.nextSeen(t)

	var raw []byte
	require.Eventually(t, func() bool {
		f, err := os.Open(stderrFile)
		if err != nil {
			return false
		}
		defer func() { _ = f.Close() }()
		_, _ = f.Seek(before, io.SeekStart)
		raw, _ = io.ReadAll(f)
		return bytes.Contains(raw, []byte("Coraza: "))
	}, 10*time.Second, 200*time.Millisecond, "the WAF logged nothing for an XSS payload")

	var events []waf.Event
	for _, l := range strings.Split(string(raw), "\n") {
		ev, out := waf.ParseCaddyLine(l)
		require.NotEqual(t, waf.Malformed, out, "Caddy wrote a rule match the parser cannot read: %.300s", l)
		if out == waf.Matched {
			events = append(events, ev)
		}
	}
	require.NotEmpty(t, events)

	// Real output, real attribution: the host rode in the transaction id.
	for _, ev := range events {
		assert.Equal(t, wafAppHost, ev.Host, "the host the request asked for is in the line Caddy wrote")
		assert.NotEmpty(t, ev.TxID)
		assert.True(t, ev.ClientIP.IsValid())
	}

	// The line has to stay in stderr for the recorder, so what protects an admin is
	// the redaction — and it has to hold on what Caddy actually wrote.
	require.Contains(t, string(raw), secret, "the raw line carries the matched payload, which is why it must be redacted before an admin sees it")
	require.Contains(t, string(raw), "s3cr3t-in-the-uri", "...and the URI")
	assert.NotContains(t, string(raw), "not-matched-so-not-logged", "a field that tripped no rule is not in the line")
	redacted := waf.RedactLogs(string(raw))
	for _, leak := range []string{secret, "s3cr3t-in-the-uri", "<script", "alert(", "[data", "[uri"} {
		assert.NotContains(t, redacted, leak)
	}
	assert.Contains(t, redacted, "payload and URI withheld")
}

type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 'A'
	}
	return len(p), nil
}

// Caddy parses the directives when it provisions the module, so a successful
// SyncWAF IS the proof that every shape an exclusion can take parses: a line it
// refused would come back as an error and leave the previous route in place.
// Needs only the admin API (CADDY_ADMIN_URL), not a reachable upstream.
//
//	BELUNE_CADDY_INTEGRATION=1 CADDY_ADMIN_URL=http://127.0.0.1:12020 \
//	  go test ./internal/proxy/caddy/ -run RealCaddy_WAF_AcceptsEveryExclusion -v
func TestRealCaddy_WAF_AcceptsEveryExclusionShape(t *testing.T) {
	c := liveClient(t)
	ctx := context.Background()
	c.InitCatchAll(ctx)
	t.Cleanup(func() { _ = c.SyncWAF(ctx, proxy.WAFConfig{}) })

	exclusions := []proxy.WAFExclusion{
		withTarget(ex(1, 942290, true), "REQUEST_COOKIES", "ph_phc_x_posthog"),
		withTarget(ex(2, 941100, false, "a.example.com", "B.example.com"), "ARGS", "q"),
		withTarget(ex(3, 941100, true), "ARGS_NAMES", "q"),
		withTarget(ex(4, 920270, true), "REQUEST_HEADERS", "X-Custom-Thing"),
		withPath(ex(5, 930130, true), "/.env.local"),
		withPath(ex(6, 930130, false, "a.example.com"), "/api/*"),
		ex(7, 941110, true),
		ex(8, 941110, false, "*.example.com"),
	}
	cfg := proxy.WAFConfig{Enabled: true, Hosts: []string{"a.example.com", "b.example.com"}, Exclusions: exclusions}

	require.NoError(t, c.SyncWAF(ctx, cfg), "Caddy refused the rendered exclusions")
	route, found, err := c.fetchRouteByID(ctx, wafRouteID)
	require.NoError(t, err)
	require.True(t, found)
	assert.Contains(t, string(route), "ruleRemoveTargetById=942290;REQUEST_COOKIES:ph_phc_x_posthog")

	// The same set again is a read: a write is a reload.
	same, err := routeJSONEqual(route, buildWAFRoute(wafHosts(cfg.Hosts), exclusions, wafBlockingHosts(wafHosts(cfg.Hosts), cfg.BlockingHosts)))
	require.NoError(t, err)
	assert.True(t, same, "the route Caddy holds differs from what would be written again, so every pass would reload")

	// Removing them all goes back to what shipped before exclusions existed.
	cfg.Exclusions = nil
	require.NoError(t, c.SyncWAF(ctx, cfg))
	route, _, err = c.fetchRouteByID(ctx, wafRouteID)
	require.NoError(t, err)
	assert.NotContains(t, string(route), "ruleRemoveTargetById")
}
