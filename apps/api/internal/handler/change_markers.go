package handler

import (
	"context"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/weiliang79/belune/internal/store/generated"
)

// The application detail page needs to answer "is what is running still what I
// saved?". These two helpers are the write half of that: every handler that
// changes something the running container was built or started with stamps one
// of them, and the deploy worker clears them again on success
// (see worker.clearChangeMarkers).
//
// Pick by asking what it takes to apply the change:
//
//   - markConfigChanged  a reload is enough — the container is recreated from
//     the image it is already running. Env vars, volumes, file mounts,
//     CPU/memory limits, runtime profile, health-check path.
//
//   - markSourceChanged  a real build or pull is required. source_image,
//     dockerfile_path, build_type_override, builder_image, branch,
//     root_directory, git credentials.
//
// Source is the stronger of the two and is reported on its own, so a source
// change does not also need markConfigChanged — stamping both would leave a
// stale "Reload to apply" behind once the deploy cleared only the source
// marker.
//
// Both are best-effort. A failure here means the indicator is stale, which is
// not worth failing the user's save over — the save itself already succeeded.

func (h *Handler) markConfigChanged(ctx context.Context, applicationID pgtype.UUID) {
	h.appService.MarkConfigChanged(ctx, applicationID)
}

func (h *Handler) markSourceChanged(ctx context.Context, applicationID pgtype.UUID) {
	h.appService.MarkSourceChanged(ctx, applicationID)
}

func (h *Handler) markApplicationUpdate(ctx context.Context, before, after generated.Application) {
	h.appService.MarkUpdate(ctx, before, after)
}
