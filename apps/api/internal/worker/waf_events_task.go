package worker

import (
	"context"
	"log/slog"
	"time"

	"github.com/belune-hq/belune/internal/waf"
)

// HandleWAFEventsSweep enforces the detection table's retention and row cap.
// Registered @every 10m: the cap exists for the moment a flood arrives, and a
// daily sweep would let the table grow for a day before noticing.
func (h *TaskHandler) HandleWAFEventsSweep(ctx context.Context) {
	aged, trimmed, err := waf.Sweep(ctx, h.Queries, time.Now())
	if err != nil {
		slog.Warn("waf: events sweep failed", "error", err)
		return
	}
	if aged+trimmed > 0 {
		slog.Info("waf: events sweep completed", "past_retention", aged, "over_cap", trimmed)
	}
}
