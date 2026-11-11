package service_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/belune-hq/belune/internal/runtime"
	"github.com/belune-hq/belune/internal/service"
	"github.com/belune-hq/belune/internal/status"
	"github.com/belune-hq/belune/internal/store/generated"
	"github.com/belune-hq/belune/internal/testutil"
)

// ⛔ A DELETE is bounded, not detached-and-202'd: the container is going away regardless, so there
// is no reason to wait out Docker's 30s grace for a process that ignores SIGTERM. A stop that never
// finishes (blocks until its own bounded deadline) must still be followed by a FORCED removal of
// EVERY container, on a live context, and the row must go. The old code stopped on the request
// context, the request died mid-stop, and the removal and the row delete did nothing: the app was
// left half-deleted (container Exited (137) but not removed, row still there).

// neverFinishes is a stop whose process ignores SIGTERM: it only returns when its own deadline does.
func neverFinishes(ctx context.Context, _ string) error {
	<-ctx.Done()
	return ctx.Err()
}

func (f actionFixture) rowGone(t *testing.T) bool {
	t.Helper()
	_, err := testQueries.GetApplication(context.Background(), f.app)
	return err != nil
}

func (f actionFixture) deleteIt(t *testing.T, ctx context.Context) error {
	t.Helper()
	row, err := testQueries.GetApplicationWithProjectSlug(context.Background(), f.app)
	require.NoError(t, err)
	return f.svc.Delete(ctx, f.app, row.ProjectSlug, row.Slug)
}

func TestDeleteApplication_RemovesEveryContainerWhenNoStopEverFinishes(t *testing.T) {
	f := newActionFixture(t)
	f.rt.StopFunc = neverFinishes

	start := time.Now()
	err := f.deleteIt(t, context.Background())
	elapsed := time.Since(start)

	require.NoError(t, err)
	assert.Contains(t, f.rt.RemoveCalls, f.web, "the web container is removed")
	assert.Contains(t, f.rt.RemoveCalls, f.work, "and so is the worker: a first slow stop no longer skips the rest")
	for _, e := range f.rt.RemoveCtxErrs {
		assert.NoError(t, e, "every removal ran on a live context")
	}
	assert.True(t, f.rowGone(t), "the row is deleted: one call, gone")
	// Five names are swept (three legacy formats plus two instances). One after another that is
	// 5 x 5s of stop grace; together it is one.
	assert.Less(t, elapsed, 9*time.Second, "the containers are stopped together, not one after another")
}

func TestDeleteApplication_SurvivesTheRequestBeingCancelledMidStop(t *testing.T) {
	f := newActionFixture(t)
	reqCtx, cancelReq := context.WithCancel(context.Background())
	defer cancelReq()
	f.rt.StopFunc = func(ctx context.Context, _ string) error {
		cancelReq() // the handler timeout fires, or the client goes away
		time.Sleep(50 * time.Millisecond)
		return ctx.Err() // the stop's own context must not have died with the request
	}

	err := f.deleteIt(t, reqCtx)

	require.NoError(t, err)
	assert.Contains(t, f.rt.RemoveCalls, f.web)
	assert.Contains(t, f.rt.RemoveCalls, f.work)
	for _, e := range f.rt.RemoveCtxErrs {
		assert.NoError(t, e)
	}
	assert.True(t, f.rowGone(t), "the row delete is detached too: it used to run on the dead context and fail")
}

// A container that cannot be removed is logged and the delete carries on, as it always did: the row
// goes and the orphan reaper is the backstop. It must not cost its sibling its removal.
func TestDeleteApplication_AFailedRemovalDoesNotSkipItsSibling(t *testing.T) {
	f := newActionFixture(t)
	f.rt.RemoveFunc = func(_ context.Context, name string) error {
		if name == f.work {
			return errors.New("docker: remove failed")
		}
		return nil
	}

	require.NoError(t, f.deleteIt(t, context.Background()))

	assert.Contains(t, f.rt.RemoveCalls, f.web)
	assert.Contains(t, f.rt.RemoveCalls, f.work)
	assert.True(t, f.rowGone(t))
}

