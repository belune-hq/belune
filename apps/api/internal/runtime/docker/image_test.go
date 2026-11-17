package docker

import (
	"archive/tar"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestTarDirectorySymlinkWriteTooLongRegression(t *testing.T) {
	for _, tc := range []struct {
		name, target      string
		directory, broken bool
	}{
		{name: "non-empty file", target: "target"},
		{name: "directory", target: "target", directory: true},
		{name: "broken", target: "missing", broken: true},
		{name: "outside context", target: "../outside", broken: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			if !tc.broken {
				if tc.directory {
					require.NoError(t, os.Mkdir(filepath.Join(root, tc.target), 0755))
					require.NoError(t, os.WriteFile(filepath.Join(root, tc.target, "child"), []byte("child contents"), 0644))
				} else {
					require.NoError(t, os.WriteFile(filepath.Join(root, tc.target), []byte("non-empty contents"), 0644))
				}
			}
			require.NoError(t, os.Symlink(tc.target, filepath.Join(root, "link")))
			archive, err := tarDirectory(root)
			require.NoError(t, err)
			tr := tar.NewReader(archive)
			links := 0
			for {
				h, err := tr.Next()
				if err == io.EOF {
					break
				}
				require.NoError(t, err)
				body, err := io.ReadAll(tr)
				require.NoError(t, err)
				require.NotContains(t, h.Name, "link/")
				if h.Name == "link" {
					links++
					require.Equal(t, byte(tar.TypeSymlink), h.Typeflag)
					require.Equal(t, tc.target, h.Linkname)
					require.Zero(t, h.Size)
					require.Empty(t, body)
				}
			}
			require.Equal(t, 1, links)
		})
	}
}

func TestTarDirectoryRegularFilesAndDirectories(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(root, "dir"), 0755))
	require.NoError(t, os.Mkdir(filepath.Join(root, ".git"), 0755))
	require.NoError(t, os.WriteFile(filepath.Join(root, ".git", "config"), []byte("excluded"), 0644))
	require.NoError(t, os.WriteFile(filepath.Join(root, "dir", "file"), []byte("contents"), 0644))
	require.NoError(t, os.WriteFile(filepath.Join(root, "empty"), nil, 0644))
	archive, err := tarDirectory(root)
	require.NoError(t, err)
	tr := tar.NewReader(archive)
	seen := map[string]bool{}
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		require.NoError(t, err)
		body, err := io.ReadAll(tr)
		require.NoError(t, err)
		seen[h.Name] = true
		switch h.Name {
		case ".", "dir":
			require.Equal(t, byte(tar.TypeDir), h.Typeflag)
		case "dir/file":
			require.Equal(t, byte(tar.TypeReg), h.Typeflag)
			require.Equal(t, int64(8), h.Size)
			require.Equal(t, "contents", string(body))
		case "empty":
			require.Equal(t, byte(tar.TypeReg), h.Typeflag)
			require.Zero(t, h.Size)
			require.Empty(t, body)
		default:
			t.Fatalf("unexpected archive entry %q", h.Name)
		}
	}
	require.Len(t, seen, 4)
}
