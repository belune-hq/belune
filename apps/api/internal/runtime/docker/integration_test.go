//go:build integration_docker

// Real-Docker integration tests for the runtime.ContainerRuntime implementation.
// Gated behind a build tag so CI can opt in without pulling docker into every
// unit-test run. Run with:
//
//	go test -tags=integration_docker ./internal/runtime/docker/...
//
// Each test talks to the docker daemon the test host exposes — it does not
// bring up its own daemon. Tests clean up the resources they create, but a
// failed run may leak a container or image; the prune helpers below mop that
// up on the next invocation.
package docker

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"io"
	"testing"
	"time"

	"github.com/docker/docker/api/types/volume"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/belune-hq/belune/internal/runtime"
)

const testImage = "busybox:latest"

func uniqueName(t *testing.T, prefix string) string {
	t.Helper()
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		t.Fatalf("rand: %v", err)
	}
	return "belune-it-" + prefix + "-" + hex.EncodeToString(b[:])
}

func newTestClient(t *testing.T) *Client {
	t.Helper()
	c, err := New()
	require.NoError(t, err, "docker client unavailable — is the daemon running?")
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// ensureTestImage pulls busybox once per test run (idempotent: subsequent pulls
// are near-instant because the layers are already local).
func ensureTestImage(t *testing.T, c *Client) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	require.NoError(t, c.PullImage(ctx, testImage), "pull %s", testImage)
}

func TestIntegration_ImagePullRemove(t *testing.T) {
	c := newTestClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	require.NoError(t, c.PullImage(ctx, testImage))
	// Pull again — must be idempotent and fast.
	start := time.Now()
	require.NoError(t, c.PullImage(ctx, testImage))
	assert.Less(t, time.Since(start), 30*time.Second, "second pull should be fast (cached)")
}

func TestIntegration_NetworkCreateRemove(t *testing.T) {
	c := newTestClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	name := uniqueName(t, "net")
	require.NoError(t, c.CreateNetwork(ctx, name))
	t.Cleanup(func() { _ = c.RemoveNetwork(context.Background(), name) })

	// Second call must be a no-op (idempotent).
	require.NoError(t, c.CreateNetwork(ctx, name))

	require.NoError(t, c.RemoveNetwork(ctx, name))
}

func TestIntegration_VolumeCreateRemove(t *testing.T) {
	c := newTestClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	name := uniqueName(t, "vol")
	require.NoError(t, c.CreateVolume(ctx, name))
	t.Cleanup(func() { _ = c.RemoveVolume(context.Background(), name) })

	require.NoError(t, c.RemoveVolume(ctx, name))
}

// volumeExists reports whether a named volume is present on the daemon.
func volumeExists(t *testing.T, c *Client, name string) bool {
	t.Helper()
	_, err := c.cli.VolumeInspect(context.Background(), name)
	return err == nil
}

// TestIntegration_PruneVolumes_PreservesDataAndCache is the data-loss guard for
// v0.0.26 application volumes: PruneVolumes must never reap persistent
// application data (belune-data) or build caches (belune-cache). A regression here
// silently deletes user data whenever the cleanup worker runs while an app's
// container is absent between deploys.
//
// Two guarantees are under test: labelled platform volumes survive, AND
// unlabelled-but-platform-named volumes survive (the name guard), because a
// volume can lose or never receive its labels yet still hold user data. Foreign
// dangling volumes are still reclaimed.
//
// NOTE: this calls PruneVolumes, which reaps dangling volumes on the host —
// running it may delete unrelated leftover volumes. It is gated behind the
// integration_docker build tag for that reason.
func TestIntegration_PruneVolumes_PreservesDataAndCache(t *testing.T) {
	c := newTestClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// Persistent application data volume — prune MUST keep it.
	data := uniqueName(t, "data")
	require.NoError(t, c.CreateDataVolume(ctx, data))
	t.Cleanup(func() { _ = c.RemoveVolume(context.Background(), data) })

	// Build cache volume — prune MUST keep it (existing guarantee).
	cache := uniqueName(t, "cache")
	require.NoError(t, c.CreateCacheVolume(ctx, cache))
	t.Cleanup(func() { _ = c.RemoveVolume(context.Background(), cache) })

	// Unlabeled but platform-named volume (legacy, or Docker-auto-created before
	// the labelling code) — the name guard MUST keep it, since it may hold user
	// data even though VolumeCreate never (re)applied the belune-data label.
	var lb [6]byte
	_, _ = rand.Read(lb[:])
	legacy := "belune-vol-itlegacy-" + hex.EncodeToString(lb[:])
	_, err := c.cli.VolumeCreate(ctx, volume.CreateOptions{Name: legacy}) // deliberately no labels
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.RemoveVolume(context.Background(), legacy) })

	// Foreign unlabeled dangling volume — prune SHOULD reclaim it.
	var fb [6]byte
	_, _ = rand.Read(fb[:])
	foreign := "foreign-it-" + hex.EncodeToString(fb[:])
	_, err = c.cli.VolumeCreate(ctx, volume.CreateOptions{Name: foreign})
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.RemoveVolume(context.Background(), foreign) })

	require.NoError(t, c.PruneVolumes(ctx))

	assert.True(t, volumeExists(t, c, data), "belune-data volume must survive prune")
	assert.True(t, volumeExists(t, c, cache), "belune-cache volume must survive prune")
	assert.True(t, volumeExists(t, c, legacy), "unlabeled platform-named volume must survive prune")
	assert.False(t, volumeExists(t, c, foreign), "foreign dangling volume should be reclaimed")
}

