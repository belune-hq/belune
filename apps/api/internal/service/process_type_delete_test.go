package service_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/belune-hq/belune/internal/runtime"
	"github.com/belune-hq/belune/internal/service"
	"github.com/belune-hq/belune/internal/testutil"
)

// Deleting a process type must actually remove its container.
//
// The request runs under the 15s handler timeout, and a stop is slow whenever the
// command ignores SIGTERM (a pipeline or && chain leaves the shell as PID 1): the
// request context is cancelled mid-stop, and the removal that follows used to run
// with that dead context, so the container was left behind as Exited (137) while
// the row was already gone. Here the request context is cancelled DURING the stop,
// which is what the timeout does, and the removal must still happen on a live one.
func TestDeleteProcessType_StillRemovesTheContainerWhenTheRequestContextDiesMidStop(t *testing.T) {
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
	pt, inst, err := svc.CreateProcessType(context.Background(), app.ID, service.ProcessTypeInput{Name: "worker", Command: "a | b"})
	require.NoError(t, err)

	rt.StopFunc = func(_ context.Context, _ string) error {
		cancelReq() // the handler timeout fires while the container is being stopped
		return context.Canceled
	}

	warning, err := svc.DeleteProcessType(reqCtx, app.ID, pt.ID)
	require.NoError(t, err)

	assert.Contains(t, rt.RemoveCalls, inst.ContainerName, "the container must be removed, not left Exited")
	require.NotEmpty(t, rt.RemoveCtxErrs)
	assert.NoError(t, rt.RemoveCtxErrs[len(rt.RemoveCtxErrs)-1], "the removal must run on a context the request's timeout cannot cancel")
	assert.Empty(t, warning, "a removal that succeeded must not warn")

	rows, err := testQueries.ListProcessTypesByApplication(context.Background(), app.ID)
	require.NoError(t, err)
	assert.Len(t, rows, 1, "only the routable type remains")
}
