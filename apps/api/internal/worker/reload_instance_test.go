package worker_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/belune-hq/belune/internal/status"
	"github.com/belune-hq/belune/internal/store/generated"
	"github.com/belune-hq/belune/internal/testutil"
	"github.com/belune-hq/belune/internal/worker"
)

func reloadTask(t *testing.T, app generated.Application, instanceName string, rt *testutil.MockContainerRuntime, pm *testutil.MockProxyManager) error {
	t.Helper()
	insts, err := testQueries.ListInstancesByApplication(context.Background(), app.ID)
	require.NoError(t, err)
	for _, in := range insts {
		if in.ContainerName == instanceName {
			task, _, err := worker.NewReloadInstanceTask(app.ID, in.ID)
			require.NoError(t, err)
			return newTestHandler(rt, pm).HandleReloadInstanceTask(context.Background(), task)
		}
	}
	t.Fatalf("no instance named %q", instanceName)
	return nil
}

// Reloading a worker replaces ONLY that container: stopped, removed, created again under
// the SAME name from the last successful deployment's image, and the row is repointed.
// Assert the end state -- what ran, what did not, what the row says -- not that it returned.
func TestReloadInstance_ReplacesOnlyThatWorkerInPlace(t *testing.T) {
	t.Cleanup(func() { _ = testutil.TruncateAll(context.Background(), testPool) })
	ctx := context.Background()
	app, dep := seedApp(t)
	markSuccessful(t, dep)
	seedWorkerType(t, app, "worker", "run", nil)
	two := addInstances(t, app, "worker", 2, 2)
	one := app.Slug + "-worker-1"

	rt := &testutil.MockContainerRuntime{CreateFunc: distinctIDs}
	pm := &testutil.MockProxyManager{}
	require.NoError(t, reloadTask(t, app, two[0], rt, pm))

	assert.Equal(t, []string{two[0]}, rt.StopCalls, "only the reloaded container is stopped")
	assert.Equal(t, []string{two[0]}, rt.RemoveCalls, "and force-removed")
	require.Len(t, rt.CreateCalls, 1)
	assert.Equal(t, two[0], rt.CreateCalls[0].Name, "recreated under the row's name: nothing is minted outside a roll")
	assert.Equal(t, "img:good", rt.CreateCalls[0].Image, "from the last successful deployment's image")
	assert.Empty(t, pm.AddedRoutes, "no route is touched")

	rows, err := testQueries.ListInstancesByApplication(ctx, app.ID)
	require.NoError(t, err)
	by := map[string]generated.ApplicationInstance{}
	for _, r := range rows {
		by[r.ContainerName] = r
	}
	assert.Equal(t, "id-"+two[0], by[two[0]].ContainerID.String, "the row points at the new container")
	assert.Equal(t, status.ApplicationRunning, by[two[0]].Status)
	assert.Equal(t, dep.ID, by[two[0]].DeploymentID, "still the deployment that built the image, so log grouping stays true")
	assert.False(t, by[one].ContainerID.Valid, "the sibling is untouched")
}

// One routable replica of several: its siblings are not touched and the route is not
// rewritten (the name is unchanged, so the upstream is). This is the case that would have
// needed a roll, and does not.
func TestReloadInstance_OneRoutableReplicaLeavesTheRouteAndTheOthersAlone(t *testing.T) {
	t.Cleanup(func() { _ = testutil.TruncateAll(context.Background(), testPool) })
	app, dep := seedApp(t)
	markSuccessful(t, dep)
	rest := addInstances(t, app, "web", 2, 3)
	setReplicas(t, app, "web", 3)
	seedDomain(t, app, "web.example.test")

	rt := &testutil.MockContainerRuntime{CreateFunc: distinctIDs}
	pm := &testutil.MockProxyManager{}
	require.NoError(t, reloadTask(t, app, rest[0], rt, pm))

	assert.Equal(t, []string{rest[0]}, rt.StopCalls)
	assert.Equal(t, []string{rest[0]}, rt.RemoveCalls)
	require.Len(t, rt.CreateCalls, 1)
	assert.Equal(t, rest[0], rt.CreateCalls[0].Name)
	assert.Empty(t, pm.AddedRoutes, "no roll, no route change")
	assert.Empty(t, pm.RemovedRoutes)
}

// A wedged container (the reason anyone reaches for Reload) ignores SIGTERM. The stop is
// bounded and the removal forced, so it is still replaced, and promptly.
func TestReloadInstance_AWedgedContainerIsStillReplaced(t *testing.T) {
	t.Cleanup(func() { _ = testutil.TruncateAll(context.Background(), testPool) })
	app, dep := seedApp(t)
	markSuccessful(t, dep)
	seedWorkerType(t, app, "worker", "run", nil)
	name := app.Slug + "-worker-1"

	rt := &testutil.MockContainerRuntime{
		CreateFunc: distinctIDs,
		StopFunc: func(ctx context.Context, _ string) error {
			<-ctx.Done() // never stops on its own: only the deadline ends it
			return ctx.Err()
		},
	}
	start := time.Now()
	require.NoError(t, reloadTask(t, app, name, rt, &testutil.MockProxyManager{}))

	assert.Less(t, time.Since(start), 25*time.Second, "the stop is bounded, not Docker's 30s grace")
	assert.Equal(t, []string{name}, rt.RemoveCalls, "removed regardless of the stop")
	require.Len(t, rt.CreateCalls, 1)
	assert.Equal(t, name, rt.CreateCalls[0].Name)
}

// With nothing ever deployed there is no image to recreate from. The container must NOT
// have been removed first: a reload that destroys what it cannot replace is worse than
// refusing.
func TestReloadInstance_WithNoSuccessfulDeploymentDestroysNothing(t *testing.T) {
	t.Cleanup(func() { _ = testutil.TruncateAll(context.Background(), testPool) })
	app, _ := seedApp(t)
	seedWorkerType(t, app, "worker", "run", nil)

	rt := &testutil.MockContainerRuntime{CreateFunc: distinctIDs}
	err := reloadTask(t, app, app.Slug+"-worker-1", rt, &testutil.MockProxyManager{})

	require.Error(t, err)
	assert.Empty(t, rt.StopCalls)
	assert.Empty(t, rt.RemoveCalls)
	assert.Empty(t, rt.CreateCalls)
}

// A failed removal does not go on to create a second container beside it.
func TestReloadInstance_AFailedRemovalCreatesNothing(t *testing.T) {
	t.Cleanup(func() { _ = testutil.TruncateAll(context.Background(), testPool) })
	app, dep := seedApp(t)
	markSuccessful(t, dep)
	seedWorkerType(t, app, "worker", "run", nil)

	rt := &testutil.MockContainerRuntime{
		CreateFunc: distinctIDs,
		RemoveFunc: func(context.Context, string) error { return context.DeadlineExceeded },
	}
	err := reloadTask(t, app, app.Slug+"-worker-1", rt, &testutil.MockProxyManager{})

	require.Error(t, err)
	assert.Empty(t, rt.CreateCalls)
}
