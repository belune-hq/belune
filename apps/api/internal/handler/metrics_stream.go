package handler

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/belune-hq/belune/internal/instances"
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
// Queries Docker stats API on-demand every instances.MetricsInterval — no database storage.
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

	appRow, err := h.queries.GetApplicationWithProjectSlug(r.Context(), appUUID)
	if err != nil {
		writeError(w, http.StatusNotFound, "application not found")
		return
	}
	if rows, err := h.queries.ListInstancesWithProcessTypeByApplication(r.Context(), appUUID); err != nil || len(rows) == 0 {
		writeServerError(w, r, "application has no container instance", err)
		return
	}

	// The RUNTIME is resolved once for the life of the stream: which host an
	// application is placed on does not change under a live connection. The
	// INSTANCES are not -- someone can scale the application while this page is open,
	// and a list resolved at connect would report one container for ever -- so they are
	// re-read every tick below. One indexed query per tick, next to the status re-read
	// that was already there; a change signal would be more machinery than a 4s query.
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

	ticker := time.NewTicker(instances.MetricsInterval)
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

			rows, err := h.queries.ListInstancesWithProcessTypeByApplication(ctx, appUUID)
			if err != nil {
				slog.Debug("failed to list instances for stream", "application_id", applicationID, "error", err)
				continue
			}
			live := instances.LiveInstances(rows)
			names := make([]string, 0, len(live))
			for _, l := range live {
				names = append(names, l.ContainerName)
			}
			point, ok := instances.BuildPoint(live, instances.FetchStats(ctx, rt, names), time.Now())
			if !ok {
				continue
			}
			data, _ := json.Marshal(point)
			if err := writer.SendData(string(data)); err != nil {
				return
			}
		}
	}
}
