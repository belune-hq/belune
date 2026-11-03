package worker_test

import (
	"context"
	"errors"
	"testing"

	"github.com/hibiken/asynq"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/belune-hq/belune/internal/naming"
	"github.com/belune-hq/belune/internal/runtime"
	"github.com/belune-hq/belune/internal/status"
	"github.com/belune-hq/belune/internal/store/generated"
	"github.com/belune-hq/belune/internal/testutil"
)

// seedWorkerType writes a non-routable process type and its instance straight into
// the database, with the name the instance row records. B3's CRUD does not exist
// yet, and a test that only ever sees the backfilled web type proves nothing about
// the loop.
func seedWorkerType(t *testing.T, app generated.Application, name, command string, healthType *string) string {
	t.Helper()
	ctx := context.Background()
	ht := pgtype.Text{}
	if healthType != nil {
		ht = pgtype.Text{String: *healthType, Valid: true}
	}
	pt, err := testQueries.CreateProcessType(ctx, generated.CreateProcessTypeParams{
		ApplicationID: app.ID, Name: name, Command: command, Replicas: 1, Routable: false,
		HealthCheckType: ht, HealthCheckCommand: pgtype.Text{String: "true", Valid: healthType != nil},
	})
	require.NoError(t, err)
	containerName := app.Slug + "-" + name + "-1"
	_, err = testQueries.CreateInstance(ctx, generated.CreateInstanceParams{
		ApplicationID: app.ID, ProcessTypeID: pt.ID, Idx: 1, ContainerName: containerName, Status: "pending",
	})
	require.NoError(t, err)
	return containerName
}

func distinctIDs(cfg runtime.ContainerConfig) (string, error) { return "id-" + cfg.Name, nil }

func deploy(t *testing.T, app generated.Application, dep generated.Deployment, rt *testutil.MockContainerRuntime, pm *testutil.MockProxyManager) error {
	t.Helper()
	h := newTestHandler(rt, pm)
	return h.HandleDeployTask(context.Background(), asynq.NewTask("deploy", mustMarshalPayload(t, app.ID, dep.ID)))
}

func seedDomain(t *testing.T, app generated.Application, host string) {
	t.Helper()
	_, err := testQueries.CreateDomain(context.Background(), generated.CreateDomainParams{
		ApplicationID: app.ID, Hostname: host, Path: "/", SslMode: "off",
		ContainerPort: pgtype.Int4{Int32: 8080, Valid: true},
	})
	require.NoError(t, err)
}

func countOf(haystack []string, needle string) int {
	n := 0
	for _, s := range haystack {
		if s == needle {
			n++
		}
	}
	return n
}

// The default state has one type, so the loop only earns its keep with several.
// One container per type, each with its own Cmd and labels; the routable one wired
// to the proxy and the workers not; every instance recorded; and workers first.
func TestHandleDeployTask_DeploysEveryProcessTypeFromTheOneImage(t *testing.T) {
	t.Cleanup(func() { _ = testutil.TruncateAll(context.Background(), testPool) })
	ctx := context.Background()
	none, command := "none", "command"
	app, dep := seedApp(t, func(a *generated.CreateApplicationParams, _ *generated.CreateDeploymentParams) {
		// An app-level http probe: a worker that inherited it would poll a port nothing
		// listens on for the full probe timeout.
		a.HealthCheckPath = pgtype.Text{String: "/up", Valid: true}
	})
	// Three workers: a NULL health type (inherits: not routable, so none), an explicit
	// none, and an explicit command check.
	mailer := seedWorkerType(t, app, "mailer", "run-mailer", nil)
	scheduler := seedWorkerType(t, app, "scheduler", "", &none)
	worker := seedWorkerType(t, app, "worker", "bundle exec sidekiq", &command)
	seedDomain(t, app, "web.example.test")
	// The routable type opts out of the http probe so the test does not wait on one.
	_, err := testPool.Exec(ctx, `UPDATE application_process_types SET health_check_type = 'none' WHERE application_id = $1 AND routable`, app.ID)
	require.NoError(t, err)

	rt := &testutil.MockContainerRuntime{CreateFunc: distinctIDs}
	pm := &testutil.MockProxyManager{}
	require.NoError(t, deploy(t, app, dep, rt, pm))

	require.Len(t, rt.CreateCalls, 4)
	var names []string
	for _, c := range rt.CreateCalls {
		names = append(names, c.Name)
	}
	assert.Equal(t, []string{mailer, scheduler, worker, app.Slug}, names, "workers by name, the routable type LAST, all from instance rows")

	byName := map[string]runtime.ContainerConfig{}
	for _, c := range rt.CreateCalls {
		byName[c.Name] = c
		assert.Equal(t, rt.CreateCalls[0].Image, c.Image, "one image for every type")
	}
	assert.Nil(t, byName[scheduler].Cmd, "an empty command leaves the image alone")
	assert.Nil(t, byName[scheduler].Entrypoint)
	assert.Equal(t, []string{"sh", "-c", "bundle exec sidekiq"}, byName[worker].Cmd)
	assert.NotNil(t, byName[worker].Entrypoint)
	assert.Nil(t, byName[app.Slug].Cmd)
	assert.Equal(t, "worker", byName[worker].Labels[runtime.LabelProcessType])
	assert.Equal(t, "web", byName[app.Slug].Labels[runtime.LabelProcessType])
	assert.Equal(t, "1", byName[worker].Labels[runtime.LabelInstanceIdx])

	// Only the routable type is wired to the proxy.
	require.Len(t, pm.AddedRoutes, 1)
	assert.Contains(t, pm.AddedRoutes[0].TargetURL, app.Slug+":")
	assert.NotContains(t, pm.AddedRoutes[0].TargetURL, "-worker-")

	// Only a type that asked for a command check is polled. None of the others, in
	// particular no worker that merely INHERITED a check, is gated by anything: an
	// http probe against a worker would wait on a port nothing listens on.
	assert.Equal(t, []string{worker}, rt.HealthCalls)

	// An instance row updated for each, in the one finalize.
	insts, err := testQueries.ListInstancesByApplication(ctx, app.ID)
	require.NoError(t, err)
	require.Len(t, insts, 4)
	for _, in := range insts {
		assert.Equal(t, "id-"+in.ContainerName, in.ContainerID.String, in.ContainerName)
		assert.Equal(t, status.ApplicationRunning, in.Status, in.ContainerName)
	}
	appRow, err := testQueries.GetApplication(ctx, app.ID)
	require.NoError(t, err)
	assert.Equal(t, status.ApplicationRunning, appRow.Status)

	// The application-scoped legacy names are swept ONCE, not once per type.
	appID := uuidString(app.ID)
	assert.Equal(t, 1, countOf(rt.RemoveCalls, naming.OldContainerName(appID)))
	assert.Equal(t, 1, countOf(rt.RemoveCalls, naming.IntermediateContainerName(projectSlugOf(t, app), appID)))
}

