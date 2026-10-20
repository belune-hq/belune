// Package quota enforces aggregate per-user and per-project resource caps that
// sit on top of the per-container limits stored on each application. A scope's
// limits may leave individual dimensions nil (unlimited); the caller should
// treat a missing quota record as "no caps set, allow everything".
package quota

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/belune-hq/belune/internal/store/generated"
)

const alertHysteresis = 10 // pp below threshold before resetting alert state

// ThresholdAlert is returned by MaybeAlertProjectThreshold when a quota
// threshold alert should be sent. CurrentPercent is the rounded usage %.
type ThresholdAlert struct {
	ProjectID      pgtype.UUID
	CurrentPercent int
	Threshold      int
}

const (
	ScopeUser    = "user"
	ScopeProject = "project"
)

// Usage is a point-in-time snapshot of resources consumed inside a scope.
type Usage struct {
	Applications int64   `json:"applications"`
	CPU          float64 `json:"cpu"`          // cpu cores (sum of cpu_limit; apps with 0 contribute nothing)
	MemoryBytes  int64   `json:"memory_bytes"` // bytes (sum of memory_limit)
}

// Limits are the configured caps for a scope. A nil pointer means no cap for
// that dimension.
type Limits struct {
	MaxApplications *int32   `json:"max_applications"`
	MaxCPU          *float64 `json:"max_cpu"`
	MaxMemoryMB     *int64   `json:"max_memory_mb"`
}

// ExceededError is returned when a pending operation would cross a cap. It
// carries enough context for the handler to craft a helpful error message and
// pick the right HTTP status.
type ExceededError struct {
	Scope     string
	Dimension string
	Current   float64
	Limit     float64
	Pending   float64
}

func (e *ExceededError) Error() string {
	return fmt.Sprintf("%s quota exceeded: %s (current=%.2f pending=%.2f limit=%.2f)",
		e.Scope, e.Dimension, e.Current, e.Pending, e.Limit)
}

// HTTPStatus reports the status to return when this error is surfaced through
// the API. 403 matches existing access-denied patterns.
func (e *ExceededError) HTTPStatus() int { return http.StatusForbidden }

// Service wraps quota lookups and enforcement.
type Service struct {
	q *generated.Queries
}

// NewService creates a quota service backed by sqlc queries.
func NewService(q *generated.Queries) *Service { return &Service{q: q} }

// LimitsFor returns the stored limits for a scope, or zero-value Limits (all
// nil) when no quota row exists.
func (s *Service) LimitsFor(ctx context.Context, scope string, scopeID pgtype.UUID) (Limits, error) {
	row, err := s.q.GetQuota(ctx, generated.GetQuotaParams{Scope: scope, ScopeID: scopeID})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Limits{}, nil
		}
		return Limits{}, fmt.Errorf("get quota: %w", err)
	}
	return limitsFromRow(row), nil
}

// UsageForProject sums current application count + resource totals for a project.
func (s *Service) UsageForProject(ctx context.Context, projectID pgtype.UUID) (Usage, error) {
	count, err := s.q.CountApplicationsByProject(ctx, projectID)
	if err != nil {
		return Usage{}, fmt.Errorf("count apps: %w", err)
	}
	sums, err := s.q.SumApplicationResourcesByProject(ctx, projectID)
	if err != nil {
		return Usage{}, fmt.Errorf("sum resources: %w", err)
	}
	return Usage{Applications: count, CPU: sums.CpuTotal, MemoryBytes: sums.MemoryTotalBytes}, nil
}

// UsageForUser sums current application count + resource totals across every
// project owned by the user.
func (s *Service) UsageForUser(ctx context.Context, userID pgtype.UUID) (Usage, error) {
	count, err := s.q.CountApplicationsByUser(ctx, userID)
	if err != nil {
		return Usage{}, fmt.Errorf("count apps: %w", err)
	}
	sums, err := s.q.SumApplicationResourcesByUser(ctx, userID)
	if err != nil {
		return Usage{}, fmt.Errorf("sum resources: %w", err)
	}
	return Usage{Applications: count, CPU: sums.CpuTotal, MemoryBytes: sums.MemoryTotalBytes}, nil
}

