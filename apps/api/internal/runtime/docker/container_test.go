package docker

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"io"
	"testing"
	"time"

	"github.com/docker/docker/api/types/build"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/pkg/stdcopy"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/belune-hq/belune/internal/runtime"
)

const containerTestImage = "busybox:latest"

// newTestDockerClient creates a Client and skips the test if the daemon is unreachable.
// ListContainers is used as the reachability probe because it is the only method on
// Client that exercises a real Docker API call without requiring an existing resource
// (the unexported cli.Ping is not accessible from this test file).
func newTestDockerClient(t *testing.T) *Client {
	t.Helper()
	c, err := New()
	if err != nil {
		t.Skipf("docker client: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := c.ListContainers(ctx); err != nil {
		_ = c.Close()
		t.Skipf("docker daemon unreachable: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// uniqueTestContainerName returns a random container name for use in container_test.go.
// Named distinctly from integration_test.go's uniqueName to avoid a compile conflict
// when both files are compiled together (e.g. if the integration_docker tag is dropped).
func uniqueTestContainerName(t *testing.T) string {
	t.Helper()
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		t.Fatalf("rand: %v", err)
	}
	return "belune-test-" + hex.EncodeToString(b[:])
}

// TestContainerLifecycle exercises the create→start→list→stop→remove path
// against the real Docker daemon. Requires Docker to be running; skips gracefully
// if the daemon is unreachable.
func TestContainerLifecycle(t *testing.T) {
	c := newTestDockerClient(t)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	// Ensure the test image is present before creating the container.
	require.NoError(t, c.PullImage(ctx, containerTestImage), "pull %s", containerTestImage)

	name := uniqueTestContainerName(t)
	id, err := c.CreateContainer(ctx, runtime.ContainerConfig{
		Name:   name,
		Image:  containerTestImage,
		Cmd:    []string{"sleep", "3600"},
		Labels: map[string]string{"belune-unit-test": "true"},
	})
	require.NoError(t, err, "create container")
	// Ensure cleanup even if later steps fail.
	t.Cleanup(func() { _ = c.RemoveContainer(context.Background(), id) })

	require.NoError(t, c.StartContainer(ctx, id), "start container")

	// Newly started container must appear in ListContainers (filtered by the
	// managed-by label that CreateContainer stamps on every container).
	listed, err := c.ListContainers(ctx)
	require.NoError(t, err, "list containers after start")
	found := false
	for _, ci := range listed {
		if ci.ID == id {
			found = true
			assert.Equal(t, "running", ci.Status, "container should be running")
			assert.Equal(t, labelValue, ci.Labels[labelManagedBy],
				"CreateContainer must stamp the managed-by label so ListContainers can filter by it")
		}
	}
	assert.True(t, found, "started container should appear in ListContainers")

	require.NoError(t, c.StopContainer(ctx, id), "stop container")
	require.NoError(t, c.RemoveContainer(ctx, id), "remove container")

	// After removal the container must no longer appear in ListContainers.
	listed, err = c.ListContainers(ctx)
	require.NoError(t, err, "list containers after remove")
	for _, ci := range listed {
		assert.NotEqual(t, id, ci.ID, "removed container should not appear in ListContainers")
	}
}

// TestCreateContainerUnknownImage verifies that CreateContainer returns an error
// when the image does not exist locally. The context deadline acts as the hang
// guard — if CreateContainer blocks indefinitely the test fails via timeout rather
// than requiring a separate goroutine + timer.
func TestCreateContainerUnknownImage(t *testing.T) {
	c := newTestDockerClient(t)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	_, err := c.CreateContainer(ctx, runtime.ContainerConfig{
		Name:  uniqueTestContainerName(t),
		Image: "belune-nonexistent-image-for-testing-99999:latest",
	})
	assert.Error(t, err, "creating a container with an unknown image should return an error")
}

// buildEntrypointImage builds, from busybox, an image shaped like a Railpack-built
// one: it declares an ENTRYPOINT that is a shell taking a script, plus a default CMD.
func buildEntrypointImage(t *testing.T, c *Client, tag string) {
	t.Helper()
	dockerfile := "FROM " + containerTestImage + "\nENTRYPOINT [\"/bin/sh\",\"-c\"]\nCMD [\"echo IMAGE_DEFAULT_CMD\"]\n"
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	require.NoError(t, tw.WriteHeader(&tar.Header{Name: "Dockerfile", Mode: 0o644, Size: int64(len(dockerfile))}))
	_, err := tw.Write([]byte(dockerfile))
	require.NoError(t, err)
	require.NoError(t, tw.Close())

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	require.NoError(t, c.PullImage(ctx, containerTestImage))
	resp, err := c.cli.ImageBuild(ctx, &buf, build.ImageBuildOptions{Tags: []string{tag}, Remove: true, ForceRemove: true})
	require.NoError(t, err)
	_, err = io.Copy(io.Discard, resp.Body)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	t.Cleanup(func() {
		_, _ = c.cli.ImageRemove(context.Background(), tag, image.RemoveOptions{Force: true})
	})
}

// runToCompletion creates the container from cfg, runs it to exit, and returns its
// output.
func runToCompletion(t *testing.T, c *Client, cfg runtime.ContainerConfig) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	cfg.Name = uniqueTestContainerName(t)
	id, err := c.CreateContainer(ctx, cfg)
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.RemoveContainer(context.Background(), id) })
	require.NoError(t, c.StartContainer(ctx, id))
	waitCh, errCh := c.cli.ContainerWait(ctx, id, container.WaitConditionNotRunning)
	select {
	case <-waitCh:
	case err := <-errCh:
		require.NoError(t, err)
	}
	logs, err := c.ContainerLogs(ctx, id, false)
	require.NoError(t, err)
	defer func() { _ = logs.Close() }()
	var out bytes.Buffer
	_, _ = stdcopy.StdCopy(&out, &out, logs)
	return out.String()
}

// ⛔ MEASURED against an image shaped like a Railpack build (ENTRYPOINT is a shell
// that takes a script). Sending `sh -c <cmd>` as Cmd while the image's entrypoint is
// still in place runs NOTHING -- `/bin/sh -c sh -c "<cmd>"` executes `sh` and discards
// the command -- while clearing the entrypoint runs it, and leaving both nil keeps
// the image's own behaviour exactly.
func TestEntrypoint_ClearedRunsTheCommandInheritedSwallowsIt(t *testing.T) {
	c := newTestDockerClient(t)
	tag := "belune-entrypoint-test:" + uniqueTestContainerName(t)[len("belune-test-"):]
	buildEntrypointImage(t, c, tag)

	swallowed := runToCompletion(t, c, runtime.ContainerConfig{Image: tag, Cmd: []string{"sh", "-c", "echo MARKER_RAN"}})
	assert.NotContains(t, swallowed, "MARKER_RAN", "with the image's entrypoint intact the command never runs")

	cleared := runToCompletion(t, c, runtime.ContainerConfig{Image: tag, Entrypoint: []string{}, Cmd: []string{"sh", "-c", "echo MARKER_RAN"}})
	assert.Contains(t, cleared, "MARKER_RAN", "an empty, non-nil entrypoint clears the image's")

	inherited := runToCompletion(t, c, runtime.ContainerConfig{Image: tag})
	assert.Contains(t, inherited, "IMAGE_DEFAULT_CMD", "nil for both leaves the image exactly as built")
}
