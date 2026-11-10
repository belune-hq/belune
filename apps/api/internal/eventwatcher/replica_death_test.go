package eventwatcher

import (
	"context"
	"errors"
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

// ⛔ The 0.3.0 blocker on its second axis. 0.3.0 made only the ROUTABLE container own the
// application's status (a worker stopping used to stop the whole application). 0.3.1 added
// REPLICAS and did not extend that: any routable instance owned it, so web-1 of two dying
// marked the application stopped while web-2 kept serving, buildExpected skipped it, and
// the sweep deleted its routes.

func (f fixture) webName(idx int) string {
	if idx == 1 {
		return f.inst.ContainerName
	}
	return f.app.Slug + "-web-2"
}

// addReplica gives the fixture a second instance of the routable type.
func (f fixture) addReplica(t *testing.T, instStatus string) generated.ApplicationInstance {
	t.Helper()
	in, err := f.q.CreateInstance(f.ctx, generated.CreateInstanceParams{
		ApplicationID: f.app.ID, ProcessTypeID: f.inst.ProcessTypeID, Idx: 2,
		ContainerName: f.webName(2), Status: instStatus,
	})
	require.NoError(t, err)
	return in
}

func (f fixture) webEvent(idx int, ev string, extra map[string]string) runtime.ContainerEvent {
	labels := map[string]string{
		labelApplicationID: pgUUIDToString(f.app.ID), runtime.LabelProcessType: "web",
	}
	for k, v := range extra {
		labels[k] = v
	}
	return runtime.ContainerEvent{ContainerName: f.webName(idx), Status: ev, Labels: labels}
}

// listing builds what Docker reports: web-N states by idx ("" = absent) and, when
// workerState is set, the worker.
func (f fixture) containers(web1, web2, worker string) *testutil.MockContainerRuntime {
	var list []runtime.ContainerInfo
	add := func(name, state, pt string) {
		if state == "" {
			return
		}
		list = append(list, runtime.ContainerInfo{
			Name: name, Status: state,
			Labels: map[string]string{labelApplicationID: pgUUIDToString(f.app.ID), runtime.LabelProcessType: pt},
		})
	}
	add(f.webName(1), web1, "web")
	add(f.webName(2), web2, "web")
	add(f.app.Slug+"-worker-1", worker, "worker")
	return &testutil.MockContainerRuntime{ListContainers_: list}
}

// unreachableDocker fails the listing the way an unreachable daemon does. The shared mock's
// ListContainers ignores its error field (only ListAllContainers honours it).
type unreachableDocker struct{ testutil.MockContainerRuntime }

func (*unreachableDocker) ListContainers(context.Context) ([]runtime.ContainerInfo, error) {
	return nil, errors.New("docker daemon unreachable")
}

func (f fixture) withDomain(t *testing.T) {
	t.Helper()
	_, err := f.q.CreateDomain(f.ctx, generated.CreateDomainParams{
		ApplicationID: f.app.ID, Hostname: "app.example.test", Path: "/", SslMode: "off",
		ContainerPort: pgtype.Int4{Int32: 8080, Valid: true},
	})
	require.NoError(t, err)
}

func (f fixture) routedHosts(t *testing.T) []string {
	t.Helper()
	pm := &testutil.MockProxyManager{}
	require.NoError(t, proxy.NewReconciler(f.q, pm, nil, time.Second).ReconcileNow(f.ctx))
	var hosts []string
	for _, r := range pm.AddedRoutes {
		hosts = append(hosts, r.Hostname)
	}
	return hosts
}

// One replica of two dies: the application stays running AND keeps its route, and the
// dead replica's own row records it. The instance half is what makes it correct rather
// than merely quiet.
func TestOneReplicaDyingKeepsTheApplicationRunningAndRoutedAndRecordsItOnTheInstance(t *testing.T) {
	cases := []struct {
		name      string
		event     string
		extra     map[string]string
		wantInst  string
		wantAfter func(string) bool
	}{
		{"clean stop", "stop", nil, status.ApplicationStopped, nil},
		{"crash", "die", map[string]string{"exitCode": "1"}, status.ApplicationError, nil},
		{"oom", "oom", nil, status.ApplicationError, nil},
		// buildExpected serves only "running", so an UNHEALTHY application loses its routes too.
		{"health check failing", "health_status: unhealthy", nil, status.ApplicationUnhealthy, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := seed(t)
			f.runningApp(t)
			web2 := f.addReplica(t, status.ApplicationRunning)
			f.withDomain(t)
			// Docker's view: web-1 is gone, web-2 is serving.
			f.w = New(f.containers("exited", "running", ""), f.q, nil)

			f.w.handleEvent(f.ctx, f.webEvent(1, c.event, c.extra))

			assert.Equal(t, status.ApplicationRunning, f.appStatus(t), "web-2 is serving: the application is up")
			assert.Equal(t, c.wantInst, f.instStatus(t), "the dead replica's own row records it")
			assert.Equal(t, status.ApplicationRunning, f.statusOf(t, web2), "and its sibling is untouched")
			assert.Contains(t, f.routedHosts(t), "app.example.test", "the route is still in the reconciler's expected set")
		})
	}
}

