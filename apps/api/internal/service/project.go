package service

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/belune-hq/belune/internal/naming"
	"github.com/belune-hq/belune/internal/runtime"
	"github.com/belune-hq/belune/internal/store/generated"
)

type ProjectService struct {
	queries   *generated.Queries
	runtimes  runtime.Runtimes
	apps      *ApplicationService
	databases *DatabaseService
}

func NewProjectService(queries *generated.Queries, rts runtime.Runtimes, apps *ApplicationService, databases *DatabaseService) *ProjectService {
	return &ProjectService{queries: queries, runtimes: rts, apps: apps, databases: databases}
}

// Delete tears down an entire project: every application and database is removed
// through its own service delete (so containers, data volumes, build caches,
// file mounts, and cascaded rows are reclaimed exactly as an individual delete
// would), then the per-project Docker network is removed and the project row
// deleted. Every step is best-effort so one failure can't strand the project.
func (s *ProjectService) Delete(ctx context.Context, projectID pgtype.UUID) error {
	project, err := s.queries.GetProject(ctx, projectID)
	if err != nil {
		slog.Error("failed to fetch project for cleanup", "project_id", fmt.Sprintf("%v", projectID), "error", err)
		// Still attempt the row delete even if we can't clean up resources.
		return s.queries.DeleteProject(ctx, projectID)
	}

	if applications, err := s.queries.ListApplicationsByProject(ctx, projectID); err == nil {
		for _, app := range applications {
			if err := s.apps.Delete(ctx, app.ID, project.Slug, app.Slug); err != nil {
				slog.Warn("could not delete application during project deletion",
					"application_id", uuidToString(app.ID), "error", err)
			}
		}
	}

	if databases, err := s.queries.ListDatabasesByProject(ctx, projectID); err == nil {
		for _, db := range databases {
			// Purge, not keep: a tombstone is project-scoped and cascades from
			// the project, so there is nowhere for a kept backup to live once
			// the project is gone. This is what the dialog already promises.
			if err := s.databases.Delete(ctx, db.ID, false); err != nil {
				slog.Warn("could not delete database during project deletion",
					"database_id", uuidToString(db.ID), "error", err)
			}
		}
	}

	// ⚠️ Databases deleted EARLIER, whose backups were kept, are not in the list
	// above — they no longer exist. Their tombstones cascade with the project
	// and take the backup rows with them, so without this nothing would ever
	// erase the archives: the objects would survive with no row recording their
	// keys, unreachable and still billed. That is precisely the leak this
	// release fixes on the application path, and the delete dialog promises
	// these are destroyed.
	if orphans, err := s.queries.ListOrphanedBackupsByProject(ctx, projectID); err != nil {
		slog.Warn("could not list kept backups during project deletion",
			"project_id", uuidToString(projectID), "error", err)
	} else {
		for _, o := range orphans {
			if err := s.databases.DeleteOrphanedBackup(ctx, o.ID); err != nil {
				slog.Warn("could not delete a kept backup during project deletion",
					"backup_id", uuidToString(o.ID), "error", err)
			}
		}
	}

	// Remove the per-project Docker network (RemoveNetwork force-disconnects Caddy
	// and the control plane, which are bridged in). Best-effort: a leftover empty
	// network is cosmetic, so a failure must not block the row delete.
	netName := naming.ProjectNetworkName(project.Slug)
	if rt, err := s.runtimes.For(ctx, project.ServerID); err != nil {
		// Best-effort like the removal it guards: the applications and databases
		// are already gone by now, so refusing here would strand the project row
		// with nothing in it until the host came back.
		slog.Warn("could not reach the project's server to remove its network", "network", netName, "error", err)
	} else if err := rt.RemoveNetwork(ctx, netName); err != nil {
		slog.Warn("could not remove project network during project deletion", "network", netName, "error", err)
	}

	return s.queries.DeleteProject(ctx, projectID)
}
