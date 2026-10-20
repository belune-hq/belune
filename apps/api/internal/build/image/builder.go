package image

import (
	"context"
	"fmt"

	"github.com/belune-hq/belune/internal/build"
	"github.com/belune-hq/belune/internal/runtime"
)

// Builder handles pre-built images — just pull and deploy, no build step.
type Builder struct {
	runtime runtime.ContainerRuntime
}

func New(rt runtime.ContainerRuntime) *Builder {
	return &Builder{runtime: rt}
}

func (b *Builder) Name() string { return "image" }

func (b *Builder) CanBuild(ctx context.Context, sourceDir string) bool {
	return false // Only used when explicitly selected
}

func (b *Builder) Build(ctx context.Context, opts build.BuildOptions) (*build.BuildResult, error) {
	if err := b.runtime.PullImage(ctx, opts.ImageTag); err != nil {
		return nil, fmt.Errorf("pull image: %w", err)
	}
	return &build.BuildResult{ImageTag: opts.ImageTag}, nil
}
