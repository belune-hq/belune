package worker_test

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/hibiken/asynq"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/belune-hq/belune/internal/instances"
	"github.com/belune-hq/belune/internal/proxy"
	"github.com/belune-hq/belune/internal/runtime"
	"github.com/belune-hq/belune/internal/status"
	"github.com/belune-hq/belune/internal/store/generated"
	"github.com/belune-hq/belune/internal/testutil"
)

// timeline records the order of Docker and Caddy effects in ONE slice, because the
// whole point of a roll is the order: the new container exists before the route moves,
// and the old one outlives the route's last mention of it.
type timeline struct {
	mu sync.Mutex
	ev []string
}

func (tl *timeline) add(s string) {
	tl.mu.Lock()
	defer tl.mu.Unlock()
	tl.ev = append(tl.ev, s)
}

func (tl *timeline) events() []string {
	tl.mu.Lock()
	defer tl.mu.Unlock()
	return append([]string{}, tl.ev...)
}

// orderedProxy records each route write's upstreams (host:port stripped to the name).
type orderedProxy struct {
	*testutil.MockProxyManager
	tl *timeline
}

func (p orderedProxy) AddRoute(ctx context.Context, cfg proxy.RouteConfig) error {
	if err := p.MockProxyManager.AddRoute(ctx, cfg); err != nil {
		return err
	}
	var names []string
	for _, u := range cfg.Upstreams {
		names = append(names, strings.TrimSuffix(strings.TrimPrefix(u, "http://"), ":8080"))
	}
	p.tl.add("route:" + strings.Join(names, ","))
	return nil
}

func (p orderedProxy) RemoveRoute(ctx context.Context, host, path string) error {
	p.tl.add("route-removed")
	return p.MockProxyManager.RemoveRoute(ctx, host, path)
}

// rollRig is one application with a domain, serving as its legacy-named container,
// wired so every create, stop and route write lands in one timeline.
type rollRig struct {
	app generated.Application
	dep generated.Deployment
	rt  *testutil.MockContainerRuntime
	pm  *testutil.MockProxyManager
	tl  *timeline
}

func newRollRig(t *testing.T, healthType string, mut ...func(*generated.CreateApplicationParams, *generated.CreateDeploymentParams)) *rollRig {
	t.Helper()
	t.Cleanup(func() { _ = testutil.TruncateAll(context.Background(), testPool) })
	ctx := context.Background()
	app, dep := seedApp(t, mut...)
	seedDomain(t, app, "roll.example.test")
	_, err := testPool.Exec(ctx, `UPDATE application_process_types SET health_check_type = $2, health_check_command = 'true' WHERE application_id = $1 AND routable`, app.ID, healthType)
	require.NoError(t, err)
	// The application is up and serving its old generation.
	_, err = testQueries.UpdateApplicationStatus(ctx, generated.UpdateApplicationStatusParams{ID: app.ID, Status: status.ApplicationRunning})
	require.NoError(t, err)

	tl := &timeline{}
	rt := &testutil.MockContainerRuntime{
		CreateFunc: func(cfg runtime.ContainerConfig) (string, error) {
			tl.add("create:" + cfg.Name)
			return "id-" + cfg.Name, nil
		},
		StopFunc: func(_ context.Context, id string) error {
			tl.add("stop:" + id)
			return nil
		},
	}
	// The old generation exists; the new name does not (a fresh deployment id).
	rt.MissingNames = map[string]bool{newRoutable(app, dep, 1): true}
	return &rollRig{app: app, dep: dep, rt: rt, pm: &testutil.MockProxyManager{}, tl: tl}
}

func (r *rollRig) deploy(t *testing.T) error {
	t.Helper()
	h := newTestHandler(r.rt, orderedProxy{r.pm, r.tl})
	return h.HandleDeployTask(context.Background(), asynq.NewTask("deploy", mustMarshalPayload(t, r.app.ID, r.dep.ID)))
}

func (r *rollRig) web(t *testing.T) generated.ApplicationInstance {
	t.Helper()
	rows, err := testQueries.ListInstancesByApplication(context.Background(), r.app.ID)
	require.NoError(t, err)
	require.NotEmpty(t, rows)
	return rows[0]
}

// The roll, with the upstream list asserted at EACH step rather than only at the end.
func TestRoll_TheNewGenerationExistsBeforeTheRouteMovesAndTheOldOneOutlivesIt(t *testing.T) {
	r := newRollRig(t, "none")
	old, next := r.app.Slug, newRoutable(r.app, r.dep, 1)

	require.NoError(t, r.deploy(t))

	// The stage-stop events of a stop-before-create deploy would put "stop:old" first.
	// StopContainer is called by id or name; the old one is named, the new is created.
	assert.Equal(t, []string{
		"create:" + next,            // new beside old: both exist
		"route:" + old + "," + next, // old + new
		"route:" + next,             // new only
		"stop:" + old,               // the old generation is drained last
	}, filterEvents(r.tl.events(), next, old), "create -> route(old+new) -> route(new) -> stop(old)")

	assert.NotContains(t, r.rt.RemoveCalls, next, "the new container is never removed on success")
	assert.Equal(t, 1, countOf(r.rt.RemoveCalls, old), "the old generation is removed exactly once")

	// The row describes the CURRENT container, and records its deployment.
	row := r.web(t)
	assert.Equal(t, next, row.ContainerName)
	assert.Equal(t, "id-"+next, row.ContainerID.String)
	assert.Equal(t, r.dep.ID, row.DeploymentID)
}

