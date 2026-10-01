package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/hibiken/asynq"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/weiliang79/belune/internal/pkg/tracing"
	"github.com/weiliang79/belune/internal/status"
	"github.com/weiliang79/belune/internal/store/generated"
)

// ErrDeployInProgress means another deploy/build/reload/rebuild/rollback holds
// the application's serialising TaskID and is genuinely running.
var ErrDeployInProgress = errors.New("a deployment is already in progress for this application")

// TaskEnqueuer is the slice of *asynq.Client the deploy queue uses; an
// interface so tests (and the handler's own mock) can stand in for it.
type TaskEnqueuer interface {
	Enqueue(task *asynq.Task, opts ...asynq.Option) (*asynq.TaskInfo, error)
}

// TaskDeleter is the slice of *asynq.Inspector needed to reclaim a stale
// TaskID. DeleteTask fails for an active (running) task, which is what lets a
// stale holder be told apart from a genuinely running one.
type TaskDeleter interface {
	DeleteTask(queue, id string) error
}

// DeployQueue owns the one way a deploy-like task reaches the queue. It was
// the Handler's enqueueDeployLike/failDeploymentEnqueue; it lives in a service
// so the REST handlers and the MCP tools share a single set of rules instead
// of each re-deriving the TaskID guard, the no-retry policy and the stale-task
// reclaim. (Not to be confused with DeployService, which nothing in production
// uses and which does NOT carry those rules.)
type DeployQueue struct {
	queries     *generated.Queries
	asynq       TaskEnqueuer
	inspector   TaskDeleter
	taskTimeout time.Duration
}

func NewDeployQueue(queries *generated.Queries, enq TaskEnqueuer, inspector TaskDeleter, taskTimeoutMinutes int) *DeployQueue {
	return &DeployQueue{
		queries:     queries,
		asynq:       enq,
		inspector:   inspector,
		taskTimeout: time.Duration(taskTimeoutMinutes) * time.Minute,
	}
}

// DeployPayload is the JSON payload the deploy worker consumes.
type DeployPayload struct {
	ApplicationID    string            `json:"application_id"`
	DeploymentID     string            `json:"deployment_id"`
	RollbackImageTag string            `json:"rollback_image_tag,omitempty"` // non-empty = skip build, redeploy this image (rollback/reload)
	CommitSHA        string            `json:"commit_sha,omitempty"`         // non-empty = rebuild this exact commit instead of branch HEAD
	TraceCarrier     map[string]string `json:"trace_carrier,omitempty"`
}

// FormatDeploymentID renders a pgtype.UUID as the canonical 8-4-4-4-12 string
// the deploy worker expects in its payload.
func FormatDeploymentID(id pgtype.UUID) string {
	return fmt.Sprintf("%x-%x-%x-%x-%x",
		id.Bytes[0:4], id.Bytes[4:6], id.Bytes[6:8], id.Bytes[8:10], id.Bytes[10:16])
}

// EnqueueLike enqueues task on `queue` under the per-application deploy
// TaskID guard that serialises deploy/build/reload/rebuild/rollback for one app.
// If that TaskID is already held but only by a *stale* task — pending, retry, or
// an archived task left by a previous run that exhausted its retries — the stale
// task is deleted and the new one re-enqueued, so a dead task can't block the
// app from ever deploying again. asynq.Inspector.DeleteTask refuses to remove an
// active (running) task, so a delete failure means a run is genuinely in
// progress and the original ErrTaskIDConflict is returned unchanged.
func (q *DeployQueue) EnqueueLike(queue, applicationID string, task *asynq.Task) error {
	taskID := "deploy:" + applicationID
	opts := []asynq.Option{
		asynq.Queue(queue),
		asynq.Timeout(q.taskTimeout),
		// No automatic retries: a deploy runs exactly once. Deploy failures are
		// almost always deterministic (bad build, bad config), so retrying just
		// re-runs the whole build 3 more times and delays the failure the user
		// needs to see. The user re-triggers manually after fixing the cause.
		asynq.MaxRetry(0),
		asynq.TaskID(taskID),
	}
	_, err := q.asynq.Enqueue(task, opts...)
	if errors.Is(err, asynq.ErrTaskIDConflict) {
		if delErr := q.inspector.DeleteTask(queue, taskID); delErr == nil {
			_, err = q.asynq.Enqueue(task, opts...)
		}
	}
	return err
}

// EnqueueDeploy enqueues the standard deploy task on the critical queue.
func (q *DeployQueue) EnqueueDeploy(applicationID string, payload []byte) error {
	return q.EnqueueLike("critical", applicationID, asynq.NewTask("deploy", payload))
}

// FailEnqueue marks a freshly created deployment row as failed when its task
// could not be queued, so it does not linger in "pending" forever.
func (q *DeployQueue) FailEnqueue(ctx context.Context, deploymentID pgtype.UUID, cause error) {
	if _, err := q.queries.UpdateDeploymentStatus(ctx, generated.UpdateDeploymentStatusParams{
		ID:           deploymentID,
		Status:       status.DeploymentFailed,
		ErrorMessage: pgtype.Text{String: "could not queue deploy task: " + cause.Error(), Valid: true},
	}); err != nil {
		slog.Error("could not mark deployment failed after enqueue error",
			"deployment_id", FormatDeploymentID(deploymentID), "error", err)
	}
}

// TriggerDeployment records a pending deployment for an existing application
// and queues it. The caller authorizes access and checks the application
// exists. A genuinely running deploy surfaces as ErrDeployInProgress, with the
// just-created row already marked failed.
func (q *DeployQueue) TriggerDeployment(ctx context.Context, applicationID pgtype.UUID, triggeredBy string) (generated.Deployment, error) {
	deployment, err := q.queries.CreateDeployment(ctx, generated.CreateDeploymentParams{
		ApplicationID: applicationID,
		Status:        status.DeploymentPending,
		TriggeredBy:   triggeredBy,
	})
	if err != nil {
		return generated.Deployment{}, fmt.Errorf("creating deployment: %w", err)
	}

	appID := uuidToString(applicationID)
	payload, err := json.Marshal(DeployPayload{
		ApplicationID: appID,
		DeploymentID:  FormatDeploymentID(deployment.ID),
		TraceCarrier:  tracing.InjectContext(ctx),
	})
	if err != nil {
		return generated.Deployment{}, fmt.Errorf("marshalling deploy payload: %w", err)
	}

	if err := q.EnqueueDeploy(appID, payload); err != nil {
		q.FailEnqueue(ctx, deployment.ID, err)
		if errors.Is(err, asynq.ErrTaskIDConflict) {
			return generated.Deployment{}, ErrDeployInProgress
		}
		return generated.Deployment{}, fmt.Errorf("enqueueing deploy task: %w", err)
	}
	return deployment, nil
}
