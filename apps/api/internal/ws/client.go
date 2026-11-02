package ws

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)

const (
	writeWait   = 10 * time.Second
	pongWait    = 60 * time.Second
	pingPeriod  = 30 * time.Second
	sendBufSize = 64
)

// Client represents a single WebSocket connection.
type Client struct {
	hub              *Hub
	conn             *websocket.Conn
	userID           string
	send             chan OutboundMessage
	subscriptions    map[string]struct{}
	consecutiveDrops int // incremented by hub broadcast loop; reset on successful send
}

// NewClient creates a new WebSocket client.
func NewClient(hub *Hub, conn *websocket.Conn, userID string) *Client {
	return &Client{
		hub:           hub,
		conn:          conn,
		userID:        userID,
		send:          make(chan OutboundMessage, sendBufSize),
		subscriptions: make(map[string]struct{}),
	}
}

// ReadPump reads messages from the WebSocket connection.
// It handles subscribe/unsubscribe actions from the client. A nil authz refuses
// every subscribe: a socket with nobody deciding who may hear what must not
// default to everyone.
func (c *Client) ReadPump(ctx context.Context, authz ChannelAuthorizer) {
	defer func() {
		c.hub.Unregister(c)
		c.conn.Close(websocket.StatusNormalClosure, "")
	}()

	for {
		var msg InboundMessage
		err := wsjson.Read(ctx, c.conn, &msg)
		if err != nil {
			if websocket.CloseStatus(err) == websocket.StatusNormalClosure ||
				websocket.CloseStatus(err) == websocket.StatusGoingAway {
				return
			}
			slog.Debug("ws: read error", "user", c.userID, "error", err)
			return
		}

		switch msg.Action {
		case "subscribe":
			if msg.Channel != "" {
				c.subscribe(ctx, authz, msg.Channel)
			}
		case "unsubscribe":
			if msg.Channel != "" {
				c.hub.Unsubscribe(c, msg.Channel)
				slog.Debug("ws: client unsubscribed", "user", c.userID, "channel", msg.Channel)
			}
		default:
			slog.Debug("ws: unknown action", "action", msg.Action)
		}
	}
}

// subscribe authorizes a subscription, then registers it.
//
// The check lives here, once per subscription, and not in Hub.Broadcast: that
// runs per message per channel (metrics:host at 1 Hz would cost a query per
// frame). It also has to come before Hub.Subscribe rather than after, because a
// subscriber is what makes ActiveChannelsWithPrefix start on-demand collection —
// an unauthorized metrics:app:{id} would still cost a Docker stats call every 2s.
func (c *Client) subscribe(ctx context.Context, authz ChannelAuthorizer, channel string) {
	if authz == nil || !authz.AuthorizeChannel(ctx, channel) {
		c.refuse(ctx, channel)
		// Debug, not Warn: a refusal is the expected answer to a client asking
		// for something it may not hear, and nothing rate-limits a subscribe —
		// a client looping them would otherwise fill the operator's logs with
		// warnings about a system that is working correctly. The client is told
		// on the channel; that is where the answer belongs.
		slog.Debug("ws: subscription refused", "user", c.userID, "channel", clipForLog(channel))
		return
	}
	c.hub.Subscribe(c, channel)
	slog.Debug("ws: client subscribed", "user", c.userID, "channel", channel)
}

// accessDenied is the body of every refusal. The text is the same for a
// forbidden resource and a nonexistent one, so a refusal is not a way to probe
// what exists.
var accessDenied = json.RawMessage(`{"error":"access denied"}`)

// refuse answers a refused subscribe on the channel that was asked for. It is
// answered rather than swallowed because a client that believes it subscribed
// and hears nothing looks exactly like a quiet channel — which is how a broken
// subscription stays invisible.
//
// It writes to the connection directly instead of going through c.send (as
// SendJSON does): the hub closes that channel when it drops a slow client or
// shuts down, while ReadPump can still be mid-frame, and a send on a closed
// channel panics. Conn writes are safe alongside WritePump's.
func (c *Client) refuse(ctx context.Context, channel string) {
	writeCtx, cancel := context.WithTimeout(ctx, writeWait)
	defer cancel()
	msg := OutboundMessage{Channel: channel, Event: "error", Data: accessDenied}
	if err := wsjson.Write(writeCtx, c.conn, msg); err != nil {
		slog.Debug("ws: could not send refusal", "user", c.userID, "error", err)
	}
}

// clipForLog bounds a client-supplied string before it reaches a log line — a
// channel name is arbitrary text up to the frame limit.
func clipForLog(s string) string {
	const max = 80
	if len(s) > max {
		return s[:max] + "…"
	}
	return s
}

// WritePump writes messages to the WebSocket connection.
func (c *Client) WritePump(ctx context.Context) {
	pingTicker := time.NewTicker(pingPeriod)
	defer func() {
		pingTicker.Stop()
		c.conn.Close(websocket.StatusNormalClosure, "")
	}()

	for {
		select {
		case <-ctx.Done():
			return

		case msg, ok := <-c.send:
			if !ok {
				// Hub closed the channel.
				c.conn.Close(websocket.StatusNormalClosure, "")
				return
			}
			writeCtx, cancel := context.WithTimeout(ctx, writeWait)
			err := wsjson.Write(writeCtx, c.conn, msg)
			cancel()
			if err != nil {
				slog.Debug("ws: write error", "user", c.userID, "error", err)
				return
			}

		case <-pingTicker.C:
			pingCtx, cancel := context.WithTimeout(ctx, writeWait)
			err := c.conn.Ping(pingCtx)
			cancel()
			if err != nil {
				slog.Debug("ws: ping failed", "user", c.userID, "error", err)
				return
			}
		}
	}
}

// SendJSON sends a JSON message to this specific client.
func (c *Client) SendJSON(channel, event string, data any) {
	raw, err := json.Marshal(data)
	if err != nil {
		return
	}
	select {
	case c.send <- OutboundMessage{Channel: channel, Event: event, Data: raw}:
	default:
	}
}
