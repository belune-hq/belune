package handler_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"nhooyr.io/websocket"
	"nhooyr.io/websocket/wsjson"

	"github.com/belune-hq/belune/internal/runtime"
	"github.com/belune-hq/belune/internal/server"
	"github.com/belune-hq/belune/internal/terminal"
	"github.com/belune-hq/belune/internal/testutil"
	"github.com/belune-hq/belune/internal/ws"
)

// wsHarness is an API server with a live hub — the shared test env is built with
// a nil hub, so its /api/ws answers 503 — plus a handle on that hub, so a test
// can inspect who is subscribed and publish onto a channel itself. One per
// persona, so a subscriber left over from another socket cannot make a channel
// look subscribed.
type wsHarness struct {
	hub    *ws.Hub
	url    string // ws:// URL of /api/ws
	origin string // what a browser served by this server would send as Origin
}

func startWSHarness(t *testing.T) *wsHarness {
	t.Helper()
	hub := ws.NewHub(50)
	ctx, cancel := context.WithCancel(context.Background())
	go hub.Run(ctx)

	srv := server.New(env.Config, env.Pool, env.Queries, env.Asynq, env.Inspector,
		runtime.NewLocalRuntimes(env.Runtime), env.Proxy, env.Reconciler, env.Redis,
		hub, nil, nil, terminal.NewManager(2), nil)
	ts := httptest.NewServer(srv.Router())
	t.Cleanup(func() {
		ts.Close()
		cancel()
	})
	return &wsHarness{
		hub:    hub,
		url:    "ws" + strings.TrimPrefix(ts.URL, "http") + "/api/ws",
		origin: ts.URL,
	}
}

func (h *wsHarness) dial(t *testing.T, header http.Header) *websocket.Conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, resp, err := websocket.Dial(ctx, h.url, &websocket.DialOptions{HTTPHeader: header})
	require.NoError(t, err, "the handshake must succeed — authorization is per channel, not per socket")
	if resp != nil && resp.Body != nil {
		resp.Body.Close()
	}
	t.Cleanup(func() { conn.Close(websocket.StatusNormalClosure, "") })
	return conn
}

func bearer(token string) http.Header {
	return http.Header{"Authorization": []string{"Bearer " + token}}
}

func wsSend(t *testing.T, conn *websocket.Conn, action, channel string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	require.NoError(t, wsjson.Write(ctx, conn, ws.InboundMessage{Action: action, Channel: channel}))
}

func wsRead(t *testing.T, conn *websocket.Conn, why string) ws.OutboundMessage {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	var msg ws.OutboundMessage
	require.NoError(t, wsjson.Read(ctx, conn, &msg), why)
	return msg
}

// wsRefused asserts the server answered a subscribe with an error frame on that
// channel and did not register the client. Both halves matter: a test that only
// waits for silence passes just as happily when the socket is broken.
func (h *wsHarness) wsRefused(t *testing.T, conn *websocket.Conn, channel string) {
	t.Helper()
	wsSend(t, conn, "subscribe", channel)
	msg := wsRead(t, conn, "a refused subscribe must be answered, not swallowed: "+channel)
	assert.Equal(t, channel, msg.Channel)
	assert.Equal(t, "error", msg.Event, "expected a refusal frame for %s, got %s", channel, msg.Event)
	var body map[string]string
	require.NoError(t, json.Unmarshal(msg.Data, &body))
	assert.Equal(t, "access denied", body["error"])
	assert.Zero(t, h.hub.SubscriberCount(channel), "a refused client must not be a subscriber of %s", channel)
}

// wsAccepted asserts the subscribe took effect and that a frame published to the
// channel reaches this socket.
func (h *wsHarness) wsAccepted(t *testing.T, conn *websocket.Conn, channel string) {
	t.Helper()
	wsSend(t, conn, "subscribe", channel)
	require.Eventually(t, func() bool { return h.hub.SubscriberCount(channel) == 1 },
		2*time.Second, 5*time.Millisecond, "expected to be subscribed to %s", channel)
	h.hub.Broadcast(channel, "probe", json.RawMessage(`null`))
	msg := wsRead(t, conn, "an accepted subscription must receive frames: "+channel)
	assert.Equal(t, channel, msg.Channel)
	assert.Equal(t, "probe", msg.Event)
}

