package handler

import (
	"encoding/json"
	"log/slog"
	"net/http"

	"github.com/hibiken/asynq"

	"github.com/weiliang79/belune/internal/pkg/metrics"
	"github.com/weiliang79/belune/internal/runtime"
	"github.com/weiliang79/belune/internal/status"
)

type metricsResponse struct {
	Projects     int64          `json:"projects"`
	Applications int64          `json:"applications"`
	Databases    int64          `json:"databases"`
	Deployments  int64          `json:"deployments"`
	Containers   containerStats `json:"containers"`
}

type containerTypeCount struct {
	Running int `json:"running"`
	Total   int `json:"total"`
}

type containerStats struct {
	Running int `json:"running"`
	Stopped int `json:"stopped"`
	Error   int `json:"error"`
	Total   int `json:"total"`
	// ByType groups managed containers into the categories the platform
	// actually models: "application" (carries an application-id label) and
	// "database" (everything else managed). No synthetic worker/cron buckets.
	ByType map[string]containerTypeCount `json:"by_type"`
}

// GetSummary returns the dashboard's overview tile: resource COUNTS, not
// metrics. Named for what it returns — the old GetMetrics collided with
// GetHostHistoricalMetrics and StreamHostMetrics, which are genuine host
// time-series, and its route collided with the Prometheus scrape at /metrics.
//
//apidoc:tag platform/metrics
//apidoc:title Get Summary
//apidoc:description Resource counts for the dashboard overview — project, application, database and deployment totals plus a container census. Not time-series data: see `/api/metrics/host` for that, and `/metrics` for the Prometheus scrape endpoint.
//apidoc:order 1
func (h *Handler) GetSummary(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	projects, err := h.queries.CountProjects(ctx)
	if err != nil {
		slog.Error("failed to count projects", "error", err)
	}
	applications, err := h.queries.CountApplications(ctx)
	if err != nil {
		slog.Error("failed to count applications", "error", err)
	}
	databases, err := h.queries.CountDatabases(ctx)
	if err != nil {
		slog.Error("failed to count databases", "error", err)
	}
	deployments, err := h.queries.CountDeployments(ctx)
	if err != nil {
		slog.Error("failed to count deployments", "error", err)
	}

	stats := containerStats{ByType: map[string]containerTypeCount{}}
	// The container census is host-wide, so it is the control plane's own
	// runtime rather than any one resource's placement.
	rt, err := h.runtimes.Local(ctx)
	if err != nil {
		slog.Error("failed to reach the Docker host", "error", err)
	} else if containers, err := rt.ListContainers(ctx); err == nil {
		for _, c := range containers {
			stats.Total++
			switch c.Status {
			case status.ApplicationRunning:
				stats.Running++
			case "dead", "restarting":
				stats.Error++
			default:
				stats.Stopped++
			}

			kind := "database"
			if _, ok := c.Labels[runtime.LabelApplicationID]; ok {
				kind = "application"
			}
			tc := stats.ByType[kind]
			tc.Total++
			if c.Status == status.ApplicationRunning {
				tc.Running++
			}
			stats.ByType[kind] = tc
		}
	}

	writeJSON(w, http.StatusOK, metricsResponse{
		Projects:     projects,
		Applications: applications,
		Databases:    databases,
		Deployments:  deployments,
		Containers:   stats,
	})
}

// ServeMetrics wraps metrics.Handler() (the Prometheus registry's own
// promhttp handler) as a named *Handler method instead of registering that
// third-party http.Handler directly. It's otherwise a pure passthrough —
// the wrapping exists only so this route has a real FuncDecl: an inline
// third-party handler value has no name reflection can recover (it showed
// up as the meaningless operationId "func1") and no doc comment a
// //apidoc:tag directive could attach to.
//
//apidoc:tag platform/metrics
//apidoc:title Scrape Prometheus Metrics
//apidoc:description The Prometheus scrape endpoint at `GET /metrics`, a passthrough to the registry's own promhttp handler. Unrelated to `GET /api/summary`, which returns resource counts.
//apidoc:order 4
func (h *Handler) ServeMetrics(w http.ResponseWriter, r *http.Request) {
	metrics.Handler().ServeHTTP(w, r)
}

//apidoc:tag platform/maintenance
//apidoc:title Trigger Cleanup
//apidoc:order 5
func (h *Handler) TriggerCleanup(w http.ResponseWriter, r *http.Request) {
	type cleanupRequest struct {
		RetainCount int      `json:"retain_count,omitempty"`
		Actions     []string `json:"actions,omitempty"` // empty = full cleanup
	}

	var req cleanupRequest
	if r.Body != nil {
		_ = json.NewDecoder(r.Body).Decode(&req)
	}
	if req.RetainCount <= 0 {
		req.RetainCount = 3
	}

	validActions := map[string]bool{
		"deployments": true, "images": true, "volumes": true,
		"containers": true, "build_cache": true,
		"orphaned_backups": true,
	}
	for _, a := range req.Actions {
		if !validActions[a] {
			writeError(w, http.StatusBadRequest, "invalid cleanup action: "+a)
			return
		}
	}

	payload, _ := json.Marshal(req)
	task := asynq.NewTask("cleanup", payload)
	if _, err := h.asynq.Enqueue(task, asynq.Queue("low")); err != nil {
		writeServerError(w, r, "failed to enqueue cleanup task", err)
		return
	}

	writeJSON(w, http.StatusAccepted, map[string]string{"status": "cleanup queued"})
}