// filterEvents keeps the events that matter to the roll, dropping the legacy-name
// sweep and any no-op removals.
func filterEvents(all []string, names ...string) []string {
	var out []string
	for _, e := range all {
		if strings.HasPrefix(e, "route:") || strings.HasPrefix(e, "route-removed") {
			out = append(out, e)
			continue
		}
		for _, n := range names {
			if strings.HasSuffix(e, ":"+n) {
				out = append(out, e)
				break
			}
		}
	}
	return out
}

// ⬅️ The point of the feature. The new container fails its health gate: it is removed,
// the OLD container is still running, Caddy never carried the new one, and the
// application is NOT flagged errored. Before D2 the old container was already gone.
func TestRoll_AFailingNewGenerationLeavesTheOldOneServingAndTheApplicationUp(t *testing.T) {
	r := newRollRig(t, "command")
	r.rt.HealthStatus_ = "unhealthy"
	old, next := r.app.Slug, newRoutable(r.app, r.dep, 1)
	before := r.web(t)

	require.Error(t, r.deploy(t))

	assert.NotContains(t, r.rt.StopCalls, old, "the old container is never stopped")
	assert.NotContains(t, r.rt.RemoveCalls, old, "or removed")
	assert.Contains(t, r.rt.RemoveCalls, "id-"+next, "the failed new container is removed")
	assert.Empty(t, r.pm.AddedRoutes, "the route never carried the new generation")
	assert.Empty(t, r.pm.RemovedRoutes, "and the old route was not removed")

	app, err := testQueries.GetApplication(context.Background(), r.app.ID)
	require.NoError(t, err)
	assert.Equal(t, status.ApplicationRunning, app.Status, "still serving: not flagged errored")

	after := r.web(t)
	assert.Equal(t, before.ContainerName, after.ContainerName, "the row still names the old container")
	assert.Equal(t, before.DeploymentID, after.DeploymentID)

	dep, err := testQueries.GetDeployment(context.Background(), r.dep.ID)
	require.NoError(t, err)
	assert.Equal(t, status.DeploymentFailed, dep.Status)
}

// A failure AFTER the route carried old+new: the compensator must RESTORE the old
// upstreams, not remove the route of an application that is still serving.
func TestRoll_ACompensatorRestoresTheOldRouteInsteadOfRemovingIt(t *testing.T) {
	r := newRollRig(t, "none")
	old, next := r.app.Slug, newRoutable(r.app, r.dep, 1)
	// finalize writes the instance rows; make that the failing step.
	_, err := testPool.Exec(context.Background(), `
		CREATE FUNCTION roll_test_refuse() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'roll test: refused'; END $$;
		CREATE TRIGGER roll_test_refuse BEFORE UPDATE ON application_instances FOR EACH ROW EXECUTE FUNCTION roll_test_refuse();`)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = testPool.Exec(context.Background(), `DROP TRIGGER IF EXISTS roll_test_refuse ON application_instances; DROP FUNCTION IF EXISTS roll_test_refuse()`)
	})

	require.Error(t, r.deploy(t))

	routes := filterEvents(r.tl.events(), next, old)
	assert.Equal(t, []string{"route:" + old + "," + next, "route:" + old}, routes[len(routes)-2:],
		"old+new, then the compensator writes the old upstreams back")
	assert.Empty(t, r.pm.RemovedRoutes, "the route is restored, never removed")
	assert.NotContains(t, r.rt.StopCalls, old)
	assert.NotContains(t, r.rt.RemoveCalls, old)
	assert.Contains(t, r.rt.RemoveCalls, "id-"+next)
}

// With nothing to restore (a first deploy) the compensator removes the route, as it
// always has.
func TestRoll_AFirstDeployHasNoOldGenerationAndItsRouteIsWrittenOnce(t *testing.T) {
	r := newRollRig(t, "none")
	r.rt.ContainerMissing_ = true
	next := newRoutable(r.app, r.dep, 1)

	require.NoError(t, r.deploy(t))

	require.Len(t, r.pm.AddedRoutes, 2)
	assert.Equal(t, []string{"http://" + next + ":8080"}, r.pm.AddedRoutes[0].Upstreams, "nothing old to keep")
	assert.Equal(t, []string{"http://" + next + ":8080"}, r.pm.AddedRoutes[1].Upstreams)
	for _, c := range r.rt.StopCalls {
		assert.NotEqual(t, r.app.Slug, c, "there was no old container to drain")
	}
}

