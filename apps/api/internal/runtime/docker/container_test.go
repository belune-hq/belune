package docker

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"testing"
	"time"

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
