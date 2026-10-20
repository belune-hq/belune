package ws

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"

	"github.com/redis/go-redis/v9"

	"github.com/belune-hq/belune/internal/pkg/buildlog"
)

// RedisAdapter bridges Redis pub/sub channels to the WebSocket hub.
// Each adapter subscribes to a Redis channel (or pattern) and broadcasts
// received messages to the corresponding WebSocket channel.
type RedisAdapter struct {
	rdb *redis.Client
	hub *Hub
}

func NewRedisAdapter(rdb *redis.Client, hub *Hub) *RedisAdapter {
	return &RedisAdapter{rdb: rdb, hub: hub}
}

// RunBuildLogAdapter forwards build-logs:{deploymentID} messages. Payloads are
// NDJSON log entries; the end-of-stream sentinel is forwarded as a distinct
// "done" event so the frontend can stop and refetch.
func (a *RedisAdapter) RunBuildLogAdapter(ctx context.Context) {
	pubsub := a.rdb.PSubscribe(ctx, "build-logs:*")
	defer pubsub.Close()

	ch := pubsub.Channel()
	for {
		select {
		case <-ctx.Done():
			return
		case msg, ok := <-ch:
			if !ok {
				return
			}
			// Redis channel: "build-logs:{deploymentID}"
			// WS channel:    "build-logs:{deploymentID}"
			if msg.Payload == buildlog.DoneSentinel {
				a.hub.Broadcast(msg.Channel, "done", json.RawMessage(`null`))
				continue
			}
			// Payload is already a JSON log entry — forward it verbatim.
			a.hub.Broadcast(msg.Channel, "log", json.RawMessage(msg.Payload))
		}
	}
}

// RunHostMetricsAdapter forwards host:metrics:live messages.
func (a *RedisAdapter) RunHostMetricsAdapter(ctx context.Context) {
	pubsub := a.rdb.Subscribe(ctx, "host:metrics:live")
	defer pubsub.Close()

	ch := pubsub.Channel()
	for {
		select {
		case <-ctx.Done():
			return
		case msg, ok := <-ch:
			if !ok {
				return
			}
			a.hub.Broadcast("metrics:host", "metrics", json.RawMessage(msg.Payload))
		}
	}
}

// RunRequestLogAdapter forwards requests:live:{appID} messages.
func (a *RedisAdapter) RunRequestLogAdapter(ctx context.Context) {
	pubsub := a.rdb.PSubscribe(ctx, "requests:live:*")
	defer pubsub.Close()

	ch := pubsub.Channel()
	for {
		select {
		case <-ctx.Done():
			return
		case msg, ok := <-ch:
			if !ok {
				return
			}
			// Redis channel: "requests:live:{appID}"
			// WS channel:    "requests:{appID}"
			parts := strings.TrimPrefix(msg.Channel, "requests:live:")
			if parts != "" {
				wsChannel := "requests:" + parts
				a.hub.Broadcast(wsChannel, "request", json.RawMessage(msg.Payload))
			}
			// Also broadcast to global requests channel
			a.hub.Broadcast("requests:all", "request", json.RawMessage(msg.Payload))
		}
	}
}

// RunContainerLogAdapter forwards container-logs:{sourceID} messages from Redis
// to the WebSocket channel of the same name. The log collector publishes app
// and database container log lines (with a level) to these channels.
func (a *RedisAdapter) RunContainerLogAdapter(ctx context.Context) {
	pubsub := a.rdb.PSubscribe(ctx, "container-logs:*")
	defer pubsub.Close()

	ch := pubsub.Channel()
	for {
		select {
		case <-ctx.Done():
			return
		case msg, ok := <-ch:
			if !ok {
				return
			}
			// Redis channel: "container-logs:{sourceID}"
			// WS channel:    "container-logs:{sourceID}"
			a.hub.Broadcast(msg.Channel, "log", json.RawMessage(msg.Payload))
		}
	}
}

// ContainerStatusBroadcaster provides a method for the event watcher to push status updates.
type ContainerStatusBroadcaster struct {
	hub *Hub
}

func NewContainerStatusBroadcaster(hub *Hub) *ContainerStatusBroadcaster {
	return &ContainerStatusBroadcaster{hub: hub}
}

// BroadcastStatus sends a container status update to subscribers.
func (b *ContainerStatusBroadcaster) BroadcastStatus(applicationID, status string) {
	data, _ := json.Marshal(map[string]string{
		"application_id": applicationID,
		"status":         status,
	})
	channel := "container-status:" + applicationID
	b.hub.Broadcast(channel, "status", data)

	slog.Debug("ws: broadcast container status", "app_id", applicationID, "status", status)
}

// BroadcastDatabaseStatus sends a database container status update to subscribers.
func (b *ContainerStatusBroadcaster) BroadcastDatabaseStatus(databaseID, status string) {
	data, _ := json.Marshal(map[string]string{
		"database_id": databaseID,
		"status":      status,
	})
	channel := "database-status:" + databaseID
	b.hub.Broadcast(channel, "status", data)

	slog.Debug("ws: broadcast database status", "database_id", databaseID, "status", status)
}