// The routable container answers to the application's stable slug; a worker does not.
func TestRoll_TheRoutableContainerCarriesTheSlugAsAnAliasAndAWorkerDoesNot(t *testing.T) {
	r := newRollRig(t, "none")
	worker := seedWorkerType(t, r.app, "worker", "run", nil)

	require.NoError(t, r.deploy(t))

	by := map[string]runtime.ContainerConfig{}
	for _, c := range r.rt.CreateCalls {
		by[c.Name] = c
	}
	assert.Equal(t, []string{r.app.Slug}, by[newRoutable(r.app, r.dep, 1)].NetworkAliases)
	assert.Empty(t, by[worker].NetworkAliases)
}

// replicas = 1, the common case: one container at the end, the application never
// without an upstream, and the row count unchanged.
func TestRoll_AtOneReplicaOneContainerRemainsAndTheRouteIsNeverEmpty(t *testing.T) {
	r := newRollRig(t, "none")
	require.NoError(t, r.deploy(t))

	for i, rt := range r.pm.AddedRoutes {
		assert.NotEmpty(t, rt.Upstreams, "route write %d", i)
	}
	assert.Equal(t, 1, countOf(createdNames(r.rt), newRoutable(r.app, r.dep, 1)))
	rows, err := testQueries.ListInstancesByApplication(context.Background(), r.app.ID)
	require.NoError(t, err)
	assert.Len(t, rows, 1, "a roll never invents or drops an instance row")
}

func createdNames(rt *testutil.MockContainerRuntime) []string {
	var out []string
	for _, c := range rt.CreateCalls {
		out = append(out, c.Name)
	}
	return out
}

// ⛔ A stateful application must not run two generations over one volume (two
// databases on one data directory): it keeps the stop-then-create sequence.
func TestRoll_AnApplicationWithAVolumeStillStopsTheOldContainerBeforeCreatingTheNewOne(t *testing.T) {
	r := newRollRig(t, "none")
	_, err := testQueries.CreateApplicationVolume(context.Background(), generated.CreateApplicationVolumeParams{
		ApplicationID: r.app.ID, Name: "data", MountPath: "/data",
	})
	require.NoError(t, err)

	require.NoError(t, r.deploy(t))

	ev := r.tl.events()
	stop, create := indexOf(ev, "stop:"+r.app.Slug), indexOf(ev, "create:"+r.app.Slug)
	require.GreaterOrEqual(t, stop, 0, "events: %v", ev)
	require.GreaterOrEqual(t, create, 0, "events: %v", ev)
	assert.Less(t, stop, create, "stopped before created: one writer on the volume at a time")
	assert.Equal(t, r.app.Slug, r.web(t).ContainerName, "created in place, under the stored name")
}

func indexOf(s []string, v string) int {
	for i, e := range s {
		if e == v {
			return i
		}
	}
	return -1
}

// The deploy gate and the API's `inert` flag are ONE predicate (instances.ResolveHealth),
// so they agree by construction; this asserts it on the cases that matter. An inert http
// check records 'skipped' -- the gate ran nothing -- while http with a path really
// probes (here it fails: nothing listens on the mock container's name).
func TestRoll_TheGateSkipsExactlyWhatTheResolverCallsInert(t *testing.T) {
	cases := []struct {
		name       string
		kind, path string
		inert      bool
	}{
		{"http with no path is inert", "http", "", true},
		{"none is a choice, not a gap", "none", "", false},
		{"http with a path is probed", "http", "/up", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := newRollRig(t, c.kind, func(a *generated.CreateApplicationParams, _ *generated.CreateDeploymentParams) {
				if c.path != "" {
					a.HealthCheckPath = pgtype.Text{String: c.path, Valid: true}
				}
			})
			// Short budget: the probed case must fail fast rather than wait two minutes.
			_, err := testPool.Exec(context.Background(), `UPDATE applications SET health_check_timeout_seconds = 1 WHERE id = $1`, r.app.ID)
			require.NoError(t, err)

			app, err := testQueries.GetApplication(context.Background(), r.app.ID)
			require.NoError(t, err)
			pt, err := testQueries.GetProcessTypeByName(context.Background(), generated.GetProcessTypeByNameParams{ApplicationID: r.app.ID, Name: "web"})
			require.NoError(t, err)
			// The type's own setting (the rig writes it) wins, as in a real deploy.
			resolved := instances.ResolveHealth(app, pt)
			require.Equal(t, c.inert, resolved.Inert, "the resolver's verdict")

			_ = r.deploy(t)

			dep, err := testQueries.GetDeployment(context.Background(), r.dep.ID)
			require.NoError(t, err)
			if c.inert || c.kind == "none" {
				assert.Equal(t, "skipped", dep.HealthStatus.String, "the gate checked nothing")
			} else {
				assert.Equal(t, "failing", dep.HealthStatus.String, "the gate really probed")
			}
		})
	}
}
