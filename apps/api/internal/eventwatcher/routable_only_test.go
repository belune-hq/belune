package eventwatcher

import (
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/belune-hq/belune/internal/proxy"
	"github.com/belune-hq/belune/internal/runtime"
	"github.com/belune-hq/belune/internal/status"
	"github.com/belune-hq/belune/internal/store/generated"
	"github.com/belune-hq/belune/internal/testutil"
)

// addWorker gives the fixture's application a second, non-routable process type
// with its own instance, named as B3 names them.
func (f fixture) addWorker(t *testing.T, instStatus string) generated.ApplicationInstance {
	t.Helper()
	pt, err := f.q.CreateProcessType(f.ctx, generated.CreateProcessTypeParams{
		ApplicationID: f.app.ID, Name: "worker", Command: "run-worker", Replicas: 1,
	})
	require.NoError(t, err)
	in, err := f.q.CreateInstance(f.ctx, generated.CreateInstanceParams{
		ApplicationID: f.app.ID, ProcessTypeID: pt.ID, Idx: 1,
		ContainerName: f.app.Slug + "-worker-1", Status: instStatus,
	})
	require.NoError(t, err)
	return in
}

func (f fixture) workerEvent(ev string, extra map[string]string) runtime.ContainerEvent {
	labels := map[string]string{
		labelApplicationID:       pgUUIDToString(f.app.ID),
		runtime.LabelProcessType: "worker",
		runtime.LabelInstanceIdx: "1",
	}
	for k, v := range extra {
		labels[k] = v
	}
	return runtime.ContainerEvent{ContainerID: "worker-cid", ContainerName: f.app.Slug + "-worker-1", Status: ev, Labels: labels}
}

func (f fixture) statusOf(t *testing.T, in generated.ApplicationInstance) string {
	t.Helper()
	got, err := f.q.GetInstance(f.ctx, in.ID)
	require.NoError(t, err)
	return got.Status
}

func (f fixture) runningApp(t *testing.T) {
	t.Helper()
	f.setAppStatus(t, status.ApplicationRunning)
	_, err := f.q.UpdateInstanceStatus(f.ctx, generated.UpdateInstanceStatusParams{ID: f.inst.ID, Status: status.ApplicationRunning})
	require.NoError(t, err)
}

// ⛔ THE RELEASE BLOCKER. Every container of an application carries application-id,
// so a worker stopping used to mark the WHOLE application stopped. buildExpected then
// skipped the application, the sweep deleted its routes, and every domain fell
// through to the dashboard. This reproduces that end to end: the worker's stop event,
// then a real reconciler pass over a real domain.
func TestWorkerStopDoesNotStopTheApplicationOrDropItsRoutes(t *testing.T) {
	f := seed(t)
	f.runningApp(t)
	worker := f.addWorker(t, status.ApplicationRunning)
	_, err := f.q.CreateDomain(f.ctx, generated.CreateDomainParams{
		ApplicationID: f.app.ID, Hostname: "app.example.test", Path: "/", SslMode: "off",
		ContainerPort: pgtype.Int4{Int32: 8080, Valid: true},
	})
	require.NoError(t, err)

	f.w.handleEvent(f.ctx, f.workerEvent("stop", nil))

	assert.Equal(t, status.ApplicationRunning, f.appStatus(t), "a worker stopping must not stop the application")
	assert.Equal(t, status.ApplicationStopped, f.statusOf(t, worker), "the worker's own instance records it")
	assert.Equal(t, status.ApplicationRunning, f.instStatus(t), "and the routable instance is untouched")

	pm := &testutil.MockProxyManager{}
	require.NoError(t, proxy.NewReconciler(f.q, pm, nil, time.Second).ReconcileNow(f.ctx))
	var hosts []string
	for _, r := range pm.AddedRoutes {
		hosts = append(hosts, r.Hostname)
	}
	assert.Contains(t, hosts, "app.example.test", "the application's route is still in the reconciler's expected set")
}

// Removing a process type deletes its row BEFORE its container stops, so the stop
// event arrives with no row to resolve. That is the exact UI scenario that took the
// domains down; a labelled container with no row must leave the application alone.
func TestAStopForARemovedInstanceLeavesTheApplicationAlone(t *testing.T) {
	f := seed(t)
	f.runningApp(t)
	worker := f.addWorker(t, status.ApplicationRunning)
	require.NoError(t, f.q.DeleteProcessType(f.ctx, worker.ProcessTypeID)) // cascades the instance

	f.w.handleEvent(f.ctx, f.workerEvent("stop", nil))

	assert.Equal(t, status.ApplicationRunning, f.appStatus(t))
	assert.Equal(t, status.ApplicationRunning, f.instStatus(t))
}

// The deliberate not-found decision: a container with NO process-type label is the
// pre-upgrade single container, which keeps driving the application as before.
func TestAnUnlabelledContainerWithNoRowStillDrivesTheApplication(t *testing.T) {
	f := seed(t)
	f.runningApp(t)
	require.NoError(t, f.q.DeleteProcessType(f.ctx, f.inst.ProcessTypeID))

	f.w.handleEvent(f.ctx, runtime.ContainerEvent{
		ContainerName: f.app.Slug, Status: "stop",
		Labels: map[string]string{labelApplicationID: pgUUIDToString(f.app.ID)},
	})
	assert.Equal(t, status.ApplicationStopped, f.appStatus(t))
}

