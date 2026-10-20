package dockerfile

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/belune-hq/belune/internal/build"
	"github.com/belune-hq/belune/internal/runtime"
)

type Builder struct {
	runtime runtime.ContainerRuntime
}

func New(rt runtime.ContainerRuntime) *Builder {
	return &Builder{runtime: rt}
}

func (b *Builder) Name() string { return "dockerfile" }

func (b *Builder) CanBuild(ctx context.Context, sourceDir string) bool {
	_, err := os.Stat(filepath.Join(sourceDir, "Dockerfile"))
	return err == nil
}

func (b *Builder) Build(ctx context.Context, opts build.BuildOptions) (*build.BuildResult, error) {
	dockerfilePath := opts.DockerfilePath
	if dockerfilePath == "" {
		dockerfilePath = "Dockerfile"
	}

	if err := b.runtime.BuildImage(ctx, opts.SourceDir, dockerfilePath, opts.ImageTag); err != nil {
		return nil, fmt.Errorf("docker build: %w", err)
	}

	return &build.BuildResult{ImageTag: opts.ImageTag}, nil
}
