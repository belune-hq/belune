package worker_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/belune-hq/belune/internal/runtime"
	"github.com/belune-hq/belune/internal/service"
	"github.com/belune-hq/belune/internal/status"
	"github.com/belune-hq/belune/internal/store/generated"
	"github.com/belune-hq/belune/internal/testutil"
	"github.com/belune-hq/belune/internal/worker"
)

// addInstances adds rows idx from..to to an existing process type, pending, named as
// the service names them.
func addInstances(t *testing.T, app generated.Application, typeName string, from, to int32) []string {
	t.Helper()
	ctx := context.Background()
	pt, err := testQueries.GetProcessTypeByName(ctx, generated.GetProcessTypeByNameParams{ApplicationID: app.ID, Name: typeName})
	require.NoError(t, err)
	var names []string
	for idx := from; idx <= to; idx++ {
		name := app.Slug + "-" + typeName + "-" + string(rune('0'+idx))
		_, err := testQueries.CreateInstance(ctx, generated.CreateInstanceParams{
			ApplicationID: app.ID, ProcessTypeID: pt.ID, Idx: idx, ContainerName: name, Status: "pending",
		})
		require.NoError(t, err)
		names = append(names, name)
	}
	return names
}

func setReplicas(t *testing.T, app generated.Application, typeName string, n int32) {
	t.Helper()
	_, err := testPool.Exec(context.Background(), `UPDATE application_process_types SET replicas = $1 WHERE application_id = $2 AND name = $3`, n, app.ID, typeName)
	require.NoError(t, err)
}

// ⛔ A paused worker (replicas = 0) has ZERO instance rows, which is its correct state.
// The deploy used to fail any type with no row, so after a pause EVERY deploy failed.
func TestHandleDeployTask_APausedWorkerDoesNotFailTheDeploy(t *testing.T) {
	t.Cleanup(func() { _ = testutil.TruncateAll(context.Background(), testPool) })
	app, dep := seedApp(t)
	_, err := testQueries.CreateProcessType(context.Background(), generated.CreateProcessTypeParams{
		ApplicationID: app.ID, Name: "paused", Command: "run", Replicas: 0,
	})
	require.NoError(t, err)

	rt := &testutil.MockContainerRuntime{CreateFunc: distinctIDs}
	require.NoError(t, deploy(t, app, dep, rt, &testutil.MockProxyManager{}))

	require.Len(t, rt.CreateCalls, 1, "only the routable container; nothing for the paused type")
	assert.Equal(t, app.Slug, rt.CreateCalls[0].Name)
}

// Every replica of every type is created, each with its own idx label and name; the
// route's upstream list is the routable replicas in idx order.
func TestHandleDeployTask_RollsEveryReplicaAndRoutesToAllRoutableOnes(t *testing.T) {
	t.Cleanup(func() { _ = testutil.TruncateAll(context.Background(), testPool) })
	ctx := context.Background()
	app, dep := seedApp(t)
	webRest := addInstances(t, app, "web", 2, 3)
	setReplicas(t, app, "web", 3)
	seedWorkerType(t, app, "worker", "run", nil)
	workerRest := addInstances(t, app, "worker", 2, 2)
	setReplicas(t, app, "worker", 2)
	seedDomain(t, app, "web.example.test")
	_, err := testPool.Exec(ctx, `UPDATE application_process_types SET health_check_type = 'none' WHERE application_id = $1 AND routable`, app.ID)
	require.NoError(t, err)

	rt := &testutil.MockContainerRuntime{CreateFunc: distinctIDs}
	pm := &testutil.MockProxyManager{}
	require.NoError(t, deploy(t, app, dep, rt, pm))

	var created []string
	idxOf := map[string]string{}
	for _, c := range rt.CreateCalls {
		created = append(created, c.Name)
		idxOf[c.Name] = c.Labels[runtime.LabelInstanceIdx]
	}
	assert.ElementsMatch(t, append(append([]string{app.Slug, app.Slug + "-worker-1"}, webRest...), workerRest...), created)
	assert.Equal(t, "3", idxOf[webRest[1]])

	require.Len(t, pm.AddedRoutes, 1)
	assert.Equal(t, []string{
		"http://" + app.Slug + ":8080", "http://" + webRest[0] + ":8080", "http://" + webRest[1] + ":8080",
	}, pm.AddedRoutes[0].Upstreams, "every routable replica, in idx order")

	rows, err := testQueries.ListInstancesByApplication(ctx, app.ID)
	require.NoError(t, err)
	require.Len(t, rows, 5)
	for _, r := range rows {
		assert.Equal(t, status.ApplicationRunning, r.Status, r.ContainerName)
		assert.Equal(t, "id-"+r.ContainerName, r.ContainerID.String)
	}
}

// A type's own limits win; NULL inherits the application's.
func TestHandleDeployTask_PerTypeLimitsOverrideAndNullInheritsTheApplications(t *testing.T) {
	t.Cleanup(func() { _ = testutil.TruncateAll(context.Background(), testPool) })
	ctx := context.Background()
	app, dep := seedApp(t, func(a *generated.CreateApplicationParams, _ *generated.CreateDeploymentParams) {
		a.CpuLimit, a.MemoryLimit = 2, 1<<30
	})
	own := seedWorkerType(t, app, "heavy", "run", nil)
	inherit := seedWorkerType(t, app, "light", "run", nil)
	_, err := testPool.Exec(ctx, `UPDATE application_process_types SET cpu_limit = 0.25, memory_limit = 134217728 WHERE application_id = $1 AND name = 'heavy'`, app.ID)
	require.NoError(t, err)

	rt := &testutil.MockContainerRuntime{CreateFunc: distinctIDs}
	require.NoError(t, deploy(t, app, dep, rt, &testutil.MockProxyManager{}))

	by := map[string]runtime.ContainerConfig{}
	for _, c := range rt.CreateCalls {
		by[c.Name] = c
	}
	assert.InDelta(t, 0.25, by[own].CPULimit, 0.0001)
	assert.EqualValues(t, 134217728, by[own].MemoryLimit)
	assert.InDelta(t, 2, by[inherit].CPULimit, 0.0001, "NULL inherits the application's")
	assert.EqualValues(t, 1<<30, by[inherit].MemoryLimit)
	assert.InDelta(t, 2, by[app.Slug].CPULimit, 0.0001)
}