// The invisibility check: an application with only the backfilled web type, which IS
// the routable type, behaves exactly as it did.
func TestAWebOnlyApplicationIsDrivenByItsRoutableContainerAsBefore(t *testing.T) {
	f := seed(t)
	web := func(ev string, extra map[string]string) runtime.ContainerEvent {
		labels := map[string]string{
			labelApplicationID: pgUUIDToString(f.app.ID), runtime.LabelProcessType: "web", runtime.LabelInstanceIdx: "1",
		}
		for k, v := range extra {
			labels[k] = v
		}
		return runtime.ContainerEvent{ContainerName: f.app.Slug, Status: ev, Labels: labels}
	}

	f.w.handleEvent(f.ctx, web("start", nil))
	assert.Equal(t, status.ApplicationRunning, f.appStatus(t))
	assert.Equal(t, status.ApplicationRunning, f.instStatus(t))

	f.w.handleEvent(f.ctx, web("die", map[string]string{"exitCode": "1"}))
	assert.Equal(t, status.ApplicationError, f.appStatus(t))
	assert.Equal(t, status.ApplicationError, f.instStatus(t))

	f.w.handleEvent(f.ctx, web("stop", nil))
	assert.Equal(t, status.ApplicationStopped, f.appStatus(t))
	assert.Equal(t, status.ApplicationStopped, f.instStatus(t))
}

// The filters that protect the application protect a worker's instance too, against
// its own recorded status: a die its own stop explains is not an error, and a stale
// health verdict does not resurrect a stopped worker.
func TestWorkerInstanceHonoursTheDieAndHealthFilters(t *testing.T) {
	f := seed(t)
	f.runningApp(t)
	worker := f.addWorker(t, status.ApplicationStopped)

	f.w.handleEvent(f.ctx, f.workerEvent("die", map[string]string{"exitCode": "1"}))
	assert.Equal(t, status.ApplicationStopped, f.statusOf(t, worker), "an error must not overwrite a recorded stop")

	f.w.handleEvent(f.ctx, f.workerEvent("health_status: healthy", nil))
	assert.Equal(t, status.ApplicationStopped, f.statusOf(t, worker), "a stale health event does not resurrect a stopped worker")

	// A real crash of a running worker is recorded, and still not on the application.
	_, err := f.q.UpdateInstanceStatus(f.ctx, generated.UpdateInstanceStatusParams{ID: worker.ID, Status: status.ApplicationRunning})
	require.NoError(t, err)
	f.w.handleEvent(f.ctx, f.workerEvent("die", map[string]string{"exitCode": "1"}))
	assert.Equal(t, status.ApplicationError, f.statusOf(t, worker))
	assert.Equal(t, status.ApplicationRunning, f.appStatus(t), "a crashing worker does not take the application down")
}

// Half 2. The application is stopped when its ROUTABLE container is gone, and a
// worker that is still running is not falsely marked stopped.
func TestReconcileUsesTheRoutableContainerAndDoesNotFalselyStopAWorker(t *testing.T) {
	f := seed(t)
	f.runningApp(t)
	worker := f.addWorker(t, status.ApplicationRunning)

	rt := &testutil.MockContainerRuntime{ListContainers_: []runtime.ContainerInfo{{
		Name: f.app.Slug + "-worker-1", Status: "running",
		Labels: map[string]string{labelApplicationID: pgUUIDToString(f.app.ID), runtime.LabelProcessType: "worker"},
	}}}
	New(rt, f.q, nil).reconcile(f.ctx)

	assert.Equal(t, status.ApplicationStopped, f.appStatus(t), "the routable container is gone, so the application is stopped")
	assert.Equal(t, status.ApplicationStopped, f.instStatus(t), "the routable instance's container is gone")
	assert.Equal(t, status.ApplicationRunning, f.statusOf(t, worker), "the worker's container is up: it must not be written as stopped")
}

// The reverse: a worker gone while the routable container is up leaves the application
// running (it used to depend on which container won the label map).
func TestReconcileLeavesTheApplicationRunningWhenOnlyAWorkerIsGone(t *testing.T) {
	f := seed(t)
	f.runningApp(t)
	f.addWorker(t, status.ApplicationRunning)

	rt := &testutil.MockContainerRuntime{ListContainers_: []runtime.ContainerInfo{{
		Name: f.app.Slug, Status: "running",
		Labels: map[string]string{labelApplicationID: pgUUIDToString(f.app.ID), runtime.LabelProcessType: "web"},
	}}}
	New(rt, f.q, nil).reconcile(f.ctx)

	assert.Equal(t, status.ApplicationRunning, f.appStatus(t))
}

// With a command health check the probe must ask the ROUTABLE container. The worker
// is listed last, so a map keyed by application id (last writer wins) would hand the
// probe the worker.
func TestReconcileProbesTheRoutableContainerForACommandHealthCheck(t *testing.T) {
	f := seed(t)
	f.runningApp(t)
	f.addWorker(t, status.ApplicationRunning)
	_, err := f.q.SetApplicationHealthCheck(f.ctx, generated.SetApplicationHealthCheckParams{
		ID: f.app.ID, HealthCheckType: "command", HealthCheckCommand: pgtype.Text{String: "true", Valid: true},
	})
	require.NoError(t, err)

	app := pgUUIDToString(f.app.ID)
	rt := &testutil.MockContainerRuntime{ListContainers_: []runtime.ContainerInfo{
		{Name: f.app.Slug, Status: "running", Labels: map[string]string{labelApplicationID: app, runtime.LabelProcessType: "web"}},
		{Name: f.app.Slug + "-worker-1", Status: "running", Labels: map[string]string{labelApplicationID: app, runtime.LabelProcessType: "worker"}},
	}}
	New(rt, f.q, nil).reconcile(f.ctx)

	assert.Equal(t, []string{f.app.Slug}, rt.HealthCalls, "the application's health is the routable container's, never a worker's")
}
