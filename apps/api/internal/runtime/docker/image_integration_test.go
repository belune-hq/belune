//go:build integration_docker

package docker

import (
	"archive/tar"
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/stretchr/testify/require"
)

func TestBuildImagePreservesContextSymlink(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	c := newTestClient(t)
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "Dockerfile"), []byte("FROM scratch\nCOPY . /context/\n"), 0644))
	require.NoError(t, os.WriteFile(filepath.Join(root, "target"), []byte("non-empty contents"), 0644))
	require.NoError(t, os.Symlink("target", filepath.Join(root, "link")))
	tag := uniqueName(t, "symlink") + ":test"
	require.NoError(t, c.BuildImage(ctx, root, "Dockerfile", tag))
	t.Cleanup(func() { _ = c.RemoveImage(context.Background(), tag) })
	// Inspect Docker's actual filesystem; the build stream can contain errors
	// even when BuildImage returns nil.
	ctr, err := c.cli.ContainerCreate(ctx, &container.Config{Image: tag, Cmd: []string{"/unused"}}, nil, nil, nil, "")
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.cli.ContainerRemove(context.Background(), ctr.ID, container.RemoveOptions{Force: true}) })
	stream, err := c.cli.ContainerExport(ctx, ctr.ID)
	require.NoError(t, err)
	defer stream.Close()
	tr := tar.NewReader(stream)
	found := false
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		require.NoError(t, err)
		if h.Name == "context/link" {
			found = true
			require.Equal(t, byte(tar.TypeSymlink), h.Typeflag)
			require.Equal(t, "target", h.Linkname)
		}
	}
	require.True(t, found, "built image must contain the symlink")
}
