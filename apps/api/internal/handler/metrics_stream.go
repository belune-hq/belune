package handler

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/belune-hq/belune/internal/naming"
	"github.com/belune-hq/belune/internal/pkg/sse"
	"github.com/belune-hq/belune/internal/status"
)

// StreamHostMetrics streams live host metric points via SSE.
// Subscribes to Redis pub/sub channel published by the metrics ticker.
// GET /api/metrics/host/stream
//
//apidoc:tag platform/metrics
//apidoc:title Stream Host Metrics
//apidoc:order 3
func (h *Handler) StreamHostMetrics(w http.ResponseWriter, r *http.Request) {
	writer, err := sse.NewWriter(w)
	if err != nil {
		writeServerError(w, r, "streaming not supported", err)
		return
	}

	ctx := r.Context()

	if err := writer.SendComment("ping"); err != nil {
		return
	}

	// Subscribe to the Redis pub/sub channel for live host metrics
	pubsub := h.rdb.Subscribe(ctx, "host:metrics:live")
	defer pubsub.Close()
	ch := pubsub.Channel()

	heartbeat := time.NewTicker(30 * time.Second)
	defer heartbeat.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-heartbeat.C:
			if err := writer.SendComment("ping"); err != nil {
				return
			}
		case msg, ok := <-ch:
			// A closed subscription yields a nil message whose payload panics
			// on read; go-redis closes ch on teardown, so this is not an error.
			if !ok {
				return
			}
			if err := writer.SendData(msg.Payload); err != nil {
				return
			}
		}
	}
}

// StreamApplicationMetrics streams live container metrics for a single application via SSE.
// Queries Docker stats API on-demand every 2 seconds — no database storage.
// GET /api/projects/{projectId}/applications/{applicationId}/metrics/stream
//
//apidoc:tag applications/metrics-logs
//apidoc:title Stream Metrics
//apidoc:order 1
func (h *Handler) StreamApplicationMetrics(w http.ResponseWriter, r *http.Request) {
	applicationID := chi.URLParam(r, "applicationId")
	var appUUID pgtype.UUID
	if err := appUUID.Scan(applicationID); err != nil {
		writeError(w, http.StatusBadRequest, "invalid application id")
		return
	}

	if !h.canAccessApplication(r, appUUID) {
		writeError(w, http.StatusForbidden, "access denied")
		return
	}

	// Look up container name from application record
	appRow, err := h.queries.GetApplicationWithProjectSlug(r.Context(), appUUID)
	if err != nil {
		writeError(w, http.StatusNotFound, "application not found")
		return
	}
	containerName := naming.ContainerName(appRow.ProjectSlug, appRow.Slug, applicationID)

	// Resolved once for the life of the stream: placement does not change under
	// a live connection, and re-resolving every tick would be a query per tick.
	rt, err := h.runtimes.For(r.Context(), appRow.ServerID)
	if err != nil {
		writeError(w, http.StatusBadGateway, "failed to reach the application's server")
		return
	}

	writer, sseErr := sse.NewWriter(w)
	if sseErr != nil {
		writeServerError(w, r, "streaming not supported", sseErr)
		return
	}

	ctx := r.Context()

	if err := writer.SendComment("ping"); err != nil {
		return
	}

	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	heartbeat := time.NewTicker(30 * time.Second)
	defer heartbeat.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-heartbeat.C:
			if err := writer.SendComment("ping"); err != nil {
				return
			}
		case <-ticker.C:
			// Re-read the status each tick rather than trusting the one read at
			// connect: a long-lived stream outlives a stop. Docker answers a
			// stats request for a stopped container with its last cgroup
			// reading rather than zeroes, so without this the stream reports
			// memory the application is not using.
			if row, err := h.queries.GetApplication(ctx, appUUID); err == nil &&
				row.Status != status.ApplicationRunning {
				continue
			}

			stats, err := rt.ContainerStats(ctx, containerName)
			if err != nil {
				slog.Debug("failed to collect container stats for stream", "container", containerName, "error", err)
				continue
			}

			point := appMetricPoint{
				CPUPercent:     &stats.CPUPercent,
				MemoryUsage:    &stats.MemoryUsage,
				MemoryLimit:    &stats.MemoryLimit,
				NetworkRxBytes: &stats.NetworkRxBytes,
				NetworkTxBytes: &stats.NetworkTxBytes,
				RecordedAt:     time.Now().Format(time.RFC3339),
			}
			data, _ := json.Marshal(point)
			if err := writer.SendData(string(data)); err != nil {
				return
			}
		}
	}
}
