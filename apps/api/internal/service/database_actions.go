package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/belune-hq/belune/internal/runtime"
	"github.com/belune-hq/belune/internal/status"
	"github.com/belune-hq/belune/internal/store/generated"
)

// ErrDatabaseNotFound means no such database.
var ErrDatabaseNotFound = errors.New("database not found")

// ⛔ Stop, Start and Restart run their Docker calls on a context DETACHED from the request, with a
// per-step deadline whose stop is ABOVE Docker's 30s grace, for the reason spelled out on
// ApplicationService.Stop: the request is under the 15s handler timeout, a container whose PID 1
// ignores SIGTERM burns the whole grace, and anything issued on the cancelled request context
// afterwards silently does nothing, leaving the database Exited (137).
//
// Belune's own database images happen to trap SIGTERM, which is irrelevant: nothing enforces it,
// and the "Other" engine takes a user-supplied image. A database is one container, so unlike the
// application actions there is nothing to run concurrently.

func (s *DatabaseService) actionTarget(ctx context.Context, dbID pgtype.UUID) (generated.Database, runtime.ContainerRuntime, error) {
	db, err := s.queries.GetDatabase(ctx, dbID)
	if err != nil {
		return generated.Database{}, nil, ErrDatabaseNotFound
	}
	rt, err := RuntimeForDatabase(ctx, s.queries, s.runtimes, dbID)
	if err != nil {
		return generated.Database{}, nil, fmt.Errorf("%w: %v", ErrServerUnreachable, err)
	}
	return db, rt, nil
}

func (s *DatabaseService) recordStatus(ctx context.Context, dbID pgtype.UUID, st string) (generated.Database, error) {
	updated, err := s.queries.UpdateDatabaseStatus(ctx, generated.UpdateDatabaseStatusParams{ID: dbID, Status: st})
	if err != nil {
		return generated.Database{}, fmt.Errorf("%w: %v", ErrStatusUpdate, err)
	}
	return updated, nil
}

// Stop stops the managed database container without removing it, and records it as stopped.
func (s *DatabaseService) Stop(ctx context.Context, dbID pgtype.UUID) (generated.Database, error) {
	bg := context.WithoutCancel(ctx)
	db, rt, err := s.actionTarget(bg, dbID)
	if err != nil {
		return generated.Database{}, err
	}
	// The database container name matches its slug (see provision/delete paths).
	stopCtx, cancel := context.WithTimeout(bg, instanceStopTimeout)
	defer cancel()
	if err := rt.StopContainer(stopCtx, db.Slug); err != nil {
		slog.Error("failed to stop database container", "container", db.Slug, "error", err)
		return generated.Database{}, fmt.Errorf("%w: stopping: %v", ErrContainerAction, err)
	}
	return s.recordStatus(bg, dbID, status.DatabaseStopped)
}

// Start starts a stopped database container and records it as running.
func (s *DatabaseService) Start(ctx context.Context, dbID pgtype.UUID) (generated.Database, error) {
	bg := context.WithoutCancel(ctx)
	db, rt, err := s.actionTarget(bg, dbID)
	if err != nil {
		return generated.Database{}, err
	}
	startCtx, cancel := context.WithTimeout(bg, instanceStartTimeout)
	defer cancel()
	if err := rt.StartContainer(startCtx, db.Slug); err != nil {
		slog.Error("failed to start database container", "container", db.Slug, "error", err)
		return generated.Database{}, fmt.Errorf("%w: starting: %v", ErrContainerAction, err)
	}
	return s.recordStatus(bg, dbID, status.DatabaseRunning)
}

// Restart stops and starts the existing database container (no recreate).
//
// ⛔ The start is ALWAYS attempted, whatever the stop did: the invariant is that the container ends
// up running, and starting a running container is a Docker no-op. The stop's failure is reported
// only if the start fails too.
func (s *DatabaseService) Restart(ctx context.Context, dbID pgtype.UUID) (generated.Database, error) {
	bg := context.WithoutCancel(ctx)
	db, rt, err := s.actionTarget(bg, dbID)
	if err != nil {
		return generated.Database{}, err
	}
	stopCtx, cancelStop := context.WithTimeout(bg, instanceStopTimeout)
	stopErr := rt.StopContainer(stopCtx, db.Slug)
	cancelStop()
	if stopErr != nil {
		slog.Warn("restart database: stop did not complete cleanly (starting anyway)", "container", db.Slug, "error", stopErr)
	}
	startCtx, cancelStart := context.WithTimeout(bg, instanceStartTimeout)
	defer cancelStart()
	if err := rt.StartContainer(startCtx, db.Slug); err != nil {
		slog.Error("failed to start database container for restart", "container", db.Slug, "error", err)
		if stopErr != nil {
			return generated.Database{}, fmt.Errorf("%w: stopping: %v; starting: %v", ErrContainerAction, stopErr, err)
		}
		return generated.Database{}, fmt.Errorf("%w: starting: %v", ErrContainerAction, err)
	}
	return s.recordStatus(bg, dbID, status.DatabaseRunning)
}
