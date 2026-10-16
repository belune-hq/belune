package handler

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/weiliang79/belune/internal/config"
	"github.com/weiliang79/belune/internal/server/middleware"
	"github.com/weiliang79/belune/internal/service"
	"github.com/weiliang79/belune/internal/store/generated"
)

// latestHostStats returns the cached host snapshot written by the metrics
// ticker (~1s fresh), falling back to a live collection when the cache is empty
// (e.g. the ticker isn't running). Reading the cache avoids re-running gopsutil
// per request and the CPU-percent jitter that a second concurrent sampler causes.
func (h *Handler) latestHostStats(ctx context.Context) service.HostMetricPoint {
	if h.rdb != nil {
		if data, err := h.rdb.Get(ctx, service.HostMetricsLatestKey).Bytes(); err == nil {
			var p service.HostMetricPoint
			if json.Unmarshal(data, &p) == nil {
				return p
			}
		}
	}
	return service.CollectHostStats(ctx)
}

// healthRatio breaks the service fleet down by state. Each state is reported
// separately rather than as a single "not running" figure: a deliberately
// stopped service, a crashed one, and one that is up but failing its health
// check all need different responses, so collapsing them hid the distinction
// the operator actually cares about.
//
// The buckets are exhaustive — they always sum to Total. Busy is deliberately
// the residual rather than a counted set (databases mid-create/upgrade/backup
// today): if a new service status is ever introduced it surfaces here instead
// of silently vanishing from the card.
type healthRatio struct {
	Running   int64 `json:"running"`
	Errored   int64 `json:"errored"`
	Stopped   int64 `json:"stopped"`
	Unhealthy int64 `json:"unhealthy"`
	Inactive  int64 `json:"inactive"`
	Busy      int64 `json:"busy"`
	Total     int64 `json:"total"`
}

type deploy7dStats struct {
	Succeeded     int64   `json:"succeeded"`
	Failed        int64   `json:"failed"`
	Total         int64   `json:"total"`
	MedianBuildMs float64 `json:"median_build_ms"`
}

// needsAttention counts what is currently broken. The buckets are disjoint —
// each affected service appears in exactly one — so they sum to Total and a
// single incident is never reported as two issues.
//
// UnhealthyServices is separate from ErrorServices rather than folded into it:
// an unhealthy container is up and serving, just failing its check, which is a
// different situation (and a different urgency) from one that is not running.
type needsAttention struct {
	FailedDeploys     int64 `json:"failed_deploys"`
	ErrorServices     int64 `json:"error_services"`
	UnhealthyServices int64 `json:"unhealthy_services"`
	FailedBackups     int64 `json:"failed_backups"`
	// FailedTasks counts scheduled tasks whose latest run failed — tasks, not runs.
	FailedTasks int64 `json:"failed_tasks"`
	Total       int64 `json:"total"`
}

type statsResponse struct {
	IsAdmin        bool                     `json:"is_admin"`
	AppHealth      healthRatio              `json:"app_health"`
	Deploy7d       deploy7dStats            `json:"deploy_7d"`
	NeedsAttention needsAttention           `json:"needs_attention"`
	Host           *service.HostMetricPoint `json:"host"`
	// ConfigWarnings sits beside NeedsAttention rather than inside it. That
	// block counts failing workloads; a configuration finding is a different
	// kind of thing, and folding it in would corrupt the number an operator
	// reads as "how many things are broken right now". Admin-only, like Host.
	ConfigWarnings []config.Warning `json:"config_warnings,omitempty"`
}

// appHealth folds the per-resource counts into the exhaustive fleet breakdown.
// Databases have no 'unhealthy' or 'inactive' state, so those come from
// applications alone; everything left over (a database creating, upgrading, or
// backing up) becomes Busy, which keeps the buckets summing to Total.
func appHealth(appH generated.CountApplicationHealthRow, dbH generated.CountDatabaseHealthRow, errored int64) healthRatio {
	h := healthRatio{
		Running:   appH.Running + dbH.Running,
		Errored:   errored,
		Stopped:   appH.Stopped + dbH.Stopped,
		Unhealthy: appH.Unhealthy,
		Inactive:  appH.Inactive,
		Total:     appH.Total + dbH.Total,
	}
	// Residual. Clamped at zero so a counting skew can never render a negative
	// badge; the named buckets are all mutually exclusive, so it should not occur.
	h.Busy = h.Total - h.Running - h.Errored - h.Stopped - h.Unhealthy - h.Inactive
	if h.Busy < 0 {
		h.Busy = 0
	}
	return h
}