// CheckCurrentUsage verifies that the current aggregate usage for the project
// and owning user is within their configured limits. Unlike
// CheckApplicationCreate, it adds no delta: it answers "is the existing
// fleet still inside quota?" — used by the deploy worker as defence in depth
// after a quota was tightened post-creation.
func (s *Service) CheckCurrentUsage(ctx context.Context, projectID, userID pgtype.UUID) error {
	projLimits, err := s.LimitsFor(ctx, ScopeProject, projectID)
	if err != nil {
		return err
	}
	projUsage, err := s.UsageForProject(ctx, projectID)
	if err != nil {
		return err
	}
	if err := checkDelta(ScopeProject, projLimits, projUsage, 0, 0, 0); err != nil {
		return err
	}

	userLimits, err := s.LimitsFor(ctx, ScopeUser, userID)
	if err != nil {
		return err
	}
	userUsage, err := s.UsageForUser(ctx, userID)
	if err != nil {
		return err
	}
	return checkDelta(ScopeUser, userLimits, userUsage, 0, 0, 0)
}

// CheckApplicationCreate validates that adding an application with the given
// cpu/memory request would not exceed either the project or the owning user's
// quota. A zero cpuDelta or memoryDelta still consumes one application slot.
func (s *Service) CheckApplicationCreate(ctx context.Context, projectID, userID pgtype.UUID, cpuDelta float64, memoryBytesDelta int64) error {
	projLimits, err := s.LimitsFor(ctx, ScopeProject, projectID)
	if err != nil {
		return err
	}
	projUsage, err := s.UsageForProject(ctx, projectID)
	if err != nil {
		return err
	}
	if err := checkDelta(ScopeProject, projLimits, projUsage, 1, cpuDelta, memoryBytesDelta); err != nil {
		return err
	}

	userLimits, err := s.LimitsFor(ctx, ScopeUser, userID)
	if err != nil {
		return err
	}
	userUsage, err := s.UsageForUser(ctx, userID)
	if err != nil {
		return err
	}
	return checkDelta(ScopeUser, userLimits, userUsage, 1, cpuDelta, memoryBytesDelta)
}

func checkDelta(scope string, limits Limits, cur Usage, appDelta int64, cpuDelta float64, memDelta int64) error {
	if limits.MaxApplications != nil {
		pending := cur.Applications + appDelta
		if pending > int64(*limits.MaxApplications) {
			return &ExceededError{Scope: scope, Dimension: "applications", Current: float64(cur.Applications), Pending: float64(pending), Limit: float64(*limits.MaxApplications)}
		}
	}
	if limits.MaxCPU != nil {
		pending := cur.CPU + cpuDelta
		if pending > *limits.MaxCPU {
			return &ExceededError{Scope: scope, Dimension: "cpu", Current: cur.CPU, Pending: pending, Limit: *limits.MaxCPU}
		}
	}
	if limits.MaxMemoryMB != nil {
		pendingMB := (cur.MemoryBytes + memDelta) / (1024 * 1024)
		if pendingMB > *limits.MaxMemoryMB {
			return &ExceededError{Scope: scope, Dimension: "memory_mb", Current: float64(cur.MemoryBytes / (1024 * 1024)), Pending: float64(pendingMB), Limit: float64(*limits.MaxMemoryMB)}
		}
	}
	return nil
}

func limitsFromRow(row generated.Quota) Limits {
	var l Limits
	if row.MaxApplications.Valid {
		v := row.MaxApplications.Int32
		l.MaxApplications = &v
	}
	if row.MaxCpu.Valid {
		v := row.MaxCpu.Float64
		l.MaxCPU = &v
	}
	if row.MaxMemoryMb.Valid {
		v := row.MaxMemoryMb.Int64
		l.MaxMemoryMB = &v
	}
	return l
}