// The fix must not make an application immortal: when the LAST routable replica goes,
// the application is stopped and its routes drop.
func TestTheLastReplicaDyingStopsTheApplication(t *testing.T) {
	f := seed(t)
	f.runningApp(t)
	f.addReplica(t, status.ApplicationRunning)
	f.withDomain(t)

	f.w = New(f.containers("exited", "running", ""), f.q, nil)
	f.w.handleEvent(f.ctx, f.webEvent(1, "stop", nil))
	require.Equal(t, status.ApplicationRunning, f.appStatus(t))

	f.w = New(f.containers("exited", "exited", ""), f.q, nil)
	f.w.handleEvent(f.ctx, f.webEvent(2, "stop", nil))

	assert.Equal(t, status.ApplicationStopped, f.appStatus(t), "nothing routable is up")
	assert.NotContains(t, f.routedHosts(t), "app.example.test", "and its route is dropped")
}

// Guard the 0.3.0 fix, with replicas in play: a worker dying never touches the application.
func TestAWorkerDyingNextToTwoReplicasLeavesTheApplicationAlone(t *testing.T) {
	f := seed(t)
	f.runningApp(t)
	f.addReplica(t, status.ApplicationRunning)
	worker := f.addWorker(t, status.ApplicationRunning)
	f.w = New(f.containers("running", "running", "exited"), f.q, nil)

	f.w.handleEvent(f.ctx, f.workerEvent("die", map[string]string{"exitCode": "1"}))

	assert.Equal(t, status.ApplicationRunning, f.appStatus(t))
	assert.Equal(t, status.ApplicationError, f.statusOf(t, worker))
	assert.Equal(t, status.ApplicationRunning, f.instStatus(t))
}

// A replica that is cycling (restarting, or created-not-yet-started) is not "down": while
// one replica dies, another mid-restart must keep the application running.
func TestAReplicaMidRestartCountsAsMaybeUpAndLeavesTheApplicationAlone(t *testing.T) {
	for _, state := range []string{"restarting", "created", "removing"} {
		t.Run(state, func(t *testing.T) {
			f := seed(t)
			f.runningApp(t)
			f.addReplica(t, status.ApplicationRunning)
			f.w = New(f.containers("exited", state, ""), f.q, nil)

			f.w.handleEvent(f.ctx, f.webEvent(1, "die", map[string]string{"exitCode": "1"}))

			assert.Equal(t, status.ApplicationRunning, f.appStatus(t))
			assert.Equal(t, status.ApplicationError, f.instStatus(t))
		})
	}
}

// When Docker cannot be asked, the application is left alone: a status that lags is
// corrected by the next reconcile, a wrongly stopped application loses its routes.
func TestWhenTheOtherReplicasCannotBeCheckedTheApplicationIsLeftAlone(t *testing.T) {
	f := seed(t)
	f.runningApp(t)
	f.addReplica(t, status.ApplicationRunning)
	f.w = New(&unreachableDocker{}, f.q, nil)

	f.w.handleEvent(f.ctx, f.webEvent(1, "stop", nil))

	assert.Equal(t, status.ApplicationRunning, f.appStatus(t))
	assert.Equal(t, status.ApplicationStopped, f.instStatus(t), "the instance is still recorded")
}