// DeleteProcessType used to loop serially (13s per container), so a type with several replicas
// outran the 15s window. It shares the helper now.
func TestDeleteProcessType_RemovesReplicasConcurrently(t *testing.T) {
	f := newActionFixture(t)
	types, err := testQueries.ListProcessTypesByApplication(context.Background(), f.app)
	require.NoError(t, err)
	var workerType generated.ApplicationProcessType
	for _, pt := range types {
		if !pt.Routable {
			workerType = pt
		}
	}
	require.NotEmpty(t, workerType.Name)
	// Two more rows for the worker type, as scaling would add.
	for _, idx := range []int32{2, 3} {
		_, err := testQueries.CreateInstance(context.Background(), generated.CreateInstanceParams{
			ApplicationID: f.app, ProcessTypeID: workerType.ID, Idx: idx,
			ContainerName: f.work + "-r" + string(rune('0'+idx)), Status: "running",
		})
		require.NoError(t, err)
	}
	const each = 400 * time.Millisecond
	f.rt.StopFunc = func(context.Context, string) error { time.Sleep(each); return nil }

	start := time.Now()
	warning, err := f.svc.DeleteProcessType(context.Background(), f.app, workerType.ID)
	elapsed := time.Since(start)

	require.NoError(t, err)
	assert.Empty(t, warning)
	assert.Len(t, f.rt.RemoveCalls, 3, "all three replicas are removed")
	assert.Less(t, elapsed, 3*each-100*time.Millisecond, "three replicas cost about one stop, not three")
}

// ── databases ──────────────────────────────────────────────────────────────

type dbFixture struct {
	svc *service.DatabaseService
	rt  *testutil.MockContainerRuntime
	db  generated.Database
}

func newDBFixture(t *testing.T) dbFixture {
	t.Helper()
	t.Cleanup(func() { truncate(t) })
	_, project := seedUserAndProject(t)
	rt := &testutil.MockContainerRuntime{}
	svc := service.NewDatabaseService(testPool, testQueries, runtime.NewLocalRuntimes(rt), nil, nil)
	db, err := testQueries.CreateDatabase(context.Background(), generated.CreateDatabaseParams{
		ProjectID: project.ID, Type: "other", Name: "d", Slug: "proj-db-12345678", Version: "1",
		Status: status.DatabaseRunning, BackupMode: "none",
	})
	require.NoError(t, err)
	return dbFixture{svc: svc, rt: rt, db: db}
}

func (f dbFixture) dbStatus(t *testing.T) string {
	t.Helper()
	got, err := testQueries.GetDatabase(context.Background(), f.db.ID)
	require.NoError(t, err)
	return got.Status
}

// Nothing guarantees a database image traps SIGTERM (the "Other" engine takes a user-supplied one).
func TestDeleteDatabase_RemovesTheContainerAndTheRowWhenTheStopNeverFinishes(t *testing.T) {
	f := newDBFixture(t)
	f.rt.StopFunc = neverFinishes

	require.NoError(t, f.svc.Delete(context.Background(), f.db.ID, false))

	assert.Equal(t, []string{f.db.Slug}, f.rt.RemoveCalls)
	require.Len(t, f.rt.RemoveCtxErrs, 1)
	assert.NoError(t, f.rt.RemoveCtxErrs[0], "the removal ran on a live context")
	_, err := testQueries.GetDatabase(context.Background(), f.db.ID)
	assert.Error(t, err, "the row is gone")
}

