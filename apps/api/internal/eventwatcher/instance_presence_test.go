package eventwatcher

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/belune-hq/belune/internal/runtime"
	"github.com/belune-hq/belune/internal/status"
	"github.com/belune-hq/belune/internal/testutil"
)

// containers builds a listing: the routable web container up, plus whichever worker
// state the test wants ("" = absent).
func (f fixture) listing(workerState string) *testutil.MockContainerRuntime {
	list := []runtime.ContainerInfo{{
		Name: f.app.Slug, Status: "running",
		Labels: map[string]string{labelApplicationID: pgUUIDToString(f.app.ID), runtime.LabelProcessType: "web"},
	}}
	if workerState != "" {
		list = append(list, runtime.ContainerInfo{
			Name: f.app.Slug + "-worker-1", Status: workerState,
			Labels: map[string]string{labelApplicationID: pgUUIDToString(f.app.ID), runtime.LabelProcessType: "worker"},
		})
	}
	return &testutil.MockContainerRuntime{ListContainers_: list}
}

// ⛔ The 0.3.0 blocker's regression, on the path that was missing: a worker that died
// while the control plane was down. The application is running and its status does
// not change, so nothing ever corrected the worker's row -- and the application must
// STAY running while it is corrected.
func TestReconcile_AWorkerGoneWhileTheAppRunsIsMarkedStoppedAndTheAppStaysRunning(t *testing.T) {
	f := seed(t)
	f.runningApp(t)
	worker := f.addWorker(t, status.ApplicationRunning)

	New(f.listing(""), f.q, nil).reconcile(f.ctx)

	assert.Equal(t, status.ApplicationStopped, f.statusOf(t, worker), "the worker's container is gone")
	assert.Equal(t, status.ApplicationRunning, f.appStatus(t), "a missing worker never stops the application")
	assert.Equal(t, status.ApplicationRunning, f.instStatus(t))
}

// The mirror: a row that says stopped (or error, or never-reconciled pending) while
// the container is up is corrected, as the application's own status is.
func TestReconcile_AnUpContainerCorrectsAStoppedErrorOrPendingRow(t *testing.T) {
	for _, from := range []string{status.ApplicationStopped, status.ApplicationError, "pending"} {
		t.Run(from, func(t *testing.T) {
			f := seed(t)
			f.runningApp(t)
			worker := f.addWorker(t, from)

			New(f.listing("running"), f.q, nil).reconcile(f.ctx)

			assert.Equal(t, status.ApplicationRunning, f.statusOf(t, worker))
		})
	}
}

// pending + absent is the documented state of a type added to a running application:
// no container until the next deploy. It must not become "stopped" (deliberate).
func TestReconcile_PendingOrStoppedOrErrorWithNoContainerIsLeftAlone(t *testing.T) {
	for _, from := range []string{"pending", status.ApplicationStopped, status.ApplicationError} {
		t.Run(from, func(t *testing.T) {
			f := seed(t)
			f.runningApp(t)
			worker := f.addWorker(t, from)

			New(f.listing(""), f.q, nil).reconcile(f.ctx)

			assert.Equal(t, from, f.statusOf(t, worker))
		})
	}
}

// A container that is restarting proves nothing: the row is neither stopped nor
// promoted from a sample taken mid-restart.
func TestReconcile_ATransientWorkerContainerLeavesItsRowAlone(t *testing.T) {
	for _, from := range []string{status.ApplicationRunning, status.ApplicationStopped} {
		f := seed(t)
		f.runningApp(t)
		worker := f.addWorker(t, from)

		New(f.listing("restarting"), f.q, nil).reconcile(f.ctx)

		assert.Equal(t, from, f.statusOf(t, worker), "row was %s", from)
	}
}

// Health is not presence: an unhealthy row with its container up is left for the
// event stream, which is where health arrives.
func TestReconcile_AnUnhealthyRowWithItsContainerUpIsLeftAlone(t *testing.T) {
	f := seed(t)
	f.runningApp(t)
	worker := f.addWorker(t, status.ApplicationUnhealthy)

	New(f.listing("running"), f.q, nil).reconcile(f.ctx)

	assert.Equal(t, status.ApplicationUnhealthy, f.statusOf(t, worker))
}

// The other branch had the same defect: with the application ALREADY stopped, the
// per-instance correction was gated behind the app's running->stopped transition and
// never ran, so a stale worker row survived.
func TestReconcile_AStaleWorkerRowIsCorrectedEvenWhenTheAppIsAlreadyStopped(t *testing.T) {
	f := seed(t)
	f.setAppStatus(t, status.ApplicationStopped)
	worker := f.addWorker(t, status.ApplicationRunning)

	New(&testutil.MockContainerRuntime{}, f.q, nil).reconcile(f.ctx)

	assert.Equal(t, status.ApplicationStopped, f.statusOf(t, worker))
	assert.Equal(t, status.ApplicationStopped, f.appStatus(t))
}