// wsResources is one project's worth of everything a channel can name.
type wsResources struct {
	app, db, deployment string
}

func (r wsResources) channels() []string {
	return []string{
		"requests:" + r.app,
		"metrics:app:" + r.app,
		"container-status:" + r.app,
		"container-logs:" + r.app,
		"database-status:" + r.db,
		"container-logs:" + r.db,
		"build-logs:" + r.deployment,
	}
}

func seedWSResources(t *testing.T, token, projectID string) wsResources {
	t.Helper()
	app := extractID(minimalApp(t, token, projectID)["id"])

	resp := env.DoRequest(t, "POST", fmt.Sprintf("/api/projects/%s/databases", projectID),
		map[string]any{"name": "db", "type": "postgres"}, testutil.AuthHeader(token))
	require.Equal(t, http.StatusAccepted, resp.StatusCode)
	db := extractID(testutil.ReadJSON(t, resp)["id"])

	return wsResources{app: app, db: db, deployment: seedBuildLog(t, app, "build output")}
}

// wsWorld is three projects with different owners, so every persona has one
// project it may reach, one it may not, and one that is open to everybody:
//
//	private  — owned by "owner", not shared
//	shared   — owned by "owner", shared with every member
//	stranger — owned by "stranger", not shared
type wsWorld struct {
	adminToken, ownerToken, strangerToken string
	adminID, ownerID, strangerID          string
	privateID, sharedID, strangerProjID   string
	private, shared, strangers            wsResources
}

func newWSWorld(t *testing.T) *wsWorld {
	t.Helper()
	resetDB(t)
	w := &wsWorld{}
	w.adminToken = env.SetupAdmin(t, "admin@test.com", "password123")
	w.adminID = extractID(mustAuthMe(t, w.adminToken)["id"])
	w.ownerID, w.ownerToken = createMember(t, w.adminToken, "owner@test.com")
	w.strangerID, w.strangerToken = createMember(t, w.adminToken, "stranger@test.com")

	w.privateID = extractID(env.CreateProject(t, w.ownerToken, "Private", "private")["id"])
	w.sharedID = extractID(env.CreateProject(t, w.ownerToken, "Shared", "shared")["id"])
	resp := env.DoRequest(t, "PUT", fmt.Sprintf("/api/projects/%s/sharing", w.sharedID),
		map[string]bool{"shared": true}, testutil.AuthHeader(w.ownerToken))
	require.Equal(t, http.StatusOK, resp.StatusCode)
	resp.Body.Close()
	w.strangerProjID = extractID(env.CreateProject(t, w.strangerToken, "Strangers", "strangers")["id"])

	w.private = seedWSResources(t, w.ownerToken, w.privateID)
	w.shared = seedWSResources(t, w.ownerToken, w.sharedID)
	w.strangers = seedWSResources(t, w.strangerToken, w.strangerProjID)
	return w
}

