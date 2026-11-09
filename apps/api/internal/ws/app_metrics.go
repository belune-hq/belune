package ws

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/belune-hq/belune/internal/instances"
	"github.com/belune-hq/belune/internal/runtime"
	"github.com/belune-hq/belune/internal/status"
	"github.com/belune-hq/belune/internal/store/generated"
)

// AppMetricsQueries is the database slice the broadcaster reads.
type AppMetricsQueries interface {
	GetApplicationWithProjectSlug(ctx context.Context, id pgtype.UUID) (generated.GetApplicationWithProjectSlugRow, error)
	ListInstancesWithProcessTypeByApplication(ctx context.Context, applicationID pgtype.UUID) ([]generated.ListInstancesWithProcessTypeByApplicationRow, error)
}

// RunAppMetricsBroadcaster polls Docker container stats for applications that
// have active WebSocket subscribers on "metrics:app:{applicationId}" and
// broadcasts them to the hub. Runs until ctx is cancelled.
//
// One pass collects every live instance of every subscribed application into ONE
// concurrent fan-out, so a pass costs about one stats call (~2s, see
// instances.MetricsInterval) whatever the number of applications and replicas. It
// used to be sequential on a 2s ticker with each call costing ~2s: permanently behind
// from two applications up, and replicas would have multiplied it.
//
// time.Ticker drops ticks while a pass is running, so a slow pass cannot pile up
// work: the next one starts when this one ends, at the earliest.
func RunAppMetricsBroadcaster(
	ctx context.Context,
	hub *Hub,
	rt runtime.ContainerRuntime,
	queries AppMetricsQueries,
) {
	const channelPrefix = "metrics:app:"
	ticker := time.NewTicker(instances.MetricsInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			broadcastAppMetrics(ctx, hub, rt, queries, channelPrefix)
		}
	}
}

type appWork struct {
	channel string
	live    []generated.ListInstancesWithProcessTypeByApplicationRow
}

// broadcastAppMetrics runs one pass.
func broadcastAppMetrics(ctx context.Context, hub *Hub, rt instances.StatsRuntime, queries AppMetricsQueries, channelPrefix string) {
	var work []appWork
	var names []string
	for _, ch := range hub.ActiveChannelsWithPrefix(channelPrefix) {
		var appUUID pgtype.UUID
		if err := appUUID.Scan(ch[len(channelPrefix):]); err != nil {
			continue
		}
		row, err := queries.GetApplicationWithProjectSlug(ctx, appUUID)
		if err != nil {
			continue
		}
		// An application that is not running has no current usage; nothing is
		// published and the client shows no data instead of convincing fiction (see
		// instances.LiveInstances for the per-instance version of the same rule).
		if row.Status != status.ApplicationRunning {
			continue
		}
		// Resolved per tick, unlike the old single container: the set of channels is
		// dynamic and replicas change under a live subscription.
		rows, err := queries.ListInstancesWithProcessTypeByApplication(ctx, appUUID)
		if err != nil {
			continue
		}
		live := instances.LiveInstances(rows)
		if len(live) == 0 {
			continue
		}
		work = append(work, appWork{channel: ch, live: live})
		for _, r := range live {
			names = append(names, r.ContainerName)
		}
	}
	if len(work) == 0 {
		return
	}

	stats := instances.FetchStats(ctx, rt, names)
	now := time.Now()
	for _, w := range work {
		point, ok := instances.BuildPoint(w.live, stats, now)
		if !ok {
			continue
		}
		data, err := json.Marshal(point)
		if err != nil {
			slog.Debug("app metrics: could not encode a point", "channel", w.channel, "error", err)
			continue
		}
		hub.Broadcast(w.channel, "metrics", data)
	}
}
