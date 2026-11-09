package handler_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/belune-hq/belune/internal/store/generated"
	"github.com/belune-hq/belune/internal/worker"
)

func (f ptFixture) instances(t *testing.T, id string) []generated.ApplicationInstance {
	t.Helper()
	var u pgtype.UUID
	require.NoError(t, u.Scan(id))
	rows, err := env.Queries.ListInstancesByProcessType(context.Background(), u)
	require.NoError(t, err)
	return rows
}

func scaleTasks() []worker.ScalePayload {
	var out []worker.ScalePayload
	for _, task := range env.Asynq.Tasks {
		if task.TypeName != worker.TypeScaleProcessType {
			continue
		}
		var p worker.ScalePayload
		_ = json.Unmarshal(task.Payload, &p)
		out = append(out, p)
	}
	return out
}

// replicas = 0 on the routable type is refused with the reason; on a worker it pauses.
func TestScale_ZeroReplicasIsRefusedOnTheRoutableTypeAndAllowedOnAWorker(t *testing.T) {
	f := newPTFixture(t)
	web := f.webTypeID(t)

	code, out := f.do(t, "PUT", "/"+web, map[string]any{"replicas": 0})
	assert.Equal(t, http.StatusBadRequest, code)
	assert.Contains(t, out["error"], "routable")
	assert.EqualValues(t, 1, f.row(t, web).Replicas, "a refused request stores nothing")
	assert.Len(t, f.instances(t, web), 1)

	id := f.createWorker(t, "worker", nil)
	env.Asynq.Tasks = nil
	code, out = f.do(t, "PUT", "/"+id, map[string]any{"replicas": 0})
	require.Equal(t, http.StatusOK, code, "%v", out)
	assert.EqualValues(t, 0, f.row(t, id).Replicas)
	assert.Empty(t, f.instances(t, id), "the surplus row is gone")
	assert.Equal(t, "worker", f.row(t, id).Name, "the type row itself remains")

	tasks := scaleTasks()
	require.Len(t, tasks, 1)
	assert.Equal(t, []string{f.appSlug + "-worker-1"}, tasks[0].RemoveContainers, "the container to stop is named, since its row is gone")
	assert.False(t, tasks[0].CreateContainers)
}

func TestScale_UpMintsRowsInIdxOrderAndDownDeletesTheSurplus(t *testing.T) {
	f := newPTFixture(t)
	id := f.createWorker(t, "worker", nil)

	env.Asynq.Tasks = nil
	code, out := f.do(t, "PUT", "/"+id, map[string]any{"replicas": 3})
	require.Equal(t, http.StatusOK, code, "%v", out)
	rows := f.instances(t, id)
	require.Len(t, rows, 3)
	for i, r := range rows {
		assert.EqualValues(t, i+1, r.Idx)
		assert.Equal(t, f.appSlug+"-worker-"+string(rune('1'+i)), r.ContainerName)
		assert.Equal(t, "pending", r.Status)
	}
	tasks := scaleTasks()
	require.Len(t, tasks, 1)
	assert.True(t, tasks[0].CreateContainers)
	assert.Empty(t, tasks[0].RemoveContainers)

	env.Asynq.Tasks = nil
	code, _ = f.do(t, "PUT", "/"+id, map[string]any{"replicas": 1})
	require.Equal(t, http.StatusOK, code)
	rows = f.instances(t, id)
	require.Len(t, rows, 1)
	assert.EqualValues(t, 1, rows[0].Idx, "idx 1 is untouched")
	tasks = scaleTasks()
	require.Len(t, tasks, 1)
	assert.Equal(t, []string{f.appSlug + "-worker-2", f.appSlug + "-worker-3"}, tasks[0].RemoveContainers)

	// An unchanged count touches nothing and enqueues nothing.
	env.Asynq.Tasks = nil
	code, _ = f.do(t, "PUT", "/"+id, map[string]any{"replicas": 1})
	require.Equal(t, http.StatusOK, code)
	assert.Empty(t, scaleTasks())
}

func TestScale_OutOfRangeReplicasIsRefused(t *testing.T) {
	f := newPTFixture(t)
	id := f.createWorker(t, "worker", nil)
	for _, n := range []int{-1, 21} {
		code, out := f.do(t, "PUT", "/"+id, map[string]any{"replicas": n})
		assert.Equal(t, http.StatusBadRequest, code, "%d: %v", n, out)
	}
}