// TestWebSocket_ChannelAuthorization is the whole matrix: every channel kind, for
// every kind of caller. What a caller may hear is (their role) ∩ (project
// ownership/sharing) ∩ (their token's project pin), and each row below exercises
// one corner of that.
func TestWebSocket_ChannelAuthorization(t *testing.T) {
	w := newWSWorld(t)

	// Unpinned read-scoped PATs — what a script or CI job holds.
	ownerPAT := mintScoped(t, w.ownerToken, []string{"read"})
	adminPAT := mintScoped(t, w.adminToken, []string{"read"})

	personas := []struct {
		name   string
		header http.Header
		// which project's channels the caller may subscribe to
		private, shared, strangers bool
		hostMetrics, allRequests   bool
	}{
		{"admin session", bearer(w.adminToken), true, true, true, true, true},
		{"member owner", bearer(w.ownerToken), true, true, false, false, false},
		{"member with no access to a private project", bearer(w.strangerToken), false, true, true, false, false},
		{"read-scoped member PAT", bearer(ownerPAT), true, true, false, false, false},
		{"read-scoped admin PAT, unpinned", bearer(adminPAT), true, true, true, true, true},

		// A pin narrows a token to its projects. Admin-owned, so ownership never
		// interferes: any refusal below is the pin's doing.
		{"admin PAT pinned to the private project",
			bearer(createPinnedAPIToken(t, w.adminID, w.privateID, []string{"read"})),
			true, false, false, true, false},
		// Non-nil and empty: pinned, to nothing. Collapsing this into "unpinned"
		// is the escalation from none to everything.
		{"admin PAT pinned to nothing",
			bearer(createMultiPinnedAPIToken(t, w.adminID, []string{}, []string{"read"})),
			false, false, false, true, false},

		// A pin and ownership are separate gates and both must pass.
		{"member owner PAT pinned to the private project",
			bearer(createPinnedAPIToken(t, w.ownerID, w.privateID, []string{"read"})),
			true, false, false, false, false},
		{"member owner PAT pinned to a project it does not own",
			bearer(createPinnedAPIToken(t, w.ownerID, w.strangerProjID, []string{"read"})),
			false, false, false, false, false},
		{"member PAT pinned to the shared project",
			bearer(createPinnedAPIToken(t, w.strangerID, w.sharedID, []string{"read"})),
			false, true, false, false, false},
	}

	for _, p := range personas {
		t.Run(p.name, func(t *testing.T) {
			h := startWSHarness(t)
			conn := h.dial(t, p.header)

			check := func(res wsResources, allowed bool) {
				for _, ch := range res.channels() {
					if allowed {
						h.wsAccepted(t, conn, ch)
					} else {
						h.wsRefused(t, conn, ch)
					}
				}
			}
			check(w.private, p.private)
			check(w.shared, p.shared)
			check(w.strangers, p.strangers)

			for ch, allowed := range map[string]bool{
				"metrics:host": p.hostMetrics,
				"requests:all": p.allRequests,
			} {
				if allowed {
					h.wsAccepted(t, conn, ch)
				} else {
					h.wsRefused(t, conn, ch)
				}
			}
		})
	}
}

// The property that protects channels nobody has written yet: a channel the
// authorizer does not recognise is refused, even for an admin — the most
// privileged caller is the one a fail-open default would serve.
func TestWebSocket_UnknownChannelsAreDenied(t *testing.T) {
	w := newWSWorld(t)
	app := w.private.app

	unknown := []string{
		"totally:unknown",
		"terminal:anything",
		"metrics:",
		"metrics:app:",
		"metrics:app:not-a-uuid",
		"metrics:app:" + app + ":extra",
		"metrics:app:00000000-0000-0000-0000-000000000000",
		"metrics:host:extra",
		"METRICS:HOST",
		"metrics:host ",
		" requests:all",
		// Redis-internal names the adapters consume, never a WebSocket channel.
		"requests:live:" + app,
		"host:metrics:live",
		// Right prefix, wrong kind of id: an application id is not a deployment,
		// and neither is a database.
		"build-logs:" + app,
		"database-status:" + app,
		"container-status:" + w.private.db,
		// Neither an application nor a database.
		"container-logs:" + w.private.deployment,
		"container-logs:00000000-0000-0000-0000-000000000000",
		// pgtype would parse these as the same id; the hub would treat each as
		// its own channel.
		"container-status:" + strings.ToUpper(app),
		"container-status:" + strings.ReplaceAll(app, "-", ""),
		// The open channel is matched exactly: near-misses stay fail-closed, or
		// "open" would quietly become a prefix anyone could widen.
		"platform:",
		"platform:logs",
		"Platform",
		"platform ",
	}

	for name, header := range map[string]http.Header{
		"admin":  bearer(w.adminToken),
		"member": bearer(w.ownerToken),
	} {
		t.Run(name, func(t *testing.T) {
			h := startWSHarness(t)
			conn := h.dial(t, header)
			for _, ch := range unknown {
				h.wsRefused(t, conn, ch)
			}
		})
	}
}

