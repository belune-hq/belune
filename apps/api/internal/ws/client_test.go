package ws

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type authorizerFunc func(ctx context.Context, channel string) bool

func (f authorizerFunc) AuthorizeChannel(ctx context.Context, channel string) bool {
	return f(ctx, channel)
}

type handshakeKey struct{}

// dialReadPump serves a real ReadPump/WritePump pair behind an httptest server
// and returns the client end of the socket. handshakeCtx stands in for the
// request context Auth decorates in production.
func dialReadPump(t *testing.T, hub *Hub, authz ChannelAuthorizer) *websocket.Conn {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		client := NewClient(hub, conn, "user-1")
		if !hub.Register(client) {
			conn.Close(websocket.StatusTryAgainLater, "")
			return
		}
		ctx := context.WithValue(r.Context(), handshakeKey{}, "from-handshake")
		go client.WritePump(ctx)
		client.ReadPump(ctx, authz)
	}))
	t.Cleanup(srv.Close)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	require.NoError(t, err)
	t.Cleanup(func() { conn.Close(websocket.StatusNormalClosure, "") })
	return conn
}

func sendAction(t *testing.T, conn *websocket.Conn, action, channel string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	require.NoError(t, wsjson.Write(ctx, conn, InboundMessage{Action: action, Channel: channel}))
}

func readFrame(t *testing.T, conn *websocket.Conn) OutboundMessage {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	var msg OutboundMessage
	require.NoError(t, wsjson.Read(ctx, conn, &msg), "expected a frame from the server")
	return msg
}

// A refusal has to be a frame the client can act on. Silence is also what a
// healthy quiet channel looks like, so it would hide the very bug being fixed.
func assertRefused(t *testing.T, conn *websocket.Conn, channel string) {
	t.Helper()
	msg := readFrame(t, conn)
	assert.Equal(t, channel, msg.Channel)
	assert.Equal(t, "error", msg.Event)
	var body map[string]string
	require.NoError(t, json.Unmarshal(msg.Data, &body))
	assert.Equal(t, "access denied", body["error"])
}

func TestReadPump_NilAuthorizerRefusesEverySubscribe(t *testing.T) {
	hub, cancel := startHub(t)
	defer cancel()
	conn := dialReadPump(t, hub, nil)

	sendAction(t, conn, "subscribe", "metrics:host")

	assertRefused(t, conn, "metrics:host")
	assert.Equal(t, 0, hub.SubscriberCount("metrics:host"))
}

func TestReadPump_RefusedSubscribeIsAnsweredAndNotRegistered(t *testing.T) {
	hub, cancel := startHub(t)
	defer cancel()
	conn := dialReadPump(t, hub, authorizerFunc(func(context.Context, string) bool { return false }))

	sendAction(t, conn, "subscribe", "metrics:app:11111111-1111-1111-1111-111111111111")

	assertRefused(t, conn, "metrics:app:11111111-1111-1111-1111-111111111111")
	assert.Equal(t, 0, hub.SubscriberCount("metrics:app:11111111-1111-1111-1111-111111111111"))
	// ActiveChannelsWithPrefix is what starts on-demand collection (a Docker
	// stats call every 2s). A refused subscriber must never become an input to it.
	assert.Empty(t, hub.ActiveChannelsWithPrefix("metrics:app:"))
}

func TestReadPump_AuthorizedSubscribeReceivesBroadcasts(t *testing.T) {
	hub, cancel := startHub(t)
	defer cancel()
	conn := dialReadPump(t, hub, authorizerFunc(func(context.Context, string) bool { return true }))

	sendAction(t, conn, "subscribe", "container-status:app-1")
	require.Eventually(t, func() bool { return hub.SubscriberCount("container-status:app-1") == 1 }, 2*time.Second, 5*time.Millisecond)

	hub.Broadcast("container-status:app-1", "status", json.RawMessage(`{"status":"running"}`))

	msg := readFrame(t, conn)
	assert.Equal(t, "container-status:app-1", msg.Channel)
	assert.Equal(t, "status", msg.Event)
}

