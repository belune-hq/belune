package service

import (
	"context"
	"log/slog"
	"sync"

	"github.com/belune-hq/belune/internal/runtime"
)

// removeContainersBounded takes containers away for a DELETE: a bounded polite stop, then a
// FORCED removal, every container at once. It returns the names that could not be removed.
//
// ⛔ A delete is bounded, not detached-and-202'd. The container is going away regardless, so
// there is no reason to wait out Docker's 30s stop grace for a process that ignores SIGTERM
// (a compound command or pipeline leaves the shell as PID 1). The stop gets processTypeStopGrace
// so a command that does handle SIGTERM can finish what it is doing, and the removal that follows
// is forced, so the stop is not allowed to decide whether the container goes away. Both together
// fit inside the 15s handler window, so the caller gets an answer that means "it is gone".
//
// ⛔ Concurrent, not a loop. The bound is PER CONTAINER (5s + 8s), so run one after another N
// containers cost 13s each and the third is already past the 15s window; run together they cost
// 13s in total. A serial loop is also how a first slow stop used to kill the context for every
// container after it. No shared context: one container's failure never cancels its siblings.
//
// The caller passes a context detached from the request (context.WithoutCancel), so a client that
// disconnects does not strand a half-removed container, and the steps get their own deadlines
// here. A stop that fails does not skip the removal.
func removeContainersBounded(ctx context.Context, rt runtime.ContainerRuntime, names []string, what string) (failed []string) {
	var (
		mu sync.Mutex
		wg sync.WaitGroup
	)
	for _, name := range names {
		wg.Add(1)
		go func() {
			defer wg.Done()
			stopCtx, cancelStop := context.WithTimeout(ctx, processTypeStopGrace)
			if err := rt.StopContainer(stopCtx, name); err != nil {
				slog.Debug(what+": stop did not complete (forcing removal)", "container", name, "error", err)
			}
			cancelStop()

			rmCtx, cancelRm := context.WithTimeout(ctx, processTypeRemoveTimeout)
			defer cancelRm()
			if err := rt.RemoveContainer(rmCtx, name); err != nil {
				slog.Warn(what+": could not remove container", "container", name, "error", err)
				mu.Lock()
				failed = append(failed, name)
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	return failed
}