func TestIntegration_ContainerLifecycle(t *testing.T) {
	c := newTestClient(t)
	ensureTestImage(t, c)

	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Minute)
	defer cancel()

	networkName := uniqueName(t, "netlc")
	require.NoError(t, c.CreateNetwork(ctx, networkName))
	t.Cleanup(func() { _ = c.RemoveNetwork(context.Background(), networkName) })

	containerName := uniqueName(t, "ctr")
	id, err := c.CreateContainer(ctx, runtime.ContainerConfig{
		Name:    containerName,
		Image:   testImage,
		Cmd:     []string{"sh", "-c", "echo hello-from-belune-it && sleep 3600"},
		Env:     map[string]string{"BELUNE_TEST": "1"},
		Labels:  map[string]string{"belune-integration-test": "true"},
		Network: networkName,
		// Conservative limits so this works on constrained CI boxes.
		CPULimit:    0.5,
		MemoryLimit: 64 * 1024 * 1024, // 64 MiB
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.RemoveContainer(context.Background(), id) })

	require.NoError(t, c.StartContainer(ctx, id))

	// ListContainers filters by the managed-by label — this container must show up.
	listed, err := c.ListContainers(ctx)
	require.NoError(t, err)
	found := false
	for _, ci := range listed {
		if ci.ID == id {
			found = true
			assert.Equal(t, "true", ci.Labels["belune-integration-test"])
		}
	}
	assert.True(t, found, "created container should appear in ListContainers")

	// Poll stats until the daemon returns a non-zero sample — can take a second
	// after start while the cgroup is populated.
	var stats *runtime.ContainerResourceStats
	require.Eventually(t, func() bool {
		s, err := c.ContainerStats(ctx, id)
		if err != nil || s == nil {
			return false
		}
		stats = s
		return s.MemoryLimit > 0
	}, 10*time.Second, 500*time.Millisecond, "stats should populate after start")
	assert.InDelta(t, 64*1024*1024, stats.MemoryLimit, float64(4*1024*1024), "memory limit roughly 64 MiB")

	// Logs should contain the echo line.
	logs, err := c.ContainerLogs(ctx, id, false)
	require.NoError(t, err)
	defer logs.Close()
	buf, err := io.ReadAll(logs)
	require.NoError(t, err)
	assert.Contains(t, string(buf), "hello-from-belune-it")

	require.NoError(t, c.StopContainer(ctx, id))
	require.NoError(t, c.RemoveContainer(ctx, id))
}

func TestIntegration_NetworkAttachSecondary(t *testing.T) {
	c := newTestClient(t)
	ensureTestImage(t, c)

	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Minute)
	defer cancel()

	primary := uniqueName(t, "netp")
	secondary := uniqueName(t, "nets")
	require.NoError(t, c.CreateNetwork(ctx, primary))
	require.NoError(t, c.CreateNetwork(ctx, secondary))
	t.Cleanup(func() {
		_ = c.RemoveNetwork(context.Background(), primary)
		_ = c.RemoveNetwork(context.Background(), secondary)
	})

	name := uniqueName(t, "ctrnet")
	id, err := c.CreateContainer(ctx, runtime.ContainerConfig{
		Name:    name,
		Image:   testImage,
		Cmd:     []string{"sleep", "3600"},
		Network: primary,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.RemoveContainer(context.Background(), id) })

	require.NoError(t, c.StartContainer(ctx, id))
	require.NoError(t, c.ConnectContainerToNetwork(ctx, id, secondary))

	require.NoError(t, c.StopContainer(ctx, id))
	require.NoError(t, c.RemoveContainer(ctx, id))
}