// The authorizer is decided per channel, and one refusal must not cost the
// client its socket or its other subscriptions.
func TestReadPump_RefusalIsPerChannelAndKeepsTheSocketOpen(t *testing.T) {
	hub, cancel := startHub(t)
	defer cancel()
	conn := dialReadPump(t, hub, authorizerFunc(func(_ context.Context, channel string) bool {
		return channel == "allowed"
	}))

	sendAction(t, conn, "subscribe", "denied")
	assertRefused(t, conn, "denied")

	sendAction(t, conn, "subscribe", "allowed")
	require.Eventually(t, func() bool { return hub.SubscriberCount("allowed") == 1 }, 2*time.Second, 5*time.Millisecond)
	assert.Equal(t, 0, hub.SubscriberCount("denied"))
}

// The authorizer decides from the handshake's context — that is how it sees the
// caller's role and token pin without the Client carrying them.
func TestReadPump_AuthorizerReceivesTheHandshakeContext(t *testing.T) {
	hub, cancel := startHub(t)
	defer cancel()
	seen := make(chan any, 1)
	conn := dialReadPump(t, hub, authorizerFunc(func(ctx context.Context, _ string) bool {
		seen <- ctx.Value(handshakeKey{})
		return true
	}))

	sendAction(t, conn, "subscribe", "any")

	select {
	case v := <-seen:
		assert.Equal(t, "from-handshake", v)
	case <-time.After(2 * time.Second):
		t.Fatal("authorizer was never consulted")
	}
}

// Leaving a channel is never a privilege: it only ever removes the caller.
func TestReadPump_UnsubscribeIsNotAuthorized(t *testing.T) {
	hub, cancel := startHub(t)
	defer cancel()
	var consulted atomic.Int32
	conn := dialReadPump(t, hub, authorizerFunc(func(context.Context, string) bool {
		consulted.Add(1)
		return true
	}))

	sendAction(t, conn, "subscribe", "ch")
	require.Eventually(t, func() bool { return hub.SubscriberCount("ch") == 1 }, 2*time.Second, 5*time.Millisecond)

	sendAction(t, conn, "unsubscribe", "ch")
	require.Eventually(t, func() bool { return hub.SubscriberCount("ch") == 0 }, 2*time.Second, 5*time.Millisecond)
	assert.EqualValues(t, 1, consulted.Load(), "only the subscribe should reach the authorizer")
}

// The hub closes a client's send channel when it drops a slow client or shuts
// down, but ReadPump keeps running until the connection dies — so a refusal can
// land after that close. It has to still reach the client, and it must not be a
// send on a closed channel.
func TestSubscribe_RefusalAfterTheHubClosedTheSendChannel(t *testing.T) {
	hub, cancel := startHub(t)
	defer cancel()

	clients := make(chan *Client, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		client := NewClient(hub, conn, "user-1")
		hub.Register(client)
		clients <- client
		// ReadPump alone, with no WritePump: it is what lets the connection
		// close cleanly, and nothing else touches c.send.
		client.ReadPump(r.Context(), nil)
	}))
	t.Cleanup(srv.Close)

	ctx, stop := context.WithTimeout(context.Background(), 5*time.Second)
	defer stop()
	conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	require.NoError(t, err)
	t.Cleanup(func() { conn.Close(websocket.StatusNormalClosure, "") })

	client := <-clients
	// Unregister is a no-op for a client the hub has not registered yet.
	require.Eventually(t, func() bool {
		hub.mu.RLock()
		defer hub.mu.RUnlock()
		_, ok := hub.clients[client]
		return ok
	}, 2*time.Second, 5*time.Millisecond)
	hub.Unregister(client)
	require.Eventually(t, func() bool {
		select {
		case _, open := <-client.send:
			return !open
		default:
			return false
		}
	}, 2*time.Second, 5*time.Millisecond, "the hub should have closed the send channel")

	require.NotPanics(t, func() { client.subscribe(ctx, nil, "metrics:host") })
	assertRefused(t, conn, "metrics:host")
}