// Volumes belong to the APPLICATION and are mounted into every process type, so more
// than one container of ANY type is refused while one exists -- and allowed again once
// it is gone.
func TestScale_AnApplicationWithAVolumeRefusesMoreThanOneReplicaOfAnyType(t *testing.T) {
	f := newPTFixture(t)
	id := f.createWorker(t, "worker", nil)
	vol, err := env.Queries.CreateApplicationVolume(context.Background(), generated.CreateApplicationVolumeParams{
		ApplicationID: f.appUUID, Name: "data", MountPath: "/data",
	})
	require.NoError(t, err)

	code, out := f.do(t, "PUT", "/"+id, map[string]any{"replicas": 2})
	assert.Equal(t, http.StatusBadRequest, code)
	assert.Contains(t, out["error"], "volume")
	code, _ = f.do(t, "PUT", "/"+f.webTypeID(t), map[string]any{"replicas": 2})
	assert.Equal(t, http.StatusBadRequest, code, "the routable type too: there is no 'type with volumes'")
	code, _ = f.do(t, "POST", "", map[string]any{"name": "other", "command": "x", "replicas": 2})
	assert.Equal(t, http.StatusBadRequest, code, "creation too")

	// One replica is always fine, as is pausing.
	code, _ = f.do(t, "PUT", "/"+id, map[string]any{"replicas": 1})
	assert.Equal(t, http.StatusOK, code)

	require.NoError(t, env.Queries.DeleteApplicationVolume(context.Background(), vol.ID))
	code, out = f.do(t, "PUT", "/"+id, map[string]any{"replicas": 2})
	assert.Equal(t, http.StatusOK, code, "%v", out)
}

// Per-type limits: absent keeps, a value sets, 0 clears back to inheriting.
func TestScale_LimitsAreSetKeptAndCleared(t *testing.T) {
	f := newPTFixture(t)
	id := f.createWorker(t, "worker", map[string]any{"cpu_limit": 0.5, "memory_limit": 134217728})
	row := f.row(t, id)
	assert.True(t, row.CpuLimit.Valid)
	assert.InDelta(t, 0.5, row.CpuLimit.Float64, 0.0001)
	assert.EqualValues(t, 134217728, row.MemoryLimit.Int64)

	// An unrelated edit must not clear them (full-row UPDATE discipline).
	code, _ := f.do(t, "PUT", "/"+id, map[string]any{"command": "other"})
	require.Equal(t, http.StatusOK, code)
	row = f.row(t, id)
	assert.True(t, row.CpuLimit.Valid, "an omitted limit is kept")
	assert.True(t, row.MemoryLimit.Valid)

	code, _ = f.do(t, "PUT", "/"+id, map[string]any{"cpu_limit": 0})
	require.Equal(t, http.StatusOK, code)
	row = f.row(t, id)
	assert.False(t, row.CpuLimit.Valid, "0 clears back to inheriting")
	assert.True(t, row.MemoryLimit.Valid)

	code, _ = f.do(t, "PUT", "/"+id, map[string]any{"memory_limit": 1024})
	assert.Equal(t, http.StatusBadRequest, code, "below Docker's floor")
}

// ⛔ Scaling the ROUTABLE type down must take the removed replicas out of Caddy BEFORE
// their containers go. An "it was called" check would pass with the calls reversed,
// which is the whole bug, so both calls append to one list and the ORDER is asserted.
func TestScale_DownOnTheRoutableTypeReconcilesBeforeTheRemovalIsQueued(t *testing.T) {
	f := newPTFixture(t)
	web := f.webTypeID(t)
	code, out := f.do(t, "PUT", "/"+web, map[string]any{"replicas": 3})
	require.Equal(t, http.StatusOK, code, "%v", out)

	var order []string
	env.Reconciler.OnCall = func(s string) { order = append(order, s) }
	env.Asynq.OnCall = func(s string) { order = append(order, s) }
	t.Cleanup(func() { env.Reconciler.OnCall, env.Asynq.OnCall = nil, nil })

	code, out = f.do(t, "PUT", "/"+web, map[string]any{"replicas": 1})
	require.Equal(t, http.StatusOK, code, "%v", out)
	assert.Equal(t, []string{"reconcile", "enqueue:" + worker.TypeScaleProcessType}, order,
		"the proxy is updated first; only then are the containers queued for removal")
}

// A worker has no routes: scaling it down does not spend a reconcile pass.
func TestScale_DownOnAWorkerDoesNotReconcile(t *testing.T) {
	f := newPTFixture(t)
	id := f.createWorker(t, "worker", map[string]any{"replicas": 3})
	var order []string
	env.Reconciler.OnCall = func(s string) { order = append(order, s) }
	t.Cleanup(func() { env.Reconciler.OnCall = nil })

	code, _ := f.do(t, "PUT", "/"+id, map[string]any{"replicas": 1})
	require.Equal(t, http.StatusOK, code)
	assert.NotContains(t, order, "reconcile")
}