// "platform" announces that an update is underway to EVERY client, so unlike the
// admin channels it must open for a Member, a PAT, and a project-pinned PAT —
// a pin narrows what projects a token reaches, and this is not project data.
// Pairs with TestWebSocket_UnknownChannelsAreDenied, which pins the other half:
// being open must not have loosened the fail-closed default.
func TestWebSocket_PlatformChannelIsOpenToEveryAuthenticatedCaller(t *testing.T) {
	w := newWSWorld(t)

	for name, header := range map[string]http.Header{
		"admin":                bearer(w.adminToken),
		"member":               bearer(w.strangerToken),
		"read-scoped member":   bearer(mintScoped(t, w.strangerToken, []string{"read"})),
		"member pinned":        bearer(createPinnedAPIToken(t, w.strangerID, w.strangerProjID, []string{"read"})),
		"admin pinned":         bearer(createPinnedAPIToken(t, w.adminID, w.privateID, []string{"read"})),
		"admin pinned nothing": bearer(createMultiPinnedAPIToken(t, w.adminID, []string{}, []string{"read"})),
	} {
		t.Run(name, func(t *testing.T) {
			h := startWSHarness(t)
			conn := h.dial(t, header)
			h.wsAccepted(t, conn, "platform")
		})
	}
}

// The reported exploit, end to end: a read-scoped PAT belonging to a Member, and
// a browser session of the same Member. The REST twins refuse both — the socket
// has to say the same, and has to actually withhold the frames.
func TestWebSocket_MemberCannotReadAdminChannelsOrOthersLogs(t *testing.T) {
	w := newWSWorld(t)
	pat := mintScoped(t, w.strangerToken, []string{"read"})

	// What the same token is told over REST, at the same moment.
	for _, path := range []string{"/api/requests", "/api/metrics/host", "/api/projects/" + w.privateID} {
		resp := env.DoRequest(t, "GET", path, nil, testutil.AuthHeader(pat))
		assert.Equal(t, http.StatusForbidden, resp.StatusCode, "REST %s", path)
		resp.Body.Close()
	}

	otherUsersApp := "container-logs:" + w.private.app
	for name, header := range map[string]func(*wsHarness) http.Header{
		"read-scoped PAT": func(*wsHarness) http.Header { return bearer(pat) },
		// What a Member's browser sends when it opens /server by URL: the session
		// cookie, and the page's own origin.
		"browser session cookie": func(h *wsHarness) http.Header {
			return http.Header{
				"Cookie": []string{"token=" + w.strangerToken},
				"Origin": []string{h.origin},
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			h := startWSHarness(t)
			conn := h.dial(t, header(h))

			for _, ch := range []string{"metrics:host", "requests:all", otherUsersApp} {
				h.wsRefused(t, conn, ch)
			}

			// Prove nothing leaked without leaning on a timeout: the hub delivers in
			// order, so if either frame below had reached this socket it would arrive
			// ahead of the one sentinel a subscription this caller IS entitled to.
			own := "container-status:" + w.strangers.app
			h.wsAccepted(t, conn, own)
			h.hub.Broadcast("metrics:host", "metrics", json.RawMessage(`{"cpu_percent":99}`))
			h.hub.Broadcast(otherUsersApp, "log", json.RawMessage(`{"line":"secret"}`))
			h.hub.Broadcast(own, "sentinel", json.RawMessage(`null`))
			msg := wsRead(t, conn, "the sentinel must arrive")
			assert.Equal(t, "sentinel", msg.Event, "a frame from a refused channel reached the socket first: %s %s", msg.Channel, msg.Event)
		})
	}
}