// A replica coming up is the application coming up, however many are down.
func TestAReplicaStartingMarksTheApplicationRunning(t *testing.T) {
	f := seed(t)
	f.setAppStatus(t, status.ApplicationStopped)
	f.addReplica(t, status.ApplicationStopped)
	f.w = New(f.containers("exited", "running", ""), f.q, nil)

	f.w.handleEvent(f.ctx, f.webEvent(2, "start", nil))

	assert.Equal(t, status.ApplicationRunning, f.appStatus(t))
}

// ── The reconcile path ─────────────────────────────────────────────────────

// The stale-row case: web-1's ROW says running but its container is gone, and web-2 is
// up. ResolveOne's pick (lowest idx whose row says running) is web-1, so the application
// read as down. The application is up when ANY routable replica is.
func TestReconcile_AStaleRowForOneReplicaDoesNotStopAnApplicationServedByAnother(t *testing.T) {
	f := seed(t)
	f.runningApp(t) // web-1's row: running
	web2 := f.addReplica(t, status.ApplicationRunning)
	f.withDomain(t)

	New(f.containers("", "running", ""), f.q, nil).reconcile(f.ctx)

	assert.Equal(t, status.ApplicationRunning, f.appStatus(t))
	assert.Equal(t, status.ApplicationStopped, f.instStatus(t), "web-1's stale row is corrected")
	assert.Equal(t, status.ApplicationRunning, f.statusOf(t, web2))
	assert.Contains(t, f.routedHosts(t), "app.example.test")
}

func TestReconcile_NoRoutableReplicaUpStopsTheApplication(t *testing.T) {
	f := seed(t)
	f.runningApp(t)
	f.addReplica(t, status.ApplicationRunning)

	New(f.containers("", "exited", ""), f.q, nil).reconcile(f.ctx)

	assert.Equal(t, status.ApplicationStopped, f.appStatus(t))
}

// Mid-restart is not a verdict: with no replica up but one cycling, reconcile leaves
// the stored status alone, exactly as it does for a single container.
func TestReconcile_AReplicaMidRestartLeavesTheApplicationAlone(t *testing.T) {
	f := seed(t)
	f.runningApp(t)
	f.addReplica(t, status.ApplicationRunning)

	New(f.containers("", "restarting", ""), f.q, nil).reconcile(f.ctx)

	assert.Equal(t, status.ApplicationRunning, f.appStatus(t))
}

// And the other direction: a stopped application whose only live container is the SECOND
// replica is corrected to running.
func TestReconcile_ARunningSecondReplicaMarksAStoppedApplicationRunning(t *testing.T) {
	f := seed(t)
	f.setAppStatus(t, status.ApplicationStopped)
	f.addReplica(t, status.ApplicationStopped)

	New(f.containers("", "running", ""), f.q, nil).reconcile(f.ctx)

	assert.Equal(t, status.ApplicationRunning, f.appStatus(t))
}

// routableContainerState answers "is the application serving", and must not depend on the
// instance rows being right. End to end, reconcile's syncInstancePresence runs first and
// corrects a stale row, which MASKS the old ResolveOne-based answer; this asks the function
// directly, with web-1's row still saying running while its container is gone and web-2 up.
// Under ResolveOne the pick is web-1, so the application read as down.
func TestRoutableContainerState_AnyReplicaUpMeansTheApplicationIsServing(t *testing.T) {
	f := seed(t)
	f.runningApp(t) // web-1's row: running
	f.addReplica(t, status.ApplicationRunning)
	idx := buildContainerIndex(f.containers("", "running", "").ListContainers_)

	name, running, transient := routableContainerState(f.ctx, f.q, f.app.ID, pgUUIDToString(f.app.ID), idx)

	assert.True(t, running, "web-2 is serving")
	assert.False(t, transient)
	assert.Equal(t, f.webName(2), name, "the probe target is the replica that is actually up")

	none := buildContainerIndex(f.containers("", "exited", "").ListContainers_)
	_, running, transient = routableContainerState(f.ctx, f.q, f.app.ID, pgUUIDToString(f.app.ID), none)
	assert.False(t, running)
	assert.False(t, transient)

	cycling := buildContainerIndex(f.containers("", "restarting", "").ListContainers_)
	_, running, transient = routableContainerState(f.ctx, f.q, f.app.ID, pgUUIDToString(f.app.ID), cycling)
	assert.False(t, running)
	assert.True(t, transient, "a replica mid-restart is a passing phase, not a verdict")
}
