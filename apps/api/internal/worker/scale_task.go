package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/hibiken/asynq"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/belune-hq/belune/internal/service"
	"github.com/belune-hq/belune/internal/status"
	"github.com/belune-hq/belune/internal/store/generated"
)

// TypeScaleProcessType carries the CONTAINER half of a scale change. The API has
// already moved the instance rows; this stops the surplus containers and creates the
// missing ones.
const TypeScaleProcessType = "process_type:scale"

// ScalePayload is the task's JSON. RemoveContainers are named because their rows are
// already deleted and nothing else remembers them.
type ScalePayload struct {
	ApplicationID    string   `json:"application_id"`
	ProcessTypeID    string   `json:"process_type_id"`
	RemoveContainers []string `json:"remove_containers,omitempty"`
	CreateContainers bool     `json:"create_containers,omitempty"`
}

// NewScaleTask builds the task for a service.ScalePlan.
func NewScaleTask(plan service.ScalePlan) (*asynq.Task, error) {
	body, err := json.Marshal(ScalePayload{
		ApplicationID:    plan.ApplicationID.String(),
		ProcessTypeID:    plan.ProcessTypeID.String(),
		RemoveContainers: plan.RemoveContainers,
		CreateContainers: plan.CreateContainers,
	})
	if err != nil {
		return nil, fmt.Errorf("marshal scale payload: %w", err)
	}
	return asynq.NewTask(TypeScaleProcessType, body), nil
}

// ⛔ scaleStopTimeout must stay ABOVE Docker's stop grace, which StopContainer
// hardcodes at 30s (SIGTERM, wait 30s, SIGKILL). Below it a worker that ignores SIGTERM
// outlives its own stop context. Removal is forced afterwards, so a stop that fails or
// times out never leaves the surplus container behind -- the stop is only the polite
// attempt.
const (
	scaleStopTimeout   = 40 * time.Second
	scaleRemoveTimeout = 15 * time.Second
)

// HandleScaleProcessTask applies a scale change to the containers.
//
// Scale DOWN acts immediately and needs no image: it stops (politely) and force-removes
// the surplus containers. Scale UP creates the new instances' containers from the image
// of the application's last successful deployment; with none, the rows stay pending and
// the containers appear on the next deploy, exactly as for a new process type.
func (h *TaskHandler) HandleScaleProcessTask(ctx context.Context, t *asynq.Task) error {
	var p ScalePayload
	if err := json.Unmarshal(t.Payload(), &p); err != nil {
		return errors.Join(fmt.Errorf("unmarshal scale payload: %w", err), asynq.SkipRetry)
	}
	appID, err := parseUUID(p.ApplicationID)
	if err != nil {
		return errors.Join(fmt.Errorf("invalid application_id: %w", err), asynq.SkipRetry)
	}
	typeID, err := parseUUID(p.ProcessTypeID)
	if err != nil {
		return errors.Join(fmt.Errorf("invalid process_type_id: %w", err), asynq.SkipRetry)
	}

	appRow, err := h.Queries.GetApplicationWithProjectSlug(ctx, appID)
	if err != nil {
		return errors.Join(fmt.Errorf("fetch application: %w", err), asynq.SkipRetry)
	}
	rt, err := h.Runtimes.For(ctx, appRow.ServerID)
	if err != nil {
		return fmt.Errorf("resolve runtime: %w", err)
	}

	// Detached from the task's own context for the same reason the request-path
	// operations are: a cancelled context must not turn a removal into a no-op.
	bg := context.WithoutCancel(ctx)
	for _, name := range p.RemoveContainers {
		stopCtx, cancelStop := context.WithTimeout(bg, scaleStopTimeout)
		if err := rt.StopContainer(stopCtx, name); err != nil {
			slog.Warn("scale down: stop did not complete cleanly (removing anyway)", "container", name, "error", err)
		}
		cancelStop()
		rmCtx, cancelRm := context.WithTimeout(bg, scaleRemoveTimeout)
		if err := rt.RemoveContainer(rmCtx, name); err != nil {
			slog.Warn("scale down: could not remove container", "container", name, "error", err)
		}
		cancelRm()
	}

	if !p.CreateContainers {
		return nil
	}
	return h.scaleUp(ctx, appID, typeID)
}

// scaleUp creates the containers of the type's instances that do not exist yet.
func (h *TaskHandler) scaleUp(ctx context.Context, appID, typeID pgtype.UUID) error {
	dep, err := h.Queries.GetLatestSuccessfulDeployment(ctx, appID)
	if errors.Is(err, pgx.ErrNoRows) {
		slog.Info("scale up: no successful deployment yet; the containers will appear on the next deploy",
			"application_id", appID.String())
		return nil
	}
	if err != nil {
		return fmt.Errorf("latest successful deployment: %w", err)
	}

	dc := &deployContext{
		applicationID: appID,
		deploymentID:  dep.ID,
		payload:       deployPayload{ApplicationID: appID.String(), DeploymentID: dep.ID.String()},
		imageName:     dep.ImageTag.String,
	}
	if err := h.loadApplication(ctx, dc); err != nil {
		return fmt.Errorf("load application: %w", err)
	}
	h.ensureNetworks(ctx, dc)

	pt, err := h.Queries.GetProcessType(ctx, typeID)
	if err != nil {
		return fmt.Errorf("load process type: %w", err)
	}

	var firstErr error
	for _, tg := range dc.all() {
		if tg.typeName != pt.Name {
			continue
		}
		exists, err := dc.runtime.ContainerExists(ctx, tg.containerName)
		if err == nil && exists {
			continue
		}
		fail := func(err error) {
			slog.Error("scale up: could not start instance", "container", tg.containerName, "error", err)
			if _, uerr := h.Queries.UpdateInstanceStatus(ctx, generated.UpdateInstanceStatusParams{
				ID: tg.instanceID, Status: status.ApplicationError,
			}); uerr != nil {
				slog.Warn("scale up: could not mark instance errored", "error", uerr)
			}
			if firstErr == nil {
				firstErr = err
			}
		}
		if err := h.createAndStart(ctx, dc, tg); err != nil {
			fail(err)
			continue
		}
		// A worker with a command health check is gated, as in a deploy. The routable
		// type has no gate here: Caddy's own upstream health check keeps a bad replica
		// out of the pool.
		if !pt.Routable {
			if err := h.verifyWorker(ctx, dc, tg); err != nil {
				fail(err)
				continue
			}
		}
		if _, err := h.Queries.UpdateInstanceContainer(ctx, generated.UpdateInstanceContainerParams{
			ID: tg.instanceID, ContainerName: tg.containerName, DeploymentID: dep.ID,
			ContainerID: pgtype.Text{String: tg.containerID, Valid: tg.containerID != ""}, Status: status.ApplicationRunning,
		}); err != nil {
			slog.Warn("scale up: could not record the container", "container", tg.containerName, "error", err)
		}
	}
	return firstErr
}
