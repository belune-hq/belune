package worker

import (
	"context"
	"log/slog"

	"github.com/belune-hq/belune/internal/pkg/redact"
)

// AutobanEvaluator turns WAF detections into bans when the operator has asked it to.
// Satisfied by *crowdsec.Service.
type AutobanEvaluator interface {
	EvaluateAutoban(ctx context.Context) error
}

// HandleCrowdSecAutoban runs one evaluation. A failure is logged and not retried:
// the next minute's run is the retry, and an asynq retry would only stack a
// second evaluation on top of it.
func (h *TaskHandler) HandleCrowdSecAutoban(ctx context.Context) {
	if h.CrowdSec == nil {
		return
	}
	if err := h.CrowdSec.EvaluateAutoban(ctx); err != nil {
		slog.Warn("crowdsec: automatic ban evaluation failed", "error", redact.Error(err.Error()))
	}
}
