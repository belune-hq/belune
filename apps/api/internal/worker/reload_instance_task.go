package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"

	"github.com/hibiken/asynq"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/belune-hq/belune/internal/service"
)

// TypeReloadInstance recreates ONE container from the application's current image and
// config. The API has already validated the instance; this does the container work.
const TypeReloadInstance = "instance:reload"

// ReloadInstancePayload is the task's JSON.
type ReloadInstancePayload struct {
	ApplicationID string `json:"application_id"`
	InstanceID    string `json:"instance_id"`
}

// NewReloadInstanceTask builds the task. The task id is per instance, so a second click on
// the same container while its reload is still queued or running is refused (asynq reports
// the conflict) instead of stopping the container it is about to create. That is the only
// guard: there is deliberately none against a concurrent deploy, which is not specific to
// this action.
func NewReloadInstanceTask(appID, instanceID pgtype.UUID) (*asynq.Task, []asynq.Option, error) {
	body, err := json.Marshal(ReloadInstancePayload{ApplicationID: appID.String(), InstanceID: instanceID.String()})
	if err != nil {
		return nil, nil, fmt.Errorf("marshal reload payload: %w", err)
	}
	return asynq.NewTask(TypeReloadInstance, body), []asynq.Option{
		asynq.MaxRetry(0),
		asynq.TaskID("reload-instance-" + instanceID.String()),
	}, nil
}

// HandleReloadInstanceTask replaces one container: a bounded stop, a forced removal, then
// the same create the deploy path uses, from the latest successful deployment's image and
// the CURRENT config (volumes, env, limits, command).
//
// ⛔ In place, never a roll. The container keeps the instance row's name, so the route's
// upstream is unchanged and nothing here touches the proxy; rolling a single replica would
// be a second implementation of what rollRoutable already does. The price is that THIS
// replica has a gap between the removal and the new container answering: for one of several
// routable replicas the others serve it, for the only one the application is unavailable
// meanwhile (an application-level Reload is the zero-gap roll).
//
// ⛔ The stop is bounded and the removal forced (service.RemoveContainersBounded): a wedged
// container, which is exactly what someone reaches for Reload to fix, ignores SIGTERM and
// would otherwise sit on Docker's 30s stop grace. No request waits on this (it is a task),
// so that is about not stranding the container, not about the handler timeout.
func (h *TaskHandler) HandleReloadInstanceTask(ctx context.Context, t *asynq.Task) error {
	var p ReloadInstancePayload
	if err := json.Unmarshal(t.Payload(), &p); err != nil {
		return errors.Join(fmt.Errorf("unmarshal reload payload: %w", err), asynq.SkipRetry)
	}
	appID, err := parseUUID(p.ApplicationID)
	if err != nil {
		return errors.Join(fmt.Errorf("invalid application_id: %w", err), asynq.SkipRetry)
	}
	instID, err := parseUUID(p.InstanceID)
	if err != nil {
		return errors.Join(fmt.Errorf("invalid instance_id: %w", err), asynq.SkipRetry)
	}
	inst, err := h.Queries.GetInstance(ctx, instID)
	if err != nil || inst.ApplicationID != appID {
		return errors.Join(fmt.Errorf("instance not found"), asynq.SkipRetry)
	}
	pt, err := h.Queries.GetProcessType(ctx, inst.ProcessTypeID)
	if err != nil {
		return errors.Join(fmt.Errorf("load process type: %w", err), asynq.SkipRetry)
	}

	dc, dep, ok, err := h.scaleContext(ctx, appID)
	if err != nil {
		return err
	}
	if !ok {
		return errors.Join(fmt.Errorf("no successful deployment to reload from"), asynq.SkipRetry)
	}
	var tg *deployTarget
	for _, c := range dc.all() {
		if c.instanceID == instID {
			tg = c
			break
		}
	}
	if tg == nil {
		return errors.Join(fmt.Errorf("instance is not part of the application's current containers"), asynq.SkipRetry)
	}

	// Detached: a cancelled task context must not turn the removal into a silent no-op and
	// strand a half-replaced container.
	bg := context.WithoutCancel(ctx)
	if failed := service.RemoveContainersBounded(bg, dc.runtime, []string{tg.containerName}, "reload instance"); len(failed) > 0 {
		return errors.Join(fmt.Errorf("could not remove %s", tg.containerName), asynq.SkipRetry)
	}
	slog.Info("reload instance: recreating", "container", tg.containerName, "application_id", p.ApplicationID)
	return h.startInstance(bg, dc, tg, pt, dep)
}