func scaleTask(t *testing.T, app generated.Application, typeName string, remove []string, create bool) error {
	t.Helper()
	pt, err := testQueries.GetProcessTypeByName(context.Background(), generated.GetProcessTypeByNameParams{ApplicationID: app.ID, Name: typeName})
	require.NoError(t, err)
	task, err := worker.NewScaleTask(service.ScalePlan{
		ApplicationID: app.ID, ProcessTypeID: pt.ID, RemoveContainers: remove, CreateContainers: create,
	})
	require.NoError(t, err)
	return newTestHandler(scaleRT, scalePM).HandleScaleProcessTask(context.Background(), task)
}

var (
	scaleRT *testutil.MockContainerRuntime
	scalePM = &testutil.MockProxyManager{}
)

func markSuccessful(t *testing.T, dep generated.Deployment) {
	t.Helper()
	_, err := testPool.Exec(context.Background(), `UPDATE deployments SET status = 'success', image_tag = 'img:good', started_at = NOW() WHERE id = $1`, dep.ID)
	require.NoError(t, err)
}

// Scale UP 1 -> 3: the two new rows get containers from the last successful
// deployment's image; the existing one is left alone.
func TestScaleTask_ScaleUpCreatesOnlyTheMissingContainersFromTheLastImage(t *testing.T) {
	t.Cleanup(func() { _ = testutil.TruncateAll(context.Background(), testPool) })
	ctx := context.Background()
	app, dep := seedApp(t)
	markSuccessful(t, dep)
	seedWorkerType(t, app, "worker", "run", nil)
	added := addInstances(t, app, "worker", 2, 3)
	setReplicas(t, app, "worker", 3)

	scaleRT = &testutil.MockContainerRuntime{CreateFunc: distinctIDs, MissingNames: map[string]bool{added[0]: true, added[1]: true}}
	require.NoError(t, scaleTask(t, app, "worker", nil, true))

	var created []string
	for _, c := range scaleRT.CreateCalls {
		created = append(created, c.Name)
		assert.Equal(t, "img:good", c.Image, "the last successful deployment's image")
		assert.Equal(t, "worker", c.Labels[runtime.LabelProcessType])
	}
	assert.Equal(t, added, created, "idx 2 and 3 only; idx 1 exists and is untouched")

	pt, _ := testQueries.GetProcessTypeByName(ctx, generated.GetProcessTypeByNameParams{ApplicationID: app.ID, Name: "worker"})
	rows, err := testQueries.ListInstancesByProcessType(ctx, pt.ID)
	require.NoError(t, err)
	require.Len(t, rows, 3)
	assert.Equal(t, "pending", rows[0].Status, "idx 1 untouched")
	assert.False(t, rows[0].DeploymentID.Valid, "a row nothing created stays NULL, and nothing breaks")
	for _, r := range rows[1:] {
		assert.Equal(t, dep.ID, r.DeploymentID, "scale-up records the deployment whose image it ran")
		assert.Equal(t, status.ApplicationRunning, r.Status)
		assert.Equal(t, "id-"+r.ContainerName, r.ContainerID.String)
	}
}

// With nothing ever deployed there is no image: the rows stay pending and the
// containers appear on the next deploy, the CreateProcessType precedent.
func TestScaleTask_ScaleUpWithNoSuccessfulDeploymentCreatesNothing(t *testing.T) {
	t.Cleanup(func() { _ = testutil.TruncateAll(context.Background(), testPool) })
	app, _ := seedApp(t)
	seedWorkerType(t, app, "worker", "run", nil)
	addInstances(t, app, "worker", 2, 2)

	scaleRT = &testutil.MockContainerRuntime{CreateFunc: distinctIDs}
	require.NoError(t, scaleTask(t, app, "worker", nil, true))
	assert.Empty(t, scaleRT.CreateCalls)
}

// Scale DOWN removes the surplus even when the polite stop fails or times out: the
// stop is only the attempt, removal is forced on a live context.
func TestScaleTask_ScaleDownRemovesTheSurplusEvenWhenTheStopTimesOut(t *testing.T) {
	t.Cleanup(func() { _ = testutil.TruncateAll(context.Background(), testPool) })
	app, _ := seedApp(t)
	seedWorkerType(t, app, "worker", "a | b", nil)

	scaleRT = &testutil.MockContainerRuntime{
		StopFunc: func(ctx context.Context, _ string) error {
			time.Sleep(30 * time.Millisecond) // a slow stop
			return context.DeadlineExceeded   // that outlives its deadline
		},
	}
	surplus := []string{app.Slug + "-worker-2", app.Slug + "-worker-3"}
	require.NoError(t, scaleTask(t, app, "worker", surplus, false))

	assert.Equal(t, surplus, scaleRT.StopCalls)
	assert.Equal(t, surplus, scaleRT.RemoveCalls, "removed regardless of the stop")
	for _, e := range scaleRT.RemoveCtxErrs {
		assert.NoError(t, e, "on a live context")
	}
}
