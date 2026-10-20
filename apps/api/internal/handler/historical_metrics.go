package handler

import (
	"net/http"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/belune-hq/belune/internal/store/generated"
)

type hostMetricPoint struct {
	CPUPercent  *float64 `json:"cpu_percent"`
	MemoryUsed  *int64   `json:"memory_used"`
	MemoryTotal *int64   `json:"memory_total"`
	DiskUsed    *int64   `json:"disk_used"`
	DiskTotal   *int64   `json:"disk_total"`
	RecordedAt  string   `json:"recorded_at"`
}

type appMetricPoint struct {
	CPUPercent     *float64 `json:"cpu_percent"`
	MemoryUsage    *int64   `json:"memory_usage"`
	MemoryLimit    *int64   `json:"memory_limit"`
	NetworkRxBytes *int64   `json:"network_rx_bytes"`
	NetworkTxBytes *int64   `json:"network_tx_bytes"`
	RecordedAt     string   `json:"recorded_at"`
}

// parseRangeDuration returns the since time for a given range query param.
func parseRangeDuration(rangeParam string) time.Time {
	now := time.Now()
	switch rangeParam {
	case "1h":
		return now.Add(-1 * time.Hour)
	case "6h":
		return now.Add(-6 * time.Hour)
	case "24h":
		return now.Add(-24 * time.Hour)
	case "7d":
		return now.Add(-7 * 24 * time.Hour)
	case "14d":
		return now.Add(-14 * 24 * time.Hour)
	default:
		return now.Add(-1 * time.Hour)
	}
}

// GetHostHistoricalMetrics returns host metrics stored at 1-second granularity.
// GET /api/metrics/host?range=1h|6h|24h|7d|14d
// GET /api/metrics/host?from=<RFC3339>&to=<RFC3339>  (explicit window; takes precedence)
//
//apidoc:tag platform/metrics
//apidoc:title Get Metrics
//apidoc:order 2
func (h *Handler) GetHostHistoricalMetrics(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()

	var (
		rows []generated.HostMetric
		err  error
	)
	if from, to, ok := parseFromTo(q.Get("from"), q.Get("to")); ok {
		rows, err = h.queries.ListHostMetricsBetween(r.Context(), generated.ListHostMetricsBetweenParams{
			RecordedAt:   pgtype.Timestamptz{Time: from, Valid: true},
			RecordedAt_2: pgtype.Timestamptz{Time: to, Valid: true},
		})
	} else {
		since := parseRangeDuration(q.Get("range"))
		rows, err = h.queries.GetHostMetrics(r.Context(), pgtype.Timestamptz{Time: since, Valid: true})
	}
	if err != nil {
		writeServerError(w, r, "failed to fetch host metrics", err)
		return
	}

	result := make([]hostMetricPoint, 0, len(rows))
	for _, row := range rows {
		result = append(result, hostMetricPoint{
			CPUPercent:  &row.CpuPercent,
			MemoryUsed:  &row.MemoryUsed,
			MemoryTotal: &row.MemoryTotal,
			DiskUsed:    &row.DiskUsed,
			DiskTotal:   &row.DiskTotal,
			RecordedAt:  row.RecordedAt.Time.Format(time.RFC3339),
		})
	}

	writeJSON(w, http.StatusOK, result)
}

// parseFromTo parses an explicit [from, to] window. Returns ok=false unless both
// parse as RFC3339 and from is before to.
func parseFromTo(fromParam, toParam string) (time.Time, time.Time, bool) {
	if fromParam == "" || toParam == "" {
		return time.Time{}, time.Time{}, false
	}
	from, err := time.Parse(time.RFC3339, fromParam)
	if err != nil {
		return time.Time{}, time.Time{}, false
	}
	to, err := time.Parse(time.RFC3339, toParam)
	if err != nil {
		return time.Time{}, time.Time{}, false
	}
	if !from.Before(to) {
		return time.Time{}, time.Time{}, false
	}
	return from, to, true
}
