package ws

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/belune-hq/belune/internal/naming"
	"github.com/belune-hq/belune/internal/runtime"
	"github.com/belune-hq/belune/internal/status"
	"github.com/belune-hq/belune/internal/store/generated"
)

// RunAppMetricsBroadcaster polls Docker container stats for applications that
// have active WebSocket subscribers on "metrics:app:{applicationId}" and
// broadcasts them to the hub. Runs until ctx is cancelled.
func RunAppMetricsBroadcaster(
	ctx context.Context,
	hub *Hub,
	rt runtime.ContainerRuntime,
	queries *generated.Queries,
) {
	const channelPrefix = "metrics:app:"
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			channels := hub.ActiveChannelsWithPrefix(channelPrefix)
			for _, ch := range channels {
				appIDStr := ch[len(channelPrefix):]
				var appUUID pgtype.UUID
				if err := appUUID.Scan(appIDStr); err != nil {
					continue
				}
				row, err := queries.GetApplicationWithProjectSlug(ctx, appUUID)
				if err != nil {
					continue
				}
				// Docker still answers a stats request for a stopped
				// container, and answers it with the last cgroup reading rather
				// than zeroes — a stopped application was reporting a live,
				// two-second-refreshing 117 MB of memory it was not using.
				// There is no such thing as current usage for something that is
				// not running, so nothing is published and the client shows no
				// data instead of convincing fiction.
				if row.Status != status.ApplicationRunning {
					continue
				}

				containerName := naming.ContainerName(row.ProjectSlug, row.Slug, appIDStr)
				stats, err := rt.ContainerStats(ctx, containerName)
				if err != nil {
					slog.Debug("app metrics: stats fetch failed", "container", containerName, "error", err)
					continue
				}
				point := map[string]any{
					"cpu_percent":      stats.CPUPercent,
					"memory_usage":     stats.MemoryUsage,
					"memory_limit":     stats.MemoryLimit,
					"network_rx_bytes": stats.NetworkRxBytes,
					"network_tx_bytes": stats.NetworkTxBytes,
					"recorded_at":      time.Now().Format(time.RFC3339),
				}
				data, err := json.Marshal(point)
				if err != nil {
					continue
				}
				hub.Broadcast(ch, "metrics", data)
			}
		}
	}
}
