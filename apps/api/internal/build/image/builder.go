package image

import (
	"context"
	"fmt"

	"github.com/belune-hq/belune/internal/build"
	"github.com/belune-hq/belune/internal/runtime"
)

type AuthResolver interface {
	Resolve(context.Context, string) (*runtime.RegistryAuth, error)
}

// Builder handles pre-built images — just pull and deploy, no build step.
type Builder struct {
	runtime  runtime.ContainerRuntime
	resolver AuthResolver
}

func New(rt runtime.ContainerRuntime, resolver AuthResolver) *Builder {
	return &Builder{runtime: rt, resolver: resolver}
}

func (b *Builder) Name() string { return "image" }

func (b *Builder) CanBuild(ctx context.Context, sourceDir string) bool {
	return false // Only used when explicitly selected
}

func (b *Builder) Build(ctx context.Context, opts build.BuildOptions) (*build.BuildResult, error) {
	auth, err := b.resolver.Resolve(ctx, opts.ImageTag)
	if err != nil {
		return nil, err
	}
	if err := b.runtime.PullImage(ctx, opts.ImageTag, auth); err != nil {
		return nil, fmt.Errorf("pull image: %w", err)
	}
	return &build.BuildResult{ImageTag: opts.ImageTag}, nil
}
