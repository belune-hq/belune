package worker

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/belune-hq/belune/internal/crowdsec"
	"github.com/hibiken/asynq"
)

func (h *TaskHandler) HandleCrowdSecCAPITask(ctx context.Context, task *asynq.Task) error {
	if h.CAPI == nil {
		return fmt.Errorf("central API manager unavailable: %w", asynq.SkipRetry)
	}
	var change crowdsec.CAPIChange
	if err := json.Unmarshal(task.Payload(), &change); err != nil {
		return fmt.Errorf("decode CAPI task: %w", asynq.SkipRetry)
	}
	if change.Enabled == nil && change.Sharing == nil {
		return fmt.Errorf("empty CAPI change: %w", asynq.SkipRetry)
	}
	return h.CAPI.Apply(ctx, change)
}
