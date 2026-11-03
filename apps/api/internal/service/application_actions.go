package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/belune-hq/belune/internal/instances"
	"github.com/belune-hq/belune/internal/status"
	"github.com/belune-hq/belune/internal/store/generated"
)

// The stages a Start or Stop can fail at. Distinct because the REST handler
// reports each with its own message, and a caller choosing what to tell a user
// needs to know whether the container was touched.
var (
	ErrApplicationNotFound = errors.New("application not found")
	ErrServerUnreachable   = errors.New("could not reach the application's server")
	ErrContainerAction     = errors.New("container action failed")
	ErrStatusUpdate        = errors.New("could not record the application's status")
)

// Stop stops the application's existing container and records it as stopped.
// It does not remove the container, its volumes or any data. The caller
// authorizes access.
func (s *ApplicationService) Stop(ctx context.Context, appID pgtype.UUID) (generated.Application, error) {
	row, err := s.queries.GetApplicationWithProjectSlug(ctx, appID)
	if err != nil {
		return generated.Application{}, ErrApplicationNotFound
	}
	rt, err := s.runtimes.For(ctx, row.ServerID)
	if err != nil {
		return generated.Application{}, fmt.Errorf("%w: %v", ErrServerUnreachable, err)
	}

	// Every container of the application. Written as a loop now so 0.3.1's
	// replicas do not have to revisit it; with one instance it is the same call.
	insts, err := instances.ResolveAll(ctx, s.queries, appID)
	if err != nil {
		return generated.Application{}, fmt.Errorf("%w: %v", ErrContainerAction, err)
	}
	for _, inst := range insts {
		if err := rt.StopContainer(ctx, inst.ContainerName); err != nil {
			return generated.Application{}, fmt.Errorf("%w: stopping: %v", ErrContainerAction, err)
		}
	}

	app, err := s.queries.UpdateApplicationStatus(ctx, generated.UpdateApplicationStatusParams{
		ID:     appID,
		Status: status.ApplicationStopped,
	})
	if err != nil {
		return generated.Application{}, fmt.Errorf("%w: %v", ErrStatusUpdate, err)
	}
	return app, nil
}

// Start starts the application's existing container and records it as running.
// It does not build or pull anything, so an application that has never
// deployed has no container to start. The caller authorizes access.
func (s *ApplicationService) Start(ctx context.Context, appID pgtype.UUID) (generated.Application, error) {
	row, err := s.queries.GetApplicationWithProjectSlug(ctx, appID)
	if err != nil {
		return generated.Application{}, ErrApplicationNotFound
	}
	rt, err := s.runtimes.For(ctx, row.ServerID)
	if err != nil {
		return generated.Application{}, fmt.Errorf("%w: %v", ErrServerUnreachable, err)
	}

	insts, err := instances.ResolveAll(ctx, s.queries, appID)
	if err != nil {
		return generated.Application{}, fmt.Errorf("%w: %v", ErrContainerAction, err)
	}
	for _, inst := range insts {
		if err := rt.StartContainer(ctx, inst.ContainerName); err != nil {
			slog.Error("failed to start application container", "container", inst.ContainerName, "error", err)
			return generated.Application{}, fmt.Errorf("%w: starting: %v", ErrContainerAction, err)
		}
	}

	app, err := s.queries.UpdateApplicationStatus(ctx, generated.UpdateApplicationStatusParams{
		ID:     appID,
		Status: status.ApplicationRunning,
	})
	if err != nil {
		return generated.Application{}, fmt.Errorf("%w: %v", ErrStatusUpdate, err)
	}
	return app, nil
}