// GetStats returns the operator-health stat strip in a single call.
//
// Audience split: admins see all projects plus host resources and failed
// backups; members see only their own projects' app health, deploy success,
// and attention items (host + backups are admin-only signals).
//
//apidoc:tag platform
//apidoc:title Get Stats
//apidoc:description Operator-health summary in one call. An admin sees every project plus host resources and failed backups; a member sees only their own projects and any shared with them, without the host and backup signals.
//apidoc:order 1
func (h *Handler) GetStats(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	isAdmin := middleware.RoleFromContext(ctx) == "admin"

	// scope is left invalid (SQL NULL) for admins so the queries count every
	// project; for members it pins aggregates to their owned projects.
	var scope pgtype.UUID
	if !isAdmin {
		if err := scope.Scan(middleware.UserIDFromContext(ctx)); err != nil {
			writeError(w, http.StatusUnauthorized, "invalid user id")
			return
		}
	}

	appH, err := h.queries.CountApplicationHealth(ctx, scope)
	if err != nil {
		slog.Warn("stats: count application health", "error", err)
	}
	dbH, err := h.queries.CountDatabaseHealth(ctx, scope)
	if err != nil {
		slog.Warn("stats: count database health", "error", err)
	}
	dep, err := h.queries.CountDeployments7d(ctx, scope)
	if err != nil {
		slog.Warn("stats: count deployments 7d", "error", err)
	}
	// Distinct from dep.Failed: that is the 7-day *statistic* behind the deploy
	// success rate and stays historical. "Needs attention" instead wants what is
	// still broken now, so it counts applications whose latest deploy failed.
	// It excludes applications already counted as errored, so the attention
	// buckets stay disjoint and one broken app is one issue, not two.
	unresolvedDeploys, err := h.queries.CountUnresolvedFailedDeploys(ctx, scope)
	if err != nil {
		slog.Warn("stats: count unresolved failed deploys", "error", err)
	}

	errorServices := appH.Errored + dbH.Errored

	resp := statsResponse{
		IsAdmin:   isAdmin,
		AppHealth: appHealth(appH, dbH, errorServices),
		Deploy7d: deploy7dStats{
			Succeeded:     dep.Succeeded,
			Failed:        dep.Failed,
			Total:         dep.Total,
			MedianBuildMs: dep.MedianBuildMs,
		},
	}

	// Scheduled backups belong to projects, so they are scoped like every other
	// aggregate and counted for members too — a member whose database backup has
	// been failing needs to know. The global platform backup below is a
	// host-level concern and stays admin-only.
	failedBackups, err := h.queries.CountUnresolvedFailedScheduledBackups(ctx, scope)
	if err != nil {
		slog.Warn("stats: count failed scheduled backups", "error", err)
		failedBackups = 0
	}

	failedTasks, err := h.queries.CountFailedScheduledTasks(ctx, scope)
	if err != nil {
		slog.Warn("stats: count failed scheduled tasks", "error", err)
		failedTasks = 0
	}

	if isAdmin {
		platformBackup, perr := h.queries.CountUnresolvedFailedBackup(ctx)
		if perr != nil {
			slog.Warn("stats: count failed platform backup", "error", perr)
		} else {
			failedBackups += platformBackup
		}
		host := h.latestHostStats(ctx)
		resp.Host = &host
		// Computed from the loaded config, so this costs no query — it is the
		// same list logged once at startup.
		resp.ConfigWarnings = h.cfg.Validate()
	}

	// Unhealthy is an application-only state; databases have no health check.
	unhealthyServices := appH.Unhealthy
	resp.NeedsAttention = needsAttention{
		FailedDeploys:     unresolvedDeploys,
		ErrorServices:     errorServices,
		UnhealthyServices: unhealthyServices,
		FailedBackups:     failedBackups,
		FailedTasks:       failedTasks,
		Total:             unresolvedDeploys + errorServices + unhealthyServices + failedBackups + failedTasks,
	}

	writeJSON(w, http.StatusOK, resp)
}
