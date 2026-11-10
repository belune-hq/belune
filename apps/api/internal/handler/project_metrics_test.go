package handler_test

import (
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/belune-hq/belune/internal/runtime"
	"github.com/belune-hq/belune/internal/store/generated"
	"github.com/belune-hq/belune/internal/testutil"
)

// A real one-shot stats call takes ~2s, and the overview used to make them
// serially — 8+ running apps could never finish inside the 15s handler timeout.
// The mock delays each call so a serial loop is unmistakable: 8 x 300ms = 2.4s
// serial versus ~300ms overlapped.
func TestProjectMetrics_StatsRunConcurrently(t *testing.T) {
	resetDB(t)
	adminToken := env.SetupAdmin(t, "admin@test.com", "password123")
	project := env.CreateProject(t, adminToken, "Metrics Project", "metrics-project")
	projectID := extractID(project["id"])

	const apps = 8
	const perCall = 300 * time.Millisecond
	ids := make([]string, apps)
	slugs := make([]string, apps)
	var containers []runtime.ContainerInfo
	for i := range apps {
		app := env.CreateApplication(t, adminToken, projectID, map[string]any{
			"name":         fmt.Sprintf("Metrics App %d", i),
			"type":         "image",
			"build_type":   "image",
			"source_image": "nginx:latest",
		})
		ids[i] = extractID(app["id"])
		slugs[i], _ = app["slug"].(string)
		require.NotEmpty(t, slugs[i])
		containers = append(containers, runtime.ContainerInfo{Name: slugs[i], Status: "running"})
	}

	// slugs[0]'s stats call fails: it must degrade to zeros without affecting
	// or cancelling its siblings.
	failing := slugs[0]
	env.Runtime.ListContainers_ = containers
	env.Runtime.StatsFunc = func(_ context.Context, name string) (*runtime.ContainerResourceStats, error) {
		time.Sleep(perCall)
		if name == failing {
			return nil, fmt.Errorf("stats unavailable")
		}
		return &runtime.ContainerResourceStats{CPUPercent: 12.5, MemoryUsage: 1024, MemoryLimit: 4096}, nil
	}
	t.Cleanup(func() {
		env.Runtime.ListContainers_ = nil
		env.Runtime.StatsFunc = nil
	})

	start := time.Now()
	resp := env.DoRequest(t, "GET", "/api/projects/"+projectID+"/metrics", nil, testutil.AuthHeader(adminToken))
	elapsed := time.Since(start)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	body := testutil.ReadJSON(t, resp)

	assert.Less(t, elapsed, apps*perCall/2, "stats calls must overlap, not run one after another")
	require.Len(t, body, apps)
	for i, id := range ids {
		m, ok := body[id].(map[string]any)
		require.True(t, ok, "app %d missing from the overview", i)
		assert.Equal(t, "running", m["status"])
		if slugs[i] == failing {
			// Unknown, not zero: null renders as a dash, 0 as a confident "0%".
			assert.Nil(t, m["cpu_percent"])
			assert.Nil(t, m["memory_used"])
			continue
		}
		assert.EqualValues(t, 12.5, m["cpu_percent"])
		assert.EqualValues(t, 1024, m["memory_used"])
		assert.EqualValues(t, 4096, m["memory_limit"])
	}
}

// metricsFixture is one application whose routable instance has been renamed away
// from its slug, as every container is after 0.3.2's first roll.
type metricsFixture struct {
	projectID, appID string
	token            string
	appUUID          pgtype.UUID
	slug             string
	web              generated.ApplicationInstance
	webPT            generated.ApplicationProcessType
}

func newMetricsFixture(t *testing.T, webName string) *metricsFixture {
	t.Helper()
	resetDB(t)
	f := &metricsFixture{token: env.SetupAdmin(t, "admin@test.com", "password123")}
	f.projectID = extractID(env.CreateProject(t, f.token, "Metrics Project", "metrics-project")["id"])
	app := env.CreateApplication(t, f.token, f.projectID, map[string]any{
		"name": "Metrics App", "type": "image", "build_type": "image", "source_image": "nginx:latest",
	})
	f.appID = extractID(app["id"])
	f.slug, _ = app["slug"].(string)
	require.NoError(t, f.appUUID.Scan(f.appID))

	ctx := context.Background()
	insts, err := env.Queries.ListInstancesByApplication(ctx, f.appUUID)
	require.NoError(t, err)
	require.Len(t, insts, 1)
	f.web = insts[0]
	// The 0.3.2 roll: the container is renamed and the slug survives only as a network alias.
	_, err = env.Pool.Exec(ctx, `UPDATE application_instances SET container_name = $2 WHERE id = $1`, f.web.ID, webName)
	require.NoError(t, err)
	f.web.ContainerName = webName
	pts, err := env.Queries.ListProcessTypesByApplication(ctx, f.appUUID)
	require.NoError(t, err)
	require.Len(t, pts, 1)
	f.webPT = pts[0]

	t.Cleanup(func() {
		env.Runtime.ListContainers_ = nil
		env.Runtime.StatsFunc = nil
	})
	return f
}