// LimitsToRow converts exposed Limits into the pgtype params used for upsert.
func LimitsToRow(l Limits) (pgtype.Int4, pgtype.Float8, pgtype.Int8) {
	var apps pgtype.Int4
	if l.MaxApplications != nil {
		apps = pgtype.Int4{Int32: *l.MaxApplications, Valid: true}
	}
	var cpu pgtype.Float8
	if l.MaxCPU != nil {
		cpu = pgtype.Float8{Float64: *l.MaxCPU, Valid: true}
	}
	var mem pgtype.Int8
	if l.MaxMemoryMB != nil {
		mem = pgtype.Int8{Int64: *l.MaxMemoryMB, Valid: true}
	}
	return apps, cpu, mem
}

// LimitsFromRow re-exports the internal conversion for tests / handlers.
func LimitsFromRow(row generated.Quota) Limits { return limitsFromRow(row) }

// MaybeAlertProjectThreshold checks whether the project's application-count
// usage has crossed threshold (rising-edge). Returns a non-nil *ThresholdAlert
// when an alert should be sent; nil when no alert is needed.
//
// threshold is the user-configured percent (1–100); pass 80 as the default.
// The function updates quota_alert_state so duplicate alerts are suppressed
// and hysteresis is enforced (state resets when usage drops ≥10pp below threshold).
func (s *Service) MaybeAlertProjectThreshold(ctx context.Context, projectID pgtype.UUID, threshold int) (*ThresholdAlert, error) {
	limits, err := s.LimitsFor(ctx, ScopeProject, projectID)
	if err != nil {
		return nil, fmt.Errorf("get project limits: %w", err)
	}
	if limits.MaxApplications == nil {
		return nil, nil // no application cap — nothing to alert on
	}

	usage, err := s.UsageForProject(ctx, projectID)
	if err != nil {
		return nil, fmt.Errorf("get project usage: %w", err)
	}

	currentPercent := int(usage.Applications * 100 / int64(*limits.MaxApplications))

	state, err := s.q.GetQuotaAlertState(ctx, generated.GetQuotaAlertStateParams{
		Scope:   ScopeProject,
		ScopeID: projectID,
	})
	lastPercent := 0
	if err == nil {
		lastPercent = int(state.LastAlertedPercent)
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("get quota alert state: %w", err)
	}

	fire, reset := evalThreshold(lastPercent, threshold, currentPercent, alertHysteresis)

	if fire || reset {
		newPercent := int32(currentPercent)
		if reset {
			newPercent = 0
		}
		if upsertErr := s.q.UpsertQuotaAlertState(ctx, generated.UpsertQuotaAlertStateParams{
			Scope:              ScopeProject,
			ScopeID:            projectID,
			LastAlertedAt:      pgtype.Timestamptz{Time: time.Now(), Valid: true},
			LastAlertedPercent: newPercent,
		}); upsertErr != nil {
			return nil, fmt.Errorf("update quota alert state: %w", upsertErr)
		}
	}

	if fire {
		return &ThresholdAlert{
			ProjectID:      projectID,
			CurrentPercent: currentPercent,
			Threshold:      threshold,
		}, nil
	}
	return nil, nil
}

// evalThreshold implements the rising-edge + hysteresis alert logic.
// fire: lastPercent < threshold ≤ currentPercent (first crossing)
// reset: currentPercent ≤ threshold-hysteresis (dropped far enough to re-arm)
func evalThreshold(lastPercent, threshold, currentPercent, hysteresis int) (fire, reset bool) {
	fire = lastPercent < threshold && currentPercent >= threshold
	reset = !fire && currentPercent <= threshold-hysteresis
	return
}
