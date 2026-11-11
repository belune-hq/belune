package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/belune-hq/belune/internal/instances"
	"github.com/belune-hq/belune/internal/runtime"
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

// ⛔ Stop, Start and Restart run their Docker calls on a context DETACHED from the request.
//
// The request is under the 15s handler timeout (server.handlerTimeout) and Docker's stop
// grace is 30s (StopContainer hardcodes it: SIGTERM, wait 30s, SIGKILL). A container whose
// PID 1 ignores SIGTERM -- a compound command or pipeline leaves the shell as PID 1 --
// burns the whole grace, so the request's context dies mid-stop, and anything issued on it
// afterwards silently does nothing: a Restart used to leave the container Exited (137) for
// good. Every container of the application is handled, so with N of them that ignore SIGTERM
// the stops alone are up to N x 30s if run one after another.
//
// So: the work runs to completion whatever the client does (WithoutCancel), each step has its
// own deadline with the stop's ABOVE Docker's grace (instanceStopTimeout, so the grace expires,
// not us), and the containers are stopped concurrently so N of them cost one grace, not N. The
// caller decides how long to WAIT; the handler answers 202 and lets this finish.
//
// This is the same rule RestartInstance and DeleteProcessType already follow. It was written
// down after the second time it was missed and not applied to its siblings.

// onEachInstance runs fn for every instance concurrently and returns the errors in instance
// order (nil for those that succeeded). There is deliberately no shared context: one
// container's failure must never cancel its siblings.
func onEachInstance(insts []generated.ApplicationInstance, fn func(name string) error) []error {
	errs := make([]error, len(insts))
	var wg sync.WaitGroup
	for i, inst := range insts {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = fn(inst.ContainerName)
		}()
	}
	wg.Wait()
	return errs
}

// firstError is the first non-nil error, with the container it belongs to.
func firstError(insts []generated.ApplicationInstance, errs []error) error {
	for i, err := range errs {
		if err != nil {
			return fmt.Errorf("%s: %w", insts[i].ContainerName, err)
		}
	}
	return nil
}

func stopAll(ctx context.Context, rt runtime.ContainerRuntime, insts []generated.ApplicationInstance) error {
	return firstError(insts, onEachInstance(insts, func(name string) error {
		stopCtx, cancel := context.WithTimeout(ctx, instanceStopTimeout)
		defer cancel()
		return rt.StopContainer(stopCtx, name)
	}))
}

func startAll(ctx context.Context, rt runtime.ContainerRuntime, insts []generated.ApplicationInstance) error {
	return firstError(insts, onEachInstance(insts, func(name string) error {
		startCtx, cancel := context.WithTimeout(ctx, instanceStartTimeout)
		defer cancel()
		return rt.StartContainer(startCtx, name)
	}))
}

// resolveActionTargets is the part of Stop, Start and Restart that is the same: the runtime
// for the application's server and every container it has.
func (s *ApplicationService) resolveActionTargets(ctx context.Context, appID pgtype.UUID) (rt runtime.ContainerRuntime, insts []generated.ApplicationInstance, err error) {
	row, err := s.queries.GetApplicationWithProjectSlug(ctx, appID)
	if err != nil {
		return nil, nil, ErrApplicationNotFound
	}
	rt, err = s.runtimes.For(ctx, row.ServerID)
	if err != nil {
		return nil, nil, fmt.Errorf("%w: %v", ErrServerUnreachable, err)
	}
	insts, err = instances.ResolveAll(ctx, s.queries, appID)
	if err != nil {
		return nil, nil, fmt.Errorf("%w: %v", ErrContainerAction, err)
	}
	return rt, insts, nil
}

func (s *ApplicationService) recordStatus(ctx context.Context, appID pgtype.UUID, st string) (generated.Application, error) {
	app, err := s.queries.UpdateApplicationStatus(ctx, generated.UpdateApplicationStatusParams{ID: appID, Status: st})
	if err != nil {
		return generated.Application{}, fmt.Errorf("%w: %v", ErrStatusUpdate, err)
	}
	return app, nil
}

// Stop stops every container of the application and records it as stopped.
// It does not remove the containers, their volumes or any data. The caller
// authorizes access.
func (s *ApplicationService) Stop(ctx context.Context, appID pgtype.UUID) (generated.Application, error) {
	bg := context.WithoutCancel(ctx)
	rt, insts, err := s.resolveActionTargets(bg, appID)
	if err != nil {
		return generated.Application{}, err
	}
	if err := stopAll(bg, rt, insts); err != nil {
		return generated.Application{}, fmt.Errorf("%w: stopping: %v", ErrContainerAction, err)
	}
	return s.recordStatus(bg, appID, status.ApplicationStopped)
}

// Start starts every existing container of the application and records it as running.
// It does not build or pull anything, so an application that has never
// deployed has no container to start. The caller authorizes access.
func (s *ApplicationService) Start(ctx context.Context, appID pgtype.UUID) (generated.Application, error) {
	bg := context.WithoutCancel(ctx)
	rt, insts, err := s.resolveActionTargets(bg, appID)
	if err != nil {
		return generated.Application{}, err
	}
	if err := startAll(bg, rt, insts); err != nil {
		slog.Error("failed to start application containers", "application_id", uuidToString(appID), "error", err)
		return generated.Application{}, fmt.Errorf("%w: starting: %v", ErrContainerAction, err)
	}
	return s.recordStatus(bg, appID, status.ApplicationRunning)
}

// Restart stops and starts every existing container of the application (no rebuild).
//
// ⛔ The start is ALWAYS attempted, whatever the stop did. The invariant is that the
// containers end up running, so a stop that failed or timed out must not skip it: starting a
// running container is a no-op at the Docker API, so this is safe in every branch. The stop's
// failure is reported only if the start fails too.
func (s *ApplicationService) Restart(ctx context.Context, appID pgtype.UUID) (generated.Application, error) {
	bg := context.WithoutCancel(ctx)
	rt, insts, err := s.resolveActionTargets(bg, appID)
	if err != nil {
		return generated.Application{}, err
	}
	stopErr := stopAll(bg, rt, insts)
	if stopErr != nil {
		slog.Warn("restart application: a stop did not complete cleanly (starting anyway)",
			"application_id", uuidToString(appID), "error", stopErr)
	}
	if err := startAll(bg, rt, insts); err != nil {
		slog.Error("failed to start application containers for restart", "application_id", uuidToString(appID), "error", err)
		if stopErr != nil {
			return generated.Application{}, fmt.Errorf("%w: stopping: %v; starting: %v", ErrContainerAction, stopErr, err)
		}
		return generated.Application{}, fmt.Errorf("%w: starting: %v", ErrContainerAction, err)
	}
	return s.recordStatus(bg, appID, status.ApplicationRunning)
}
