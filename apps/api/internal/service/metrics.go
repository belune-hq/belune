package service

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/redis/go-redis/v9"
	"github.com/shirou/gopsutil/v4/cpu"
	"github.com/shirou/gopsutil/v4/disk"
	"github.com/shirou/gopsutil/v4/mem"

	"github.com/belune-hq/belune/internal/store/generated"
)

// HostMetricPoint is published to Redis and sent via SSE.
type HostMetricPoint struct {
	CPUPercent  float64 `json:"cpu_percent"`
	MemoryUsed  int64   `json:"memory_used"`
	MemoryTotal int64   `json:"memory_total"`
	DiskUsed    int64   `json:"disk_used"`
	DiskTotal   int64   `json:"disk_total"`
	// Swap is reported separately from RAM and never summed with it: a sum reads
	// healthiest exactly when the box is thrashing. SwapTotal is 0 on hosts with
	// no swap configured.
	SwapUsed   int64  `json:"swap_used"`
	SwapTotal  int64  `json:"swap_total"`
	RecordedAt string `json:"recorded_at"`
}

type MetricsService struct {
	queries *generated.Queries
	rdb     *redis.Client
}

func NewMetricsService(queries *generated.Queries, rdb *redis.Client) *MetricsService {
	return &MetricsService{queries: queries, rdb: rdb}
}

// HostMetricsLatestKey caches the most recent host snapshot (short TTL) so HTTP
// handlers can read it instead of re-running gopsutil collection per request.
const HostMetricsLatestKey = "host:metrics:latest"

// CollectHostStats gathers host CPU, memory, and disk usage via gopsutil.
func CollectHostStats(ctx context.Context) HostMetricPoint {
	now := time.Now()
	point := HostMetricPoint{RecordedAt: now.Format(time.RFC3339)}

	cpuPercents, err := cpu.PercentWithContext(ctx, 0, false)
	if err == nil && len(cpuPercents) > 0 {
		point.CPUPercent = cpuPercents[0]
	} else if err != nil {
		slog.Warn("failed to collect host CPU", "error", err)
	}

	memInfo, err := mem.VirtualMemoryWithContext(ctx)
	if err == nil {
		point.MemoryUsed = int64(memInfo.Used)
		point.MemoryTotal = int64(memInfo.Total)
	} else {
		slog.Warn("failed to collect host memory", "error", err)
	}

	swapInfo, err := mem.SwapMemoryWithContext(ctx)
	if err == nil {
		point.SwapUsed = int64(swapInfo.Used)
		point.SwapTotal = int64(swapInfo.Total)
	} else {
		slog.Warn("failed to collect host swap", "error", err)
	}

	diskInfo, err := disk.UsageWithContext(ctx, "/")
	if err == nil {
		point.DiskUsed = int64(diskInfo.Used)
		point.DiskTotal = int64(diskInfo.Total)
	} else {
		slog.Warn("failed to collect host disk", "error", err)
	}

	return point
}

// PersistHostMetric inserts a host metric row into the database.
func (s *MetricsService) PersistHostMetric(ctx context.Context, point HostMetricPoint) {
	recordedAt, _ := time.Parse(time.RFC3339, point.RecordedAt)
	if err := s.queries.InsertHostMetric(ctx, generated.InsertHostMetricParams{
		CpuPercent:  point.CPUPercent,
		MemoryUsed:  point.MemoryUsed,
		MemoryTotal: point.MemoryTotal,
		DiskUsed:    point.DiskUsed,
		DiskTotal:   point.DiskTotal,
		SwapUsed:    point.SwapUsed,
		SwapTotal:   point.SwapTotal,
		RecordedAt:  pgtype.Timestamptz{Time: recordedAt, Valid: true},
	}); err != nil {
		slog.Error("failed to insert host metric", "error", err)
	}
}

