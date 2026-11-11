package service_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/belune-hq/belune/internal/runtime"
	"github.com/belune-hq/belune/internal/service"
	"github.com/belune-hq/belune/internal/status"
	"github.com/belune-hq/belune/internal/testutil"
)

// ⛔ The 15s-handler-timeout trap, on the application-level Stop/Start/Restart: a stop outlives
// the request whenever PID 1 ignores SIGTERM (Docker's grace is 30s), the request context dies
// mid-stop, and anything issued on it afterwards silently does nothing. A Restart used to leave
// the container Exited (137) for good. These cover EVERY container of the application, since
// the actions loop over them: web plus a worker, both ignoring SIGTERM, is an ordinary app now.

type actionFixture struct {
	svc  *service.ApplicationService
	rt   *testutil.MockContainerRuntime
	app  pgtype.UUID
	web  string
	work string
}

func newActionFixture(t *testing.T) actionFixture {
	t.Helper()
	t.Cleanup(func() { truncate(t) })
	_, project := seedUserAndProject(t)
	rt := &testutil.MockContainerRuntime{}
	svc := service.NewApplicationService(testPool, testQueries, runtime.NewLocalRuntimes(rt), testKeyring, t.TempDir(), nil)
	app, err := svc.Create(context.Background(), service.CreateApplicationParams{
		ProjectID: project.ID, ProjectSlug: project.Slug, Name: "A", BaseSlug: "a",
		Type: "image", SourceImage: "nginx:alpine", BuildType: "image",
	})
	require.NoError(t, err)
	_, workers, _, err := svc.CreateProcessType(context.Background(), app.ID, service.ProcessTypeInput{Name: "worker", Command: "a | b"})
	require.NoError(t, err)
	insts, err := testQueries.ListInstancesByApplication(context.Background(), app.ID)
	require.NoError(t, err)
	require.Len(t, insts, 2)
	f := actionFixture{svc: svc, rt: rt, app: app.ID, work: workers[0].ContainerName}
	for _, in := range insts {
		if in.ContainerName != f.work {
			f.web = in.ContainerName
		}
	}
	return f
}

func (f actionFixture) appStatus(t *testing.T) string {
	t.Helper()
	got, err := testQueries.GetApplication(context.Background(), f.app)
	require.NoError(t, err)
	return got.Status
}

func sorted(s []string) []string {
	out := append([]string(nil), s...)
	for i := range out {
		for j := i + 1; j < len(out); j++ {
			if out[j] < out[i] {
				out[i], out[j] = out[j], out[i]
			}
		}
	}
	return out
}

// The request's context dies DURING the stop of every container; the restart must still
// finish: every container stopped, every container started on a live context, status running.
func TestRestartApplication_StillStartsEveryContainerWhenTheRequestDiesMidStop(t *testing.T) {
	f := newActionFixture(t)
	reqCtx, cancelReq := context.WithCancel(context.Background())
	defer cancelReq()
	f.rt.StopFunc = func(ctx context.Context, _ string) error {
		cancelReq()                       // the handler timeout fires while containers are stopping
		time.Sleep(50 * time.Millisecond) // a slow stop
		return ctx.Err()                  // the stop's OWN context must not have died with the request
	}

	app, err := f.svc.Restart(reqCtx, f.app)

	require.NoError(t, err)
	assert.Equal(t, status.ApplicationRunning, app.Status)
	assert.Equal(t, sorted([]string{f.web, f.work}), sorted(f.rt.StopCalls), "every container is stopped")
	assert.Equal(t, sorted([]string{f.web, f.work}), sorted(f.rt.StartCalls), "and every one is started again")
	require.Len(t, f.rt.StartCtxErrs, 2)
	for _, e := range f.rt.StartCtxErrs {
		assert.NoError(t, e, "the start must run on a context the request's timeout cannot cancel")
	}
	assert.Equal(t, status.ApplicationRunning, f.appStatus(t), "and the status is recorded after the request is gone")
}

