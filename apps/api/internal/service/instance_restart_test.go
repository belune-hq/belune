package service_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/belune-hq/belune/internal/runtime"
	"github.com/belune-hq/belune/internal/service"
	"github.com/belune-hq/belune/internal/testutil"
)

// ⛔ The trap DeleteProcessType fell into, for restart: a stop outlives the 15s
// handler timeout whenever the command ignores SIGTERM, the request context is
// cancelled mid-stop, and a start issued on that dead context silently does nothing,
// leaving the container down. Here the request context dies DURING a slow stop and
// the start must still happen, on a live context, and the other container is untouched.
func TestRestartInstance_StillStartsTheContainerWhenTheRequestContextDiesMidStop(t *testing.T) {
	t.Cleanup(func() { truncate(t) })
	reqCtx, cancelReq := context.WithCancel(context.Background())
	defer cancelReq()

	_, project := seedUserAndProject(t)
	rt := &testutil.MockContainerRuntime{}
	svc := service.NewApplicationService(testPool, testQueries, runtime.NewLocalRuntimes(rt), testKeyring, t.TempDir(), nil)
	app, err := svc.Create(context.Background(), service.CreateApplicationParams{
		ProjectID: project.ID, ProjectSlug: project.Slug, Name: "A", BaseSlug: "a",
		Type: "image", SourceImage: "nginx:alpine", BuildType: "image",
	})
	require.NoError(t, err)
	_, workerInsts, _, err := svc.CreateProcessType(context.Background(), app.ID, service.ProcessTypeInput{Name: "worker", Command: "a | b"})
	require.NoError(t, err)
	worker := workerInsts[0]

	rt.StopFunc = func(ctx context.Context, _ string) error {
		cancelReq()                       // the handler timeout fires while the container is being stopped
		time.Sleep(50 * time.Millisecond) // a slow stop
		return ctx.Err()                  // the stop's OWN context must not have been cancelled with the request
	}

	_, err = svc.RestartInstance(reqCtx, app.ID, worker.ID)
	require.NoError(t, err)

	assert.Equal(t, []string{worker.ContainerName}, rt.StopCalls, "only that container is stopped")
	assert.Equal(t, []string{worker.ContainerName}, rt.StartCalls, "the start must follow the slow stop")
	require.NotEmpty(t, rt.StartCtxErrs)
	assert.NoError(t, rt.StartCtxErrs[0], "the start must run on a context the request's timeout cannot cancel")
}

func TestRestartInstance_AnInstanceOfAnotherApplicationIsNotFound(t *testing.T) {
	t.Cleanup(func() { truncate(t) })
	_, project := seedUserAndProject(t)
	rt := &testutil.MockContainerRuntime{}
	svc := service.NewApplicationService(testPool, testQueries, runtime.NewLocalRuntimes(rt), testKeyring, t.TempDir(), nil)
	mk := func(name, slug string) service.CreateApplicationParams {
		return service.CreateApplicationParams{ProjectID: project.ID, ProjectSlug: project.Slug, Name: name, BaseSlug: slug,
			Type: "image", SourceImage: "nginx:alpine", BuildType: "image"}
	}
	a, err := svc.Create(context.Background(), mk("A", "a"))
	require.NoError(t, err)
	b, err := svc.Create(context.Background(), mk("B", "b"))
	require.NoError(t, err)
	_, bInstInsts, _, err := svc.CreateProcessType(context.Background(), b.ID, service.ProcessTypeInput{Name: "worker", Command: "run"})
	require.NoError(t, err)
	bInst := bInstInsts[0]

	_, err = svc.RestartInstance(context.Background(), a.ID, bInst.ID)
	assert.ErrorIs(t, err, service.ErrInstanceNotFound)
	assert.Empty(t, rt.StopCalls)
}

// The stop ITSELF outliving its own context: Docker's grace is 30s, and a worker that
// ignores SIGTERM takes all of it. If the stop's deadline is below that, the stop
// returns a context error -- and the container used to be left down. The start must
// follow regardless, and the result must be a running container with no error.
func TestRestartInstance_StillStartsWhenTheStopFailsOrTimesOut(t *testing.T) {
	for name, stop := range map[string]func(context.Context, string) error{
		"deadline exceeded": func(context.Context, string) error { return context.DeadlineExceeded },
		"daemon error":      func(context.Context, string) error { return errors.New("docker: stop failed") },
	} {
		t.Run(name, func(t *testing.T) {
			t.Cleanup(func() { truncate(t) })
			_, project := seedUserAndProject(t)
			rt := &testutil.MockContainerRuntime{StopFunc: stop}
			svc := service.NewApplicationService(testPool, testQueries, runtime.NewLocalRuntimes(rt), testKeyring, t.TempDir(), nil)
			app, err := svc.Create(context.Background(), service.CreateApplicationParams{
				ProjectID: project.ID, ProjectSlug: project.Slug, Name: "A", BaseSlug: "a",
				Type: "image", SourceImage: "nginx:alpine", BuildType: "image",
			})
			require.NoError(t, err)
			_, workerInsts, _, err := svc.CreateProcessType(context.Background(), app.ID, service.ProcessTypeInput{Name: "worker", Command: "a && b"})
			require.NoError(t, err)
			worker := workerInsts[0]

			_, err = svc.RestartInstance(context.Background(), app.ID, worker.ID)

			require.NoError(t, err, "the operator got what they asked for: the container is running")
			assert.Equal(t, []string{worker.ContainerName}, rt.StartCalls, "the start follows a stop that failed")
		})
	}
}

// A container that no longer exists cannot be restarted; that is a redeploy, not a
// generic failure.
func TestRestartInstance_ARemovedContainerSaysSo(t *testing.T) {
	t.Cleanup(func() { truncate(t) })
	_, project := seedUserAndProject(t)
	rt := &testutil.MockContainerRuntime{
		StopErr: errors.New("No such container"), StartErr: errors.New("No such container"), ContainerMissing_: true,
	}
	svc := service.NewApplicationService(testPool, testQueries, runtime.NewLocalRuntimes(rt), testKeyring, t.TempDir(), nil)
	app, err := svc.Create(context.Background(), service.CreateApplicationParams{
		ProjectID: project.ID, ProjectSlug: project.Slug, Name: "A", BaseSlug: "a",
		Type: "image", SourceImage: "nginx:alpine", BuildType: "image",
	})
	require.NoError(t, err)
	_, workerInsts, _, err := svc.CreateProcessType(context.Background(), app.ID, service.ProcessTypeInput{Name: "worker", Command: "run"})
	require.NoError(t, err)
	worker := workerInsts[0]

	_, err = svc.RestartInstance(context.Background(), app.ID, worker.ID)
	assert.ErrorIs(t, err, service.ErrInstanceContainerGone)
}
