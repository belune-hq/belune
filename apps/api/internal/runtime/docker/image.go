package docker

import (
	"archive/tar"
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/filters"
	imagetypes "github.com/docker/docker/api/types/image"

	"github.com/belune-hq/belune/internal/pkg/metrics"
	"github.com/belune-hq/belune/internal/runtime"
)

func (c *Client) PullImage(ctx context.Context, image string) (err error) {
	defer func() { metrics.RecordDockerOp("pull_image", err) }()

	reader, err := c.cli.ImagePull(ctx, image, imagetypes.PullOptions{})
	if err != nil {
		return fmt.Errorf("pull image %s: %w", image, err)
	}
	defer reader.Close()

	// Must drain the reader for the pull to complete
	_, err = io.Copy(io.Discard, reader)
	if err != nil {
		return fmt.Errorf("reading pull response: %w", err)
	}

	return nil
}

func (c *Client) BuildImage(ctx context.Context, contextDir, dockerfile, tag string) error {
	// Create a tar archive of the context directory
	buf, err := tarDirectory(contextDir)
	if err != nil {
		return fmt.Errorf("tar context dir: %w", err)
	}

	resp, err := c.cli.ImageBuild(ctx, buf, types.ImageBuildOptions{
		Dockerfile: dockerfile,
		Tags:       []string{tag},
		Remove:     true,
	})
	if err != nil {
		return fmt.Errorf("build image: %w", err)
	}
	defer resp.Body.Close()

	// Must drain the reader for the build to complete
	_, err = io.Copy(io.Discard, resp.Body)
	if err != nil {
		return fmt.Errorf("reading build response: %w", err)
	}

	return nil
}

func (c *Client) RemoveImage(ctx context.Context, img string) error {
	_, err := c.cli.ImageRemove(ctx, img, imagetypes.RemoveOptions{PruneChildren: true})
	if err != nil {
		return fmt.Errorf("remove image %s: %w", img, err)
	}
	return nil
}

func (c *Client) PruneImages(ctx context.Context) error {
	_, err := c.cli.ImagesPrune(ctx, filters.NewArgs(filters.Arg("dangling", "true")))
	if err != nil {
		return fmt.Errorf("prune images: %w", err)
	}
	return nil
}

// ListImages lists all images on the host for the read-only admin inspect page.
// SharedSize is requested so the overview page can report reclaimable bytes.
func (c *Client) ListImages(ctx context.Context) (result []runtime.ImageInfo, err error) {
	defer func() { metrics.RecordDockerOp("list_images", err) }()
	images, err := c.cli.ImageList(ctx, imagetypes.ListOptions{All: false, SharedSize: true})
	if err != nil {
		return nil, fmt.Errorf("list images: %w", err)
	}
	result = make([]runtime.ImageInfo, 0, len(images))
	for _, img := range images {
		result = append(result, runtime.ImageInfo{
			ID:         img.ID,
			RepoTags:   img.RepoTags,
			Size:       img.Size,
			SharedSize: img.SharedSize,
			Containers: img.Containers,
			Dangling:   isDanglingImage(img.RepoTags),
			Labels:     img.Labels,
			CreatedAt:  time.Unix(img.Created, 0),
		})
	}
	return result, nil
}

// ResolveImageDigest inspects a locally-present image and returns a
// digest-pinned reference ("repo@sha256:…") drawn from its RepoDigests. It
// ImageExists reports whether an image is present in the local Docker store.
// A "not found" inspect is reported as (false, nil); any other error is
// surfaced so a transient Docker failure is not mistaken for a missing image.
func (c *Client) ImageExists(ctx context.Context, ref string) (exists bool, err error) {
	defer func() { metrics.RecordDockerOp("image_exists", err) }()

	_, err = c.cli.ImageInspect(ctx, ref)
	if err != nil {
		// Docker returns a 404 whose message contains "No such image" when the
		// image is absent. A plain string match (as ConnectContainerToNetwork
		// does for "already exists") avoids pulling in errdefs for one check.
		if strings.Contains(err.Error(), "No such image") {
			return false, nil
		}
		return false, fmt.Errorf("inspect image %s: %w", ref, err)
	}
	return true, nil
}

// prefers a digest whose repository matches ref (an image can carry digests for
// several repos); otherwise it returns the first available. Returns "" (nil
// error) when the image has no repo digest.
func (c *Client) ResolveImageDigest(ctx context.Context, ref string) (digest string, err error) {
	defer func() { metrics.RecordDockerOp("resolve_image_digest", err) }()

	inspect, err := c.cli.ImageInspect(ctx, ref)
	if err != nil {
		return "", fmt.Errorf("inspect image %s: %w", ref, err)
	}
	if len(inspect.RepoDigests) == 0 {
		return "", nil
	}
	// Match the digest to ref's repository when possible (strip any tag first).
	repo := ref
	if at := strings.IndexByte(repo, '@'); at >= 0 {
		repo = repo[:at]
	}
	if colon := strings.LastIndexByte(repo, ':'); colon >= 0 && !strings.ContainsRune(repo[colon:], '/') {
		repo = repo[:colon]
	}
	for _, rd := range inspect.RepoDigests {
		if strings.HasPrefix(rd, repo+"@") {
			return rd, nil
		}
	}
	return inspect.RepoDigests[0], nil
}

// isDanglingImage reports whether an image has no usable tag (untagged), which
// is how the Docker CLI marks images eligible for dangling prune.
func isDanglingImage(repoTags []string) bool {
	for _, t := range repoTags {
		if t != "" && t != "<none>:<none>" {
			return false
		}
	}
	return true
}

// tarDirectory creates a tar archive from a directory.
func tarDirectory(dir string) (io.Reader, error) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)

	err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		// Get relative path
		relPath, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}

		// Skip .git directory
		if info.IsDir() && info.Name() == ".git" {
			return filepath.SkipDir
		}

		header, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}
		header.Name = strings.ReplaceAll(relPath, string(filepath.Separator), "/")

		if err := tw.WriteHeader(header); err != nil {
			return err
		}

		if info.IsDir() {
			return nil
		}

		f, err := os.Open(path)
		if err != nil {
			return err
		}
		defer f.Close()

		_, err = io.Copy(tw, f)
		return err
	})
	if err != nil {
		return nil, err
	}

	if err := tw.Close(); err != nil {
		return nil, err
	}

	return &buf, nil
}