// The invariant is that the containers end up running: a stop that fails or times out must not
// skip the start. Starting a running container is a Docker no-op, so it is safe in every branch.
func TestRestartApplication_StartsAfterAFailedOrTimedOutStop(t *testing.T) {
	for name, stop := range map[string]func(context.Context, string) error{
		"deadline exceeded": func(context.Context, string) error { return context.DeadlineExceeded },
		"daemon error":      func(context.Context, string) error { return errors.New("docker: stop failed") },
	} {
		t.Run(name, func(t *testing.T) {
			f := newActionFixture(t)
			f.rt.StopFunc = stop

			app, err := f.svc.Restart(context.Background(), f.app)

			require.NoError(t, err, "the operator got what they asked for: the containers are running")
			assert.Equal(t, status.ApplicationRunning, app.Status)
			assert.Equal(t, sorted([]string{f.web, f.work}), sorted(f.rt.StartCalls), "the start follows a stop that failed")
		})
	}
}

// The stop's failure is reported only when the start fails too, and then both are named.
func TestRestartApplication_ReportsTheStopFailureOnlyWhenTheStartAlsoFails(t *testing.T) {
	f := newActionFixture(t)
	f.rt.StopFunc = func(context.Context, string) error { return errors.New("stop boom") }
	f.rt.StartErr = errors.New("start boom")

	_, err := f.svc.Restart(context.Background(), f.app)

	require.ErrorIs(t, err, service.ErrContainerAction)
	assert.Contains(t, err.Error(), "stop boom")
	assert.Contains(t, err.Error(), "start boom")
}

// Stop must complete, and record its status, even though the request is already cancelled: the
// status write used to be on the request context too.
func TestStopApplication_CompletesAfterTheRequestIsCancelled(t *testing.T) {
	f := newActionFixture(t)
	reqCtx, cancelReq := context.WithCancel(context.Background())
	cancelReq() // the client is already gone

	app, err := f.svc.Stop(reqCtx, f.app)

	require.NoError(t, err)
	assert.Equal(t, status.ApplicationStopped, app.Status)
	assert.Equal(t, sorted([]string{f.web, f.work}), sorted(f.rt.StopCalls), "every container is stopped")
	assert.Equal(t, status.ApplicationStopped, f.appStatus(t))
}

func TestStartApplication_CompletesAfterTheRequestIsCancelled(t *testing.T) {
	f := newActionFixture(t)
	reqCtx, cancelReq := context.WithCancel(context.Background())
	cancelReq()

	app, err := f.svc.Start(reqCtx, f.app)

	require.NoError(t, err)
	assert.Equal(t, status.ApplicationRunning, app.Status)
	assert.Equal(t, sorted([]string{f.web, f.work}), sorted(f.rt.StartCalls))
	for _, e := range f.rt.StartCtxErrs {
		assert.NoError(t, e)
	}
}

// N containers that ignore SIGTERM cost ONE grace, not N: the stops run concurrently. Run one
// after another, three containers are up to 90s against a 15s timeout.
func TestStopApplication_StopsTheContainersConcurrently(t *testing.T) {
	f := newActionFixture(t)
	const each = 300 * time.Millisecond
	var mu sync.Mutex
	inFlight, peak := 0, 0
	f.rt.StopFunc = func(context.Context, string) error {
		mu.Lock()
		inFlight++
		if inFlight > peak {
			peak = inFlight
		}
		mu.Unlock()
		time.Sleep(each)
		mu.Lock()
		inFlight--
		mu.Unlock()
		return nil
	}

	start := time.Now()
	_, err := f.svc.Stop(context.Background(), f.app)
	elapsed := time.Since(start)

	require.NoError(t, err)
	assert.Equal(t, 2, peak, "both containers were stopping at the same time")
	assert.Less(t, elapsed, 2*each-50*time.Millisecond, "two stops cost about one, not two")
}

// One container's stop failing must not cancel or skip its sibling, and the application is not
// recorded as stopped when a stop failed.
func TestStopApplication_AFailingStopDoesNotSkipItsSibling(t *testing.T) {
	f := newActionFixture(t)
	f.rt.StopFunc = func(_ context.Context, name string) error {
		if name == f.work {
			return errors.New("docker: stop failed")
		}
		return nil
	}

	_, err := f.svc.Stop(context.Background(), f.app)

	require.ErrorIs(t, err, service.ErrContainerAction)
	assert.Equal(t, sorted([]string{f.web, f.work}), sorted(f.rt.StopCalls), "the healthy container was still stopped")
	assert.NotEqual(t, status.ApplicationStopped, f.appStatus(t), "a failed stop is not recorded as a stop")
}
