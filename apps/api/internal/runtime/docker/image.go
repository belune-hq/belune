package docker

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/belune-hq/belune/internal/pkg/redact"
	"github.com/containerd/errdefs"
	"github.com/docker/docker/api/types/build"
	"github.com/docker/docker/api/types/filters"
	imagetypes "github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/registry"
	"github.com/docker/docker/pkg/jsonmessage"

	"github.com/belune-hq/belune/internal/pkg/metrics"
	"github.com/belune-hq/belune/internal/runtime"
)

func (c *Client) PullImage(ctx context.Context, image string, auth *runtime.RegistryAuth) (err error) {
	defer func() { metrics.RecordDockerOp("pull_image", err) }()
	host, err := runtime.RegistryHost(image)
	if err != nil {
		return err
	}
	options := imagetypes.PullOptions{}
	if auth != nil {
		if auth.ServerAddress != host {
			return fmt.Errorf("registry credential host does not match %s", host)
		}
		options.RegistryAuth, err = registry.EncodeAuthConfig(registry.AuthConfig{
			Username: auth.Username, Password: auth.Password, ServerAddress: host,
		})
		if err != nil {
			return fmt.Errorf("encode registry authentication for %s: %w", host, err)
		}
	}
	// Daemon errors may arrive either as an HTTP error or inside a successful
	// streaming response. Returning nil for the latter silently deploys stale images.
	pullError := func(cause error) error {
		message := cause.Error()
		if auth != nil && auth.Password != "" {
			message = strings.ReplaceAll(message, auth.Password, "[REDACTED]")
		}
		if options.RegistryAuth != "" {
			message = strings.ReplaceAll(message, options.RegistryAuth, "[REDACTED]")
		}
		lower := strings.ToLower(message)
		if errdefs.IsUnauthorized(cause) || errdefs.IsPermissionDenied(cause) || strings.Contains(lower, "unauthorized") || strings.Contains(lower, "authentication required") || strings.Contains(lower, "denied") || strings.Contains(lower, "no basic auth credentials") {
			if auth == nil {
				return fmt.Errorf("no credentials configured for %s — add a registry login for that host", host)
			}
			return fmt.Errorf("authentication failed for %s — add or correct the registry login for that host", host)
		}
		return fmt.Errorf("pull image from %s: %s", host, redact.Error(message))
	}
	reader, err := c.cli.ImagePull(ctx, image, options)
	if err != nil {
		return pullError(err)
	}
	defer func() { _ = reader.Close() }()
	decoder := json.NewDecoder(reader)
	for {
		var message struct {
			Error        *jsonmessage.JSONError `json:"errorDetail"`
			ErrorMessage string                 `json:"error"`
		}
		if err := decoder.Decode(&message); err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return pullError(err)
		}
		if message.Error != nil {
			return pullError(message.Error)
		}
		if message.ErrorMessage != "" {
			return pullError(errors.New(message.ErrorMessage))
		}
	}
}

func (c *Client) BuildImage(ctx context.Context, contextDir, dockerfile, tag string) error {
	// Create a tar archive of the context directory
	buf, err := tarDirectory(contextDir)
	if err != nil {
		return fmt.Errorf("tar context dir: %w", err)
	}

	resp, err := c.cli.ImageBuild(ctx, buf, build.ImageBuildOptions{
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

		// Preserve links rather than copying their target into a zero-size entry.
		link := ""
		if info.Mode()&os.ModeSymlink != 0 {
			link, err = os.Readlink(path)
			if err != nil {
				return fmt.Errorf("readlink %s: %w", relPath, err)
			}
		}
		header, err := tar.FileInfoHeader(info, link)
		if err != nil {
			return fmt.Errorf("archive header %s: %w", relPath, err)
		}
		header.Name = filepath.ToSlash(relPath)
		writeHeader := func() error {
			if err := tw.WriteHeader(header); err != nil {
				return fmt.Errorf("write header %s: %w", relPath, err)
			}
			return nil
		}
		if !info.Mode().IsRegular() {
			return writeHeader()
		}

		f, err := os.Open(path)
		if err != nil {
			return fmt.Errorf("open %s: %w", relPath, err)
		}
		defer f.Close()
		// Size from the descriptor, then bound the copy so later growth cannot
		// overflow the entry. Shrinkage must fail rather than truncate silently.
		st, err := f.Stat()
		if err != nil {
			return fmt.Errorf("stat %s: %w", relPath, err)
		}
		header.Size = st.Size()
		if err := writeHeader(); err != nil {
			return err
		}
		if _, err := io.CopyN(tw, f, header.Size); err != nil {
			return fmt.Errorf("%s changed while the build context was being archived: %w", relPath, err)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	if err := tw.Close(); err != nil {
		return nil, err
	}

	return &buf, nil
}
