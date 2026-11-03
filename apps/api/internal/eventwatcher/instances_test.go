package eventwatcher

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/belune-hq/belune/internal/runtime"
	"github.com/belune-hq/belune/internal/status"
	"github.com/belune-hq/belune/internal/store/generated"
	"github.com/belune-hq/belune/internal/testutil"
)

type fixture struct {
	ctx  context.Context
	w    *Watcher
	q    *generated.Queries
	app  generated.Application
	inst generated.ApplicationInstance
}

// seed builds an application with the one instance A1's backfill (or Create)
// gives it: named by the legacy slug, status pending, no container_id.
func seed(t *testing.T) fixture {
	t.Helper()
	ctx := context.Background()
	pool, q, teardown := testutil.SetupTestDB()
	t.Cleanup(teardown)

	user, err := q.CreateUser(ctx, generated.CreateUserParams{Email: "ew@test.com", PasswordHash: "x", Role: "admin"})
	require.NoError(t, err)
	proj, err := q.CreateProject(ctx, generated.CreateProjectParams{
		Name: "p", Slug: "p", UserID: user.ID, ServerID: testutil.LocalServerID(t, ctx, q),
	})
	require.NoError(t, err)
	app, err := q.CreateApplication(ctx, generated.CreateApplicationParams{
		ProjectID: proj.ID, Name: "a", Slug: "p-a-12345678", Type: "image", BuildType: "image",
		SourceImage: pgtype.Text{String: "nginx", Valid: true},
	})
	require.NoError(t, err)
	pt, err := q.CreateProcessType(ctx, generated.CreateProcessTypeParams{ApplicationID: app.ID, Name: "web", Replicas: 1, Routable: true})
	require.NoError(t, err)
	inst, err := q.CreateInstance(ctx, generated.CreateInstanceParams{
		ApplicationID: app.ID, ProcessTypeID: pt.ID, Idx: 1, ContainerName: app.Slug, Status: "pending",
	})
	require.NoError(t, err)
	_ = pool
	return fixture{ctx: ctx, w: New(nil, q, nil), q: q, app: app, inst: inst}
}

func (f fixture) setAppStatus(t *testing.T, s string) {
	t.Helper()
	_, err := f.q.UpdateApplicationStatus(f.ctx, generated.UpdateApplicationStatusParams{ID: f.app.ID, Status: s})
	require.NoError(t, err)
}

func (f fixture) instStatus(t *testing.T) string {
	t.Helper()
	got, err := f.q.GetInstance(f.ctx, f.inst.ID)
	require.NoError(t, err)
	return got.Status
}

func (f fixture) appStatus(t *testing.T) string {
	t.Helper()
	got, err := f.q.GetApplication(f.ctx, f.app.ID)
	require.NoError(t, err)
	return got.Status
}

// ⚠️ THE UPGRADE CASE, and the majority case on a real install: a container
// created before the process-type labels existed carries application-id ONLY, and
// stays that way until its application is redeployed. A label-only resolver passes
// every other test and fails this one.
func TestInstanceStatus_ALegacyContainerWithOnlyAnApplicationIDLabelDrivesItsInstance(t *testing.T) {
	f := seed(t)
	legacy := func(ev string) runtime.ContainerEvent {
		return runtime.ContainerEvent{
			ContainerID: "legacy-id-never-recorded", ContainerName: f.app.Slug, Status: ev,
			Labels: map[string]string{labelApplicationID: pgUUIDToString(f.app.ID)},
		}
	}

	f.w.handleEvent(f.ctx, legacy("start"))
	assert.Equal(t, status.ApplicationRunning, f.instStatus(t))
	assert.Equal(t, status.ApplicationRunning, f.appStatus(t), "application status is still driven exactly as before")

	f.w.handleEvent(f.ctx, legacy("stop"))
	assert.Equal(t, status.ApplicationStopped, f.instStatus(t))
	assert.Equal(t, status.ApplicationStopped, f.appStatus(t))
}