// HostMetricsCleanup prunes the 1-second host_metrics series to its configured
// window. Host metrics are high-frequency, so this runs hourly on its own (hours-
// based) setting rather than the day-based RetentionCleanup.
func (s *MetricsService) HostMetricsCleanup(ctx context.Context) {
	retentionHours := 24
	if setting, err := s.queries.GetSetting(ctx, "host_metrics_retention_hours"); err == nil {
		var hours int
		if _, scanErr := fmt.Sscanf(setting.Value, "%d", &hours); scanErr == nil && hours > 0 {
			retentionHours = hours
		}
	}

	cutoff := time.Now().Add(-time.Duration(retentionHours) * time.Hour)
	if err := s.queries.DeleteOldHostMetrics(ctx, pgtype.Timestamptz{Time: cutoff, Valid: true}); err != nil {
		slog.Error("failed to delete old host metrics", "error", err)
	} else {
		slog.Info("host metrics cleanup completed", "retention_hours", retentionHours)
	}
}

// RetentionCleanup deletes application, request, and audit logs older than their
// configured retention periods. Host metrics are pruned separately by
// HostMetricsCleanup (hours-based, hourly).
func (s *MetricsService) RetentionCleanup(ctx context.Context) {
	// Application log retention
	appLogRetentionDays := 7
	logSetting, err := s.queries.GetSetting(ctx, "app_log_retention_days")
	if err == nil {
		var days int
		if _, scanErr := fmt.Sscanf(logSetting.Value, "%d", &days); scanErr == nil && days > 0 {
			appLogRetentionDays = days
		}
	}

	daysStr := pgtype.Text{String: fmt.Sprintf("%d", appLogRetentionDays), Valid: true}
	if err := s.queries.DeleteOldContainerLogs(ctx, daysStr); err != nil {
		slog.Error("failed to delete old container logs", "error", err)
	} else {
		slog.Info("container log retention cleanup completed", "retention_days", appLogRetentionDays)
	}

	// Request log retention (same period as app logs)
	reqLogRetentionDays := 7
	reqSetting, err := s.queries.GetSetting(ctx, "request_log_retention_days")
	if err == nil {
		var days int
		if _, scanErr := fmt.Sscanf(reqSetting.Value, "%d", &days); scanErr == nil && days > 0 {
			reqLogRetentionDays = days
		}
	}

	reqDaysStr := pgtype.Text{String: fmt.Sprintf("%d", reqLogRetentionDays), Valid: true}
	if err := s.queries.DeleteOldRequestLogs(ctx, reqDaysStr); err != nil {
		slog.Error("failed to delete old request logs", "error", err)
	} else {
		slog.Info("request log retention cleanup completed", "retention_days", reqLogRetentionDays)
	}

	// Audit log retention. Audit logs are compliance-sensitive: 0 (the default)
	// means keep forever, so only prune when an operator has set a positive value.
	auditRetentionDays := 0
	if auditSetting, err := s.queries.GetSetting(ctx, "audit_log_retention_days"); err == nil {
		var days int
		if _, scanErr := fmt.Sscanf(auditSetting.Value, "%d", &days); scanErr == nil && days > 0 {
			auditRetentionDays = days
		}
	}
	if auditRetentionDays > 0 {
		auditDaysStr := pgtype.Text{String: fmt.Sprintf("%d", auditRetentionDays), Valid: true}
		if err := s.queries.DeleteOldAuditLogs(ctx, auditDaysStr); err != nil {
			slog.Error("failed to delete old audit logs", "error", err)
		} else {
			slog.Info("audit log retention cleanup completed", "retention_days", auditRetentionDays)
		}
	}
}

// StartTicker collects host metrics every 1 second, publishes to Redis for live
// SSE consumers, and persists every point to the DB so stored history matches the
// 1-second live stream (the table is bounded by hourly HostMetricsCleanup).
// Blocks until ctx is cancelled.
func (s *MetricsService) StartTicker(ctx context.Context) {
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			point := CollectHostStats(ctx)

			data, err := json.Marshal(point)
			if err == nil {
				if pubErr := s.rdb.Publish(ctx, "host:metrics:live", data).Err(); pubErr != nil {
					slog.Debug("failed to publish host metrics to Redis", "error", pubErr)
				}
				// Cache the latest point (short TTL) for per-request handlers.
				if setErr := s.rdb.Set(ctx, HostMetricsLatestKey, data, 5*time.Second).Err(); setErr != nil {
					slog.Debug("failed to cache latest host metrics", "error", setErr)
				}
			}

			s.PersistHostMetric(ctx, point)
		}
	}
}
