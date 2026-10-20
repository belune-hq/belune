package railpack

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"os"
	"os/exec"
	"time"

	"github.com/belune-hq/belune/internal/build"
)

type Builder struct{}

func New() *Builder {
	return &Builder{}
}

func (b *Builder) Name() string { return "railpack" }

func buildkitHost() string {
	if h := os.Getenv("BUILDKIT_HOST"); h != "" {
		return h
	}
	return "tcp://localhost:1234"
}

func (b *Builder) CanBuild(ctx context.Context, sourceDir string) bool {
	if _, err := exec.LookPath("railpack"); err != nil {
		slog.Debug("railpack CLI not found, skipping railpack builder")
		return false
	}
	return true
}

// CheckBuildKit verifies that the BuildKit daemon is reachable via TCP.
func CheckBuildKit() error {
	host := buildkitHost()
	u, err := url.Parse(host)
	if err != nil {
		return fmt.Errorf("invalid BUILDKIT_HOST %q: %w", host, err)
	}

	addr := u.Host
	if addr == "" {
		addr = u.Path
	}

	conn, err := net.DialTimeout(u.Scheme, addr, 3*time.Second)
	if err != nil {
		return fmt.Errorf("BuildKit daemon is not reachable at %s — ensure BuildKit is running (see infra/docker-compose.yml): %w", host, err)
	}
	conn.Close()
	return nil
}

func (b *Builder) Build(ctx context.Context, opts build.BuildOptions) (*build.BuildResult, error) {
	if err := CheckBuildKit(); err != nil {
		return nil, err
	}

	args := []string{
		"build", opts.SourceDir,
		"--name", opts.ImageTag,
	}

	for k, v := range opts.Env {
		args = append(args, "--env", fmt.Sprintf("%s=%s", k, v))
	}

	slog.Info("running railpack build", "image", opts.ImageTag, "source", opts.SourceDir)
	cmd := exec.CommandContext(ctx, "railpack", args...)
	cmd.Env = append(os.Environ(), "BUILDKIT_HOST="+buildkitHost())

	// stdout and stderr stream separately so each line's originating stream is
	// recorded (nil writer = discarded).
	cmd.Stdout = opts.StdoutWriter
	cmd.Stderr = opts.StderrWriter

	if err := cmd.Run(); err != nil {
		// The full build output is streamed to the stdout/stderr writers and
		// persisted as the deployment's build_logs, so keep the error itself
		// concise (it becomes the deployment's error_message) rather than
		// duplicating the entire output there.
		return nil, fmt.Errorf("railpack build failed: %w", err)
	}

	return &build.BuildResult{
		ImageTag: opts.ImageTag,
	}, nil
}