func (f *metricsFixture) addInstance(t *testing.T, pt pgtype.UUID, idx int32, name string) {
	t.Helper()
	_, err := env.Queries.CreateInstance(context.Background(), generated.CreateInstanceParams{
		ApplicationID: f.appUUID, ProcessTypeID: pt, Idx: idx, ContainerName: name, Status: "running",
	})
	require.NoError(t, err)
}

func (f *metricsFixture) get(t *testing.T) map[string]any {
	t.Helper()
	resp := env.DoRequest(t, "GET", "/api/projects/"+f.projectID+"/metrics", nil, testutil.AuthHeader(f.token))
	require.Equal(t, http.StatusOK, resp.StatusCode)
	m, ok := testutil.ReadJSON(t, resp)[f.appID].(map[string]any)
	require.True(t, ok, "application missing from the overview")
	return m
}

// The regression: the container is found through its instance row, not its slug. The
// handler used to key on the slug, answered 200 with empty metrics for every deployed
// app, and passed every test -- so this asserts the SAMPLED VALUES, never the status code.
func TestProjectMetrics_FindsRenamedContainerThroughInstanceRow(t *testing.T) {
	name := "app-web-1-5726bda2"
	f := newMetricsFixture(t, name)
	require.NotEqual(t, f.slug, name)

	env.Runtime.ListContainers_ = []runtime.ContainerInfo{
		{Name: name, Status: "running", CreatedAt: time.Now().Add(-time.Hour)},
	}
	env.Runtime.StatsFunc = func(_ context.Context, n string) (*runtime.ContainerResourceStats, error) {
		if n != name {
			return nil, fmt.Errorf("no such container %q (stats resolve names, not network aliases)", n)
		}
		return &runtime.ContainerResourceStats{CPUPercent: 7.5, MemoryUsage: 2048, MemoryLimit: 8192}, nil
	}

	m := f.get(t)
	assert.EqualValues(t, 7.5, m["cpu_percent"])
	assert.EqualValues(t, 2048, m["memory_used"])
	assert.EqualValues(t, 8192, m["memory_limit"])
	assert.Equal(t, "running", m["status"])
	assert.GreaterOrEqual(t, m["uptime_seconds"], float64(3500), "uptime comes from the container, not the row")
}

// Status follows Docker, not the stored row: a stopped container must not read "running".
func TestProjectMetrics_StatusFollowsTheContainerNotTheRow(t *testing.T) {
	name := "app-web-1-aaaa1111"
	f := newMetricsFixture(t, name)
	_, err := env.Pool.Exec(context.Background(), `UPDATE applications SET status = 'running' WHERE id = $1`, f.appUUID)
	require.NoError(t, err)

	env.Runtime.ListContainers_ = []runtime.ContainerInfo{{Name: name, Status: "exited"}}
	m := f.get(t)
	assert.Equal(t, "exited", m["status"])
	assert.Nil(t, m["cpu_percent"], "a container that is not running has no usage to report")
}

// An application with nothing sampled returns null usage, not 0: the UI renders null as
// a dash, and a confident 0% would be a claim nobody measured.
func TestProjectMetrics_UnsampledUsageIsNullNotZero(t *testing.T) {
	f := newMetricsFixture(t, "app-web-1-bbbb2222")
	env.Runtime.ListContainers_ = nil // Docker lists nothing for this app

	m := f.get(t)
	assert.Nil(t, m["cpu_percent"])
	assert.Nil(t, m["memory_used"])
	_, present := m["cpu_percent"]
	assert.True(t, present, "the key is present and null, not omitted")
}

// Two routable replicas report their SUM; a worker's usage is not folded in.
func TestProjectMetrics_SumsRoutableReplicasAndExcludesWorkers(t *testing.T) {
	a, b, w := "app-web-1-cccc3333", "app-web-2-dddd4444", "app-worker-1"
	f := newMetricsFixture(t, a)
	f.addInstance(t, f.webPT.ID, 2, b)
	worker, err := env.Queries.CreateProcessType(context.Background(), generated.CreateProcessTypeParams{
		ApplicationID: f.appUUID, Name: "worker", Replicas: 1, Routable: false,
	})
	require.NoError(t, err)
	f.addInstance(t, worker.ID, 1, w)

	env.Runtime.ListContainers_ = []runtime.ContainerInfo{
		{Name: a, Status: "running"}, {Name: b, Status: "running"}, {Name: w, Status: "running"},
	}
	env.Runtime.StatsFunc = func(_ context.Context, n string) (*runtime.ContainerResourceStats, error) {
		switch n {
		case a:
			return &runtime.ContainerResourceStats{CPUPercent: 10, MemoryUsage: 1000, MemoryLimit: 4000}, nil
		case b:
			return &runtime.ContainerResourceStats{CPUPercent: 5, MemoryUsage: 500, MemoryLimit: 4000}, nil
		case w:
			return &runtime.ContainerResourceStats{CPUPercent: 80, MemoryUsage: 99999, MemoryLimit: 99999}, nil
		}
		return nil, fmt.Errorf("unexpected container %q", n)
	}

	m := f.get(t)
	assert.EqualValues(t, 15, m["cpu_percent"], "10 + 5; the worker's 80 is not the web's")
	assert.EqualValues(t, 1500, m["memory_used"])
	assert.EqualValues(t, 8000, m["memory_limit"])
}