func TestInstanceStatus_ResolvesByContainerIDThenLabels(t *testing.T) {
	f := seed(t)
	app := pgUUIDToString(f.app.ID)

	// By container_id: wrong name, no process-type labels.
	_, err := f.q.UpdateInstanceContainer(f.ctx, generated.UpdateInstanceContainerParams{
		ID: f.inst.ID, ContainerID: pgtype.Text{String: "recorded-id", Valid: true}, Status: "pending",
	})
	require.NoError(t, err)
	f.w.handleEvent(f.ctx, runtime.ContainerEvent{
		ContainerID: "recorded-id", ContainerName: "some-other-name", Status: "start",
		Labels: map[string]string{labelApplicationID: app},
	})
	assert.Equal(t, status.ApplicationRunning, f.instStatus(t), "container_id is the most precise key")

	// By labels alone: unknown id, unknown name.
	_, err = f.q.UpdateInstanceStatus(f.ctx, generated.UpdateInstanceStatusParams{ID: f.inst.ID, Status: "pending"})
	require.NoError(t, err)
	f.w.handleEvent(f.ctx, runtime.ContainerEvent{
		ContainerID: "unknown", ContainerName: "unknown", Status: "start",
		Labels: map[string]string{labelApplicationID: app, runtime.LabelProcessType: "web", runtime.LabelInstanceIdx: "1"},
	})
	assert.Equal(t, status.ApplicationRunning, f.instStatus(t), "the labels identify a new container")
}

// An event for a container no instance row accounts for must not move a different
// instance of the same application, or any instance of another.
func TestInstanceStatus_AnUnmatchedContainerChangesNoInstance(t *testing.T) {
	f := seed(t)
	f.w.handleEvent(f.ctx, runtime.ContainerEvent{
		ContainerID: "x", ContainerName: "not-recorded", Status: "start",
		Labels: map[string]string{labelApplicationID: pgUUIDToString(f.app.ID)},
	})
	assert.Equal(t, "pending", f.instStatus(t))
}

// The instance must not contradict a decision the application write made. A die
// for an application recorded as stopped is the stop that was asked for, whatever
// exit code came back (ApplyDieStatus), so the instance stays stopped too.
func TestInstanceStatus_ADieTheApplicationWriteSuppressesIsSuppressedForTheInstance(t *testing.T) {
	f := seed(t)
	f.setAppStatus(t, status.ApplicationStopped)
	_, err := f.q.UpdateInstanceStatus(f.ctx, generated.UpdateInstanceStatusParams{ID: f.inst.ID, Status: status.ApplicationStopped})
	require.NoError(t, err)

	f.w.handleEvent(f.ctx, runtime.ContainerEvent{
		ContainerName: f.app.Slug, Status: "die",
		Labels: map[string]string{labelApplicationID: pgUUIDToString(f.app.ID), "exitCode": "1"},
	})

	assert.Equal(t, status.ApplicationStopped, f.appStatus(t))
	assert.Equal(t, status.ApplicationStopped, f.instStatus(t), "an unconditional write on die would have said error")
}

// Reconciliation must not turn a passing phase into a stored verdict, for the
// instance any more than for the application.
func TestInstanceStatus_ATransientContainerLeavesASettledInstanceAlone(t *testing.T) {
	f := seed(t)
	f.setAppStatus(t, status.ApplicationRunning)
	_, err := f.q.UpdateInstanceStatus(f.ctx, generated.UpdateInstanceStatusParams{ID: f.inst.ID, Status: status.ApplicationRunning})
	require.NoError(t, err)

	rt := &testutil.MockContainerRuntime{ListContainers_: []runtime.ContainerInfo{{
		Name: f.app.Slug, Status: "restarting",
		Labels: map[string]string{labelApplicationID: pgUUIDToString(f.app.ID)},
	}}}
	New(rt, f.q, nil).reconcile(f.ctx)

	assert.Equal(t, status.ApplicationRunning, f.appStatus(t))
	assert.Equal(t, status.ApplicationRunning, f.instStatus(t), "restarting proves nothing either way")
}

// The reconcile sweep also corrects an instance whose container is gone, and
// names a running one by the container the index found (a legacy name).
func TestInstanceStatus_ReconcileMirrorsTheApplicationCorrection(t *testing.T) {
	f := seed(t)
	f.setAppStatus(t, status.ApplicationRunning)
	_, err := f.q.UpdateInstanceStatus(f.ctx, generated.UpdateInstanceStatusParams{ID: f.inst.ID, Status: status.ApplicationRunning})
	require.NoError(t, err)

	// Container gone: running app is corrected to stopped, and so is its instance.
	New(&testutil.MockContainerRuntime{}, f.q, nil).reconcile(f.ctx)
	assert.Equal(t, status.ApplicationStopped, f.appStatus(t))
	assert.Equal(t, status.ApplicationStopped, f.instStatus(t))

	// Container up under its legacy name with only application-id: running again.
	rt := &testutil.MockContainerRuntime{ListContainers_: []runtime.ContainerInfo{{
		Name: f.app.Slug, Status: "running",
		Labels: map[string]string{labelApplicationID: pgUUIDToString(f.app.ID)},
	}}}
	New(rt, f.q, nil).reconcile(f.ctx)
	assert.Equal(t, status.ApplicationRunning, f.appStatus(t))
	assert.Equal(t, status.ApplicationRunning, f.instStatus(t))
}
