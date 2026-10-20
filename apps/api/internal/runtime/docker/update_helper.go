package docker

import (
	"context"
	"fmt"

	"github.com/docker/docker/api/types/container"

	"github.com/belune-hq/belune/internal/runtime"
)

// SpawnUpdateHelper launches the TARGET image's /usr/local/bin/belune-update in
// a detached helper container and returns as soon as it has started — it does
// not attach, stream, wait, or remove it. See runtime.ContainerRuntime for why:
// the whole point is that this container outlives the one calling it.
//
// ⚠️ THE CONTRACT BELOW IS FROZEN. This function is old code on every future
// update — the running Belune is always the version being updated FROM — so it
// can never be repaired retroactively. It therefore knows exactly two things:
// the image to run (already pulled by the caller) and the path
// /usr/local/bin/belune-update with the target version as its sole argument.
// Everything else — what the updater does, what language it is in — lives in
// the target image and is free to change. The path must stay outside the
// bind-mounted install dir: scripts/update.sh there is the HOST's old copy, and
// running that is the blind spot this contract exists to close.
//
// Two departures from RunHelper's helpers, both required by what the updater
// itself does inside the container:
//   - Runs as root (User "0:0"): it chowns host directories and talks to the
//     mounted Docker socket, the same as it does when a real operator runs it
//     via sudo.
//   - Host networking, not RunHelper's NetworkMode "none": its health-wait step
//     curls http://localhost:8080/healthz, which only reaches the recreated
//     belune container from the HOST's network namespace — a helper on its own
//     bridge network would never see it. Host networking also gives the helper
//     the same internet reachability the host has, which it needs to fetch the
//     target release's infra files.
func (c *Client) SpawnUpdateHelper(ctx context.Context, cfg runtime.UpdateHelperConfig) (string, error) {
	created, err := c.cli.ContainerCreate(ctx,
		&container.Config{
			Image: cfg.TargetImage,
			// Override the belune server entrypoint — this container's job is
			// to run the updater, not serve traffic.
			Entrypoint: []string{"/usr/local/bin/belune-update"},
			Cmd:        []string{cfg.Version},
			Env:        []string{"BELUNE_DIR=" + cfg.WorkingDir},
			User:       "0:0",
			WorkingDir: cfg.WorkingDir,
			Labels:     updateHelperLabels(),
		},
		&container.HostConfig{
			Binds: []string{
				cfg.WorkingDir + ":" + cfg.WorkingDir,
				"/var/run/docker.sock:/var/run/docker.sock",
			},
			NetworkMode: "host",
		},
		nil, nil, "",
	)
	if err != nil {
		return "", fmt.Errorf("create update helper: %w", err)
	}
	if err := c.cli.ContainerStart(ctx, created.ID, container.StartOptions{}); err != nil {
		return "", fmt.Errorf("start update helper: %w", err)
	}
	return created.ID, nil
}

// composeProjectLabel and composeServiceLabel are what Compose uses to decide
// which containers belong to a service. Named here rather than inlined because
// the whole point below is that they must NOT be inherited.
const (
	composeProjectLabel = "com.docker.compose.project"
	composeServiceLabel = "com.docker.compose.service"
)

// updateHelperLabels marks the helper as Belune's, and — the load-bearing half —
// explicitly clears the Compose labels it would otherwise inherit.
//
// ⚠️ A container inherits its IMAGE's labels, and an image built by
// `docker compose build` carries com.docker.compose.project/service. The helper
// runs a Belune image (now the target's), so on any install whose image was built
// that way the helper looks to Compose like a stray container of the belune
// service — and the updater runs `docker compose up -d`, which is entitled to remove the
// excess containers of a service it is recreating. The updater would kill
// itself, mid-update, right after rewriting .env.
//
// The images Belune publishes do not carry these (the Dockerfile sets no LABEL
// and the release workflow stamps only org.opencontainers.image.*), so today
// this holds by ABSENCE — nothing enforces it, and a self-hoster who rebuilds
// locally with `docker compose build` reintroduces exactly the dangerous case.
// Observed for real in this repo's own devcontainer image, which carries
// project=infra, service=api.
//
// Setting a label to "" overrides the inherited value rather than leaving it
// (verified against a real image that carries them), which is enough: Compose
// matches a service by project AND service name, and neither can match now.
func updateHelperLabels() map[string]string {
	return map[string]string{
		labelManagedBy:            labelValue,
		runtime.LabelHelper:       "true",
		runtime.LabelUpdateHelper: "true",
		composeProjectLabel:       "",
		composeServiceLabel:       "",
	}
}