func projectSlugOf(t *testing.T, app generated.Application) string {
	t.Helper()
	row, err := testQueries.GetApplicationWithProjectSlug(context.Background(), app.ID)
	require.NoError(t, err)
	return row.ProjectSlug
}

// ⛔ The compensator trap: two containers are created, then a late stage fails. Both
// must be removed BY THEIR OWN ids. A rollback that read a shared field when it ran
// removes the last container twice and leaves the first running and unreferenced,
// which a call COUNT cannot tell from the correct result.
func TestHandleDeployTask_ALateFailureRemovesEveryContainerThisDeployCreated(t *testing.T) {
	t.Cleanup(func() { _ = testutil.TruncateAll(context.Background(), testPool) })
	app, dep := seedApp(t)
	worker := seedWorkerType(t, app, "worker", "run-worker", nil)
	seedDomain(t, app, "late.example.test")

	rt := &testutil.MockContainerRuntime{CreateFunc: distinctIDs}
	pm := &testutil.MockProxyManager{AddRouteErr: errors.New("caddy said no")}
	require.Error(t, deploy(t, app, dep, rt, pm), "wire_proxy fails after both containers exist")

	require.Len(t, rt.CreateCalls, 2, "the worker and the routable container were both created")
	webID, workerID := "id-"+app.Slug, "id-"+worker
	assert.Equal(t, 1, countOf(rt.RemoveCalls, workerID), "the worker's container must not be orphaned")
	assert.Equal(t, 1, countOf(rt.RemoveCalls, webID), "and the routable one removed exactly once, not twice")
	assert.Equal(t, 1, countOf(rt.StopCalls, workerID))
	assert.Equal(t, 1, countOf(rt.StopCalls, webID))

	final, err := testQueries.GetDeployment(context.Background(), dep.ID)
	require.NoError(t, err)
	assert.Equal(t, status.DeploymentFailed, final.Status)
}

// Why workers go first: they run the same image, so a worker that cannot start is a
// canary, and the routable container has not been touched, so the application is
// still serving the old version.
func TestHandleDeployTask_AFailingWorkerLeavesTheServingContainerUntouched(t *testing.T) {
	t.Cleanup(func() { _ = testutil.TruncateAll(context.Background(), testPool) })
	ctx := context.Background()
	app, dep := seedApp(t)
	worker := seedWorkerType(t, app, "worker", "run-worker", nil)
	_, err := testQueries.UpdateApplicationStatus(ctx, generated.UpdateApplicationStatusParams{ID: app.ID, Status: status.ApplicationRunning})
	require.NoError(t, err)

	rt := &testutil.MockContainerRuntime{CreateFunc: func(cfg runtime.ContainerConfig) (string, error) {
		if cfg.Name == worker {
			return "", errors.New("image cannot run this command")
		}
		return "id-" + cfg.Name, nil
	}}
	require.Error(t, deploy(t, app, dep, rt, &testutil.MockProxyManager{}))

	assert.NotContains(t, rt.StopCalls, app.Slug, "the serving container must not be stopped")
	assert.NotContains(t, rt.RemoveCalls, app.Slug, "or removed")
	for _, c := range rt.CreateCalls {
		assert.NotEqual(t, app.Slug, c.Name, "the routable container is not even created")
	}
	appRow, err := testQueries.GetApplication(ctx, app.ID)
	require.NoError(t, err)
	assert.Equal(t, status.ApplicationRunning, appRow.Status, "still serving: the application is not flagged errored")
}

// A process type with no instance row cannot be deployed to. It fails at load, before
// anything is torn down; no deploy ever invents an instance row.
func TestHandleDeployTask_ATypeWithNoInstanceFailsBeforeAnythingIsTornDown(t *testing.T) {
	t.Cleanup(func() { _ = testutil.TruncateAll(context.Background(), testPool) })
	app, dep := seedApp(t)
	_, err := testQueries.CreateProcessType(context.Background(), generated.CreateProcessTypeParams{
		ApplicationID: app.ID, Name: "orphan", Replicas: 1,
	})
	require.NoError(t, err)

	rt := &testutil.MockContainerRuntime{}
	require.Error(t, deploy(t, app, dep, rt, &testutil.MockProxyManager{}))
	assert.Empty(t, rt.StopCalls)
	assert.Empty(t, rt.RemoveCalls)
	assert.Empty(t, rt.CreateCalls)
}
