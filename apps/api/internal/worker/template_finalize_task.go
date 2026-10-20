package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/hibiken/asynq"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/belune-hq/belune/internal/status"
	"github.com/belune-hq/belune/internal/store/generated"
)

// templateFinalizePayload drives the async completion of a template
// instantiation. Applications and databases already exist as DB rows (created
// synchronously by the instantiation handler); this task gates app deploys on
// database readiness.
type templateFinalizePayload struct {
	ProjectID   string   `json:"project_id"`
	DatabaseIDs []string `json:"database_ids"`
	AppIDs      []string `json:"app_ids"` // already topologically ordered (dependencies first)
}

// HandleTemplateFinalizeTask waits for every managed database in a template
// instantiation to reach "running", then enqueues each application's first
// deploy in dependency order.
//
// The database-readiness gate is implemented via asynq retries rather than an
// in-task poll loop: if a database is not yet running, the task returns a
// retryable error and asynq re-runs it with backoff, so no worker goroutine is
// held for minutes. Once all databases are ready the deploy enqueues run and the
// task returns nil, so it never retries past that point (no double-deploy).
func (h *TaskHandler) HandleTemplateFinalizeTask(ctx context.Context, t *asynq.Task) error {
	var p templateFinalizePayload
	if err := json.Unmarshal(t.Payload(), &p); err != nil {
		return errors.Join(fmt.Errorf("invalid template finalize payload (permanent): %w", err), asynq.SkipRetry)
	}
	slog.Info("template finalize: start", "project", p.ProjectID, "apps", len(p.AppIDs), "databases", len(p.DatabaseIDs))

	// Gate: every database must be running. A failed database aborts the deploy
	// (SkipRetry) — the user sees the failed database and can retry manually.
	for _, idStr := range p.DatabaseIDs {
		var id pgtype.UUID
		if err := id.Scan(idStr); err != nil {
			return errors.Join(fmt.Errorf("invalid database id %q (permanent): %w", idStr, err), asynq.SkipRetry)
		}
		db, err := h.Queries.GetDatabase(ctx, id)
		if err != nil {
			// Row gone (e.g. project deleted mid-provision) — nothing to finalize.
			slog.Warn("template finalize: database not found, aborting", "database_id", idStr, "error", err)
			return asynq.SkipRetry
		}
		switch db.Status {
		case status.DatabaseRunning:
			// ready
		case status.DatabaseFailed:
			slog.Warn("template finalize: database failed to provision, not deploying apps",
				"database_id", idStr, "project_id", p.ProjectID)
			return asynq.SkipRetry
		default:
			return fmt.Errorf("waiting for database %s (status=%s)", idStr, db.Status)
		}
	}

	// All databases ready: deploy applications in order. Best-effort — log and
	// continue on per-app errors, and always return nil so a partial failure does
	// not retry the whole task and re-deploy already-started apps.
	for _, idStr := range p.AppIDs {
		var id pgtype.UUID
		if err := id.Scan(idStr); err != nil {
			slog.Error("template finalize: invalid application id", "application_id", idStr, "error", err)
			continue
		}
		// Idempotency guard against a worker crash mid-loop: skip apps that
		// already have a deployment.
		if deps, err := h.Queries.ListDeploymentsByApplication(ctx, id); err == nil && len(deps) > 0 {
			slog.Info("template finalize: app already has a deployment, skipping auto-deploy", "application_id", idStr, "deployments", len(deps))
			continue
		}
		slog.Info("template finalize: auto-deploying app", "application_id", idStr)
		if _, err := h.Queries.GetApplication(ctx, id); err != nil {
			slog.Warn("template finalize: application not found, skipping", "application_id", idStr, "error", err)
			continue
		}

		deployment, err := h.Queries.CreateDeployment(ctx, generated.CreateDeploymentParams{
			ApplicationID: id,
			Status:        status.DeploymentPending,
			TriggeredBy:   "template",
		})
		if err != nil {
			slog.Error("template finalize: failed to create deployment", "application_id", idStr, "error", err)
			continue
		}

		payload, err := json.Marshal(deployPayload{
			ApplicationID: idStr,
			DeploymentID:  formatUUID(deployment.ID),
		})
		if err != nil {
			slog.Error("template finalize: failed to marshal deploy payload", "application_id", idStr, "error", err)
			continue
		}
		task := asynq.NewTask(TypeDeploy, payload)
		_, err = h.Enqueuer.Enqueue(task,
			asynq.Queue("critical"),
			asynq.MaxRetry(0),
			asynq.TaskID("deploy:"+idStr),
			asynq.Timeout(time.Duration(h.Config.TaskTimeoutMinutes)*time.Minute),
		)
		if err != nil && !errors.Is(err, asynq.ErrTaskIDConflict) {
			slog.Error("template finalize: failed to enqueue deploy", "application_id", idStr, "error", err)
		}
	}
	return nil
}
