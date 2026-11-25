package worker_test

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/hibiken/asynq"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/belune-hq/belune/internal/runtime"
	"github.com/belune-hq/belune/internal/service"
	"github.com/belune-hq/belune/internal/store/generated"
	"github.com/belune-hq/belune/internal/testutil"
)

func TestDeployAdoptedVolumeUsesLiteralAndNeverCreatesIt(t *testing.T) {
	ctx := context.Background()
	require.NoError(t, testutil.TruncateAll(ctx, testPool))
	t.Cleanup(func() { _ = testutil.TruncateAll(ctx, testPool) })
	app, dep := seedApp(t)
	require.NoError(t, testQueries.RegisterAdoptedVolume(ctx, "compose_data"))
	_, err := testQueries.CreateApplicationVolume(ctx, generated.CreateApplicationVolumeParams{ApplicationID: app.ID, Name: "data", MountPath: "/data", ExternalName: pgtype.Text{String: "compose_data", Valid: true}})
	require.NoError(t, err)
	rt := &testutil.MockContainerRuntime{ListVolumes_: []runtime.VolumeInfo{{Name: "compose_data"}}}
	require.NoError(t, deploy(t, app, dep, rt, &testutil.MockProxyManager{}))
	require.NotEmpty(t, rt.CreateCalls)
	assert.Equal(t, "/data", rt.CreateCalls[0].Volumes["compose_data"])
	assert.Empty(t, rt.CreateDataVolumeCalls, "adopted mounts must not call the idempotent create method")
}

type blockedAdoptionRuntime struct {
	*testutil.MockContainerRuntime
	checked chan struct{}
	proceed chan struct{}
	pruned  chan []string
}

func (r *blockedAdoptionRuntime) ListVolumes(ctx context.Context) ([]runtime.VolumeInfo, error) {
	close(r.checked)
	select {
	case <-r.proceed:
		return r.MockContainerRuntime.ListVolumes(ctx)
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}
func (r *blockedAdoptionRuntime) PruneVolumes(ctx context.Context, keep []string) error {
	r.pruned <- append([]string(nil), keep...)
	return r.MockContainerRuntime.PruneVolumes(ctx, keep)
}

func TestPruneWaitsForAdoptionAndReadsCommittedRegistry(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	require.NoError(t, testutil.TruncateAll(ctx, testPool))
	t.Cleanup(func() { _ = testutil.TruncateAll(context.Background(), testPool) })
	app, _ := seedApp(t)
	rt := &blockedAdoptionRuntime{MockContainerRuntime: &testutil.MockContainerRuntime{ListVolumes_: []runtime.VolumeInfo{{Name: "locked_data"}}}, checked: make(chan struct{}), proceed: make(chan struct{}), pruned: make(chan []string, 1)}
	var once sync.Once
	allow := func() { once.Do(func() { close(rt.proceed) }) }
	defer allow()
	h := newTestHandler(rt, nil)
	svc := service.NewApplicationService(testPool, testQueries, h.Runtimes, h.Keyring, t.TempDir(), nil)
	adopted := make(chan error, 1)
	go func() {
		_, err := svc.AdoptVolume(ctx, generated.CreateApplicationVolumeParams{ApplicationID: app.ID, Name: "data", MountPath: "/data", ExternalName: pgtype.Text{String: "locked_data", Valid: true}})
		adopted <- err
	}()
	select {
	case <-rt.checked:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	started, cleaned := make(chan struct{}), make(chan error, 1)
	go func() {
		close(started)
		cleaned <- h.HandleCleanupTask(ctx, asynq.NewTask("cleanup", []byte(`{"actions":["volumes"],"scheduled":false}`)))
	}()
	<-started
	select {
	case <-rt.pruned:
		t.Fatal("prune ran while adoption held the transaction lock")
	case <-time.After(200 * time.Millisecond):
	}
	allow()
	require.NoError(t, <-adopted)
	require.NoError(t, <-cleaned)
	select {
	case keep := <-rt.pruned:
		assert.Contains(t, keep, "locked_data", "prune must collect a fresh list after adoption commits")
	default:
		t.Fatal("prune did not run after lock release")
	}
}

func TestMissingAdoptedVolumeRefusesReplacementAndRestore(t *testing.T) {
	ctx := context.Background()
	require.NoError(t, testutil.TruncateAll(ctx, testPool))
	t.Cleanup(func() { _ = testutil.TruncateAll(ctx, testPool) })
	app, dep := seedApp(t)
	require.NoError(t, testQueries.RegisterAdoptedVolume(ctx, "missing_data"))
	vol, err := testQueries.CreateApplicationVolume(ctx, generated.CreateApplicationVolumeParams{ApplicationID: app.ID, Name: "data", MountPath: "/data", ExternalName: pgtype.Text{String: "missing_data", Valid: true}})
	require.NoError(t, err)
	rt := &testutil.MockContainerRuntime{}
	err = deploy(t, app, dep, rt, &testutil.MockProxyManager{})
	require.ErrorContains(t, err, "refusing to create an empty replacement")
	assert.Empty(t, rt.CreateCalls)
	assert.Empty(t, rt.CreateDataVolumeCalls)
	h := newTestHandler(rt, nil)
	payload := fmt.Sprintf(`{"application_volume_id":%q,"backup_id":%q}`, uuidString(vol.ID), uuidString(dep.ID))
	err = h.HandleRestoreVolumeTask(ctx, asynq.NewTask("restore_volume", []byte(payload)))
	require.ErrorContains(t, err, "restoring into an adopted volume is disabled")
	require.ErrorIs(t, err, asynq.SkipRetry)
}