func TestDeleteDatabase_SurvivesTheRequestBeingCancelledMidStop(t *testing.T) {
	f := newDBFixture(t)
	reqCtx, cancelReq := context.WithCancel(context.Background())
	defer cancelReq()
	f.rt.StopFunc = func(ctx context.Context, _ string) error {
		cancelReq()
		time.Sleep(50 * time.Millisecond)
		return ctx.Err()
	}

	require.NoError(t, f.svc.Delete(reqCtx, f.db.ID, false))

	assert.Equal(t, []string{f.db.Slug}, f.rt.RemoveCalls)
	_, err := testQueries.GetDatabase(context.Background(), f.db.ID)
	assert.Error(t, err)
}

func TestRestartDatabase_StillStartsWhenTheRequestDiesMidStop(t *testing.T) {
	f := newDBFixture(t)
	reqCtx, cancelReq := context.WithCancel(context.Background())
	defer cancelReq()
	f.rt.StopFunc = func(ctx context.Context, _ string) error {
		cancelReq()
		time.Sleep(50 * time.Millisecond)
		return ctx.Err()
	}

	db, err := f.svc.Restart(reqCtx, f.db.ID)

	require.NoError(t, err)
	assert.Equal(t, status.DatabaseRunning, db.Status)
	assert.Equal(t, []string{f.db.Slug}, f.rt.StartCalls)
	require.NotEmpty(t, f.rt.StartCtxErrs)
	assert.NoError(t, f.rt.StartCtxErrs[0], "the start ran on a context the request's timeout cannot cancel")
}

func TestRestartDatabase_StartsAfterAFailedOrTimedOutStop(t *testing.T) {
	for name, stop := range map[string]func(context.Context, string) error{
		"deadline exceeded": func(context.Context, string) error { return context.DeadlineExceeded },
		"daemon error":      func(context.Context, string) error { return errors.New("docker: stop failed") },
	} {
		t.Run(name, func(t *testing.T) {
			f := newDBFixture(t)
			f.rt.StopFunc = stop

			db, err := f.svc.Restart(context.Background(), f.db.ID)

			require.NoError(t, err)
			assert.Equal(t, status.DatabaseRunning, db.Status)
			assert.Equal(t, []string{f.db.Slug}, f.rt.StartCalls, "the start follows a stop that failed")
		})
	}
}

func TestRestartDatabase_ReportsBothFailuresWhenTheStartFailsToo(t *testing.T) {
	f := newDBFixture(t)
	f.rt.StopFunc = func(context.Context, string) error { return errors.New("stop boom") }
	f.rt.StartErr = errors.New("start boom")

	_, err := f.svc.Restart(context.Background(), f.db.ID)

	require.ErrorIs(t, err, service.ErrContainerAction)
	assert.Contains(t, err.Error(), "stop boom")
	assert.Contains(t, err.Error(), "start boom")
}

func TestStopAndStartDatabase_CompleteAfterTheRequestIsCancelled(t *testing.T) {
	f := newDBFixture(t)
	reqCtx, cancelReq := context.WithCancel(context.Background())
	cancelReq() // the client is already gone

	stopped, err := f.svc.Stop(reqCtx, f.db.ID)
	require.NoError(t, err)
	assert.Equal(t, status.DatabaseStopped, stopped.Status)
	assert.Equal(t, status.DatabaseStopped, f.dbStatus(t), "the status write is detached too")

	started, err := f.svc.Start(reqCtx, f.db.ID)
	require.NoError(t, err)
	assert.Equal(t, status.DatabaseRunning, started.Status)
	assert.Equal(t, []string{f.db.Slug}, f.rt.StartCalls)
}

func TestDatabaseActions_ReportAMissingDatabase(t *testing.T) {
	f := newDBFixture(t)
	var missing pgtype.UUID
	require.NoError(t, missing.Scan("00000000-0000-0000-0000-000000000001"))

	_, err := f.svc.Restart(context.Background(), missing)

	assert.ErrorIs(t, err, service.ErrDatabaseNotFound)
	assert.Empty(t, f.rt.StopCalls)
}
