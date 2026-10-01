package service

import (
	"bytes"
	"context"
	"log/slog"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/weiliang79/belune/internal/store/generated"
)

// The change-marker rules (which edits need a reload, which need a build) are
// documented at the top of internal/handler/change_markers.go, whose helpers
// delegate here so the REST handlers and the MCP tools stamp identically.

// MarkConfigChanged stamps "a reload is enough". Best-effort: a failure leaves
// the indicator stale, which is not worth failing the user's save over.
func (s *ApplicationService) MarkConfigChanged(ctx context.Context, applicationID pgtype.UUID) {
	if err := s.queries.TouchApplicationConfigChanged(ctx, applicationID); err != nil {
		slog.Warn("could not mark application config changed", "error", err, "application_id", uuidToString(applicationID))
	}
}

// MarkSourceChanged stamps "a real build or pull is required". Best-effort.
func (s *ApplicationService) MarkSourceChanged(ctx context.Context, applicationID pgtype.UUID) {
	if err := s.queries.TouchApplicationSourceChanged(ctx, applicationID); err != nil {
		slog.Warn("could not mark application source changed", "error", err, "application_id", uuidToString(applicationID))
	}
}

// MarkUpdate handles the one write path that spans both categories.
// It diffs before against after rather than inspecting the request, because the
// service can override what was asked for — a preview child keeps its own
// branch no matter what the request said — and stamping a marker for a field
// that did not actually move would show an indicator the user cannot clear by
// doing what it asks.
//
// A no-op save (open the form, hit Save, change nothing) therefore stamps
// nothing, which is the behaviour that keeps the indicator trustworthy.
func (s *ApplicationService) MarkUpdate(ctx context.Context, before, after generated.Application) {
	sourceChanged := before.SourceRepo != after.SourceRepo ||
		before.SourceImage != after.SourceImage ||
		before.DockerfilePath != after.DockerfilePath ||
		before.BuildTypeOverride != after.BuildTypeOverride ||
		before.BuilderImage != after.BuilderImage ||
		before.Branch != after.Branch ||
		before.GitIntegrationID != after.GitIntegrationID ||
		before.RootDirectory != after.RootDirectory ||
		!bytes.Equal(before.GitCredentialsEncrypted, after.GitCredentialsEncrypted)

	// Not auto_deploy_branch: it only filters which pushes trigger a deploy, so
	// changing it alone needs no deploy to take effect. It moves in lockstep
	// with branch anyway, which is covered above.
	configChanged := before.CpuLimit != after.CpuLimit ||
		before.MemoryLimit != after.MemoryLimit ||
		before.HealthCheckPath != after.HealthCheckPath

	switch {
	case sourceChanged:
		s.MarkSourceChanged(ctx, after.ID)
	case configChanged:
		s.MarkConfigChanged(ctx, after.ID)
	}
}
