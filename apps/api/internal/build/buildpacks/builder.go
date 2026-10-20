package buildpacks

import (
	"context"
	"fmt"
	"log/slog"
	"os/exec"

	"github.com/belune-hq/belune/internal/build"
	"github.com/belune-hq/belune/internal/naming"
	"github.com/belune-hq/belune/internal/runtime"
)

// Builder runs Cloud Native Buildpacks (CNB) via `pack build`.
type Builder struct {
	runtime             runtime.ContainerRuntime
	defaultBuilderImage string
}

func New(rt runtime.ContainerRuntime) *Builder {
	return &Builder{
		runtime:             rt,
		defaultBuilderImage: DefaultBuilderImage,
	}
}

func (b *Builder) Name() string { return "buildpacks" }

func (b *Builder) CanBuild(ctx context.Context, sourceDir string) bool {
	if _, err := exec.LookPath("pack"); err != nil {
		slog.Debug("pack CLI not found, skipping buildpacks builder")
		return false
	}
	return true
}

func (b *Builder) Build(ctx context.Context, opts build.BuildOptions) (*build.BuildResult, error) {
	builderImage := b.defaultBuilderImage
	if opts.BuilderImage != "" {
		builderImage = opts.BuilderImage
	}

	args := []string{
		"build", opts.ImageTag,
		"--builder", builderImage,
		"--path", opts.SourceDir,
		"--trust-builder",
	}

	for _, bp := range opts.Buildpacks {
		args = append(args, "--buildpack", bp)
	}

	for k, v := range opts.Env {
		args = append(args, "--env", fmt.Sprintf("%s=%s", k, v))
	}

	// Wire persistent build+launch caches keyed by application ID so each app
	// reuses its own layer history across deploys. Without these flags pack
	// derives an auto-generated volume name from the image digest, which
	// churns every time the image tag changes (we tag each deploy uniquely).
	//
	// Pre-create the volumes so they carry the belune-cache label — auto-created
	// volumes would not be labelled and the periodic cleanup worker would
	// wipe them via PruneVolumes.
	if opts.ApplicationID != "" {
		buildVol := naming.CNBCacheVolumeName(opts.ApplicationID)
		launchVol := naming.CNBLaunchCacheVolumeName(opts.ApplicationID)
		if b.runtime != nil {
			if err := b.runtime.CreateCacheVolume(ctx, buildVol); err != nil {
				slog.Warn("ensure cnb build cache volume", "volume", buildVol, "error", err)
			}
			if err := b.runtime.CreateCacheVolume(ctx, launchVol); err != nil {
				slog.Warn("ensure cnb launch cache volume", "volume", launchVol, "error", err)
			}
		}
		args = append(args,
			"--cache", fmt.Sprintf("type=build;format=volume;name=%s", buildVol),
			"--cache", fmt.Sprintf("type=launch;format=volume;name=%s", launchVol),
		)
	}

	slog.Info("running pack build", "image", opts.ImageTag, "builder", builderImage)
	cmd := exec.CommandContext(ctx, "pack", args...)

	// stdout and stderr stream separately so each line's originating stream is
	// recorded (nil writer = discarded).
	cmd.Stdout = opts.StdoutWriter
	cmd.Stderr = opts.StderrWriter

	if err := cmd.Run(); err != nil {
		// The full build output is streamed to the stdout/stderr writers and
		// persisted as the deployment's build_logs, so keep the error itself
		// concise (it becomes the deployment's error_message) rather than
		// duplicating the entire output there.
		return nil, fmt.Errorf("pack build failed: %w", err)
	}

	return &build.BuildResult{
		ImageTag: opts.ImageTag,
	}, nil
}
