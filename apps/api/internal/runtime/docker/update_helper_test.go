package docker

import (
	"context"
	"testing"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/belune-hq/belune/internal/runtime"
)

// TestUpdateHelperLabels_NeutralisesComposeMembership pins the one property the
// self-updater's survival depends on: Compose must not consider the helper part
// of any service.
//
// ⚠️ Why this needs a test rather than a comment. A container inherits its
// IMAGE's labels, and an image built by `docker compose build` carries
// com.docker.compose.project/service — this repo's own devcontainer image does,
// with project=infra, service=api. The helper runs a Belune image, and
// the updater runs `docker compose up -d`, which may remove the excess containers
// of a service it is recreating. That would kill the updater mid-update, right
// after it has rewritten .env.
//
// The published images happen not to carry those labels, so without the
// explicit override this holds by ABSENCE — it would break silently the day
// someone rebuilds with compose, and nothing would fail until a real update
// destroyed itself on a real install.
func TestUpdateHelperLabels_NeutralisesComposeMembership(t *testing.T) {
	labels := updateHelperLabels()

	// Compose matches a service by project AND service name. An empty value
	// overrides the image's (verified against a real image that carries them),
	// and cannot match a real project or service.
	for _, k := range []string{composeProjectLabel, composeServiceLabel} {
		v, ok := labels[k]
		assert.True(t, ok, "%s must be set explicitly, not left to inherit from the image", k)
		assert.Empty(t, v, "%s must be blanked so Compose cannot claim this container", k)
	}

	// Still discoverable as ours: the reaper spares running helpers by
	// LabelHelper, and the 409 conflict check finds it by LabelUpdateHelper.
	assert.Equal(t, labelValue, labels[labelManagedBy])
	assert.Equal(t, "true", labels[runtime.LabelHelper])
	assert.Equal(t, "true", labels[runtime.LabelUpdateHelper])
}

// TestSpawnUpdateHelper_FrozenContract pins what the dashboard actually asks
// Docker for, against the real daemon: the target image, entrypoint
// /usr/local/bin/belune-update, the version as its sole argv, and the
// root/host-network/bind/label set the updater depends on.
//
// ⚠️ Why this must be a Docker test. SpawnUpdateHelper is old code on every
// future update and can never be repaired, and reverting it to
// `bash scripts/update.sh` — restoring the blind spot this contract exists to
// remove — passed every mock-based test. Nothing else guards the Go path.
//
// The stub image has no /usr/local/bin/belune-update, so the START fails after
// the container was created; SpawnUpdateHelper then returns no id, so the
// container is found by its unique version argument instead. What matters is
// what was CREATED.
func TestSpawnUpdateHelper_FrozenContract(t *testing.T) {
	c := newTestDockerClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	require.NoError(t, c.PullImage(ctx, containerTestImage), "pull %s", containerTestImage)

	version := uniqueTestContainerName(t) // unique, so concurrent runs cannot collide
	_, err := c.SpawnUpdateHelper(ctx, runtime.UpdateHelperConfig{
		TargetImage: containerTestImage,
		WorkingDir:  "/opt/belune-test",
		Version:     version,
	})
	require.Error(t, err, "the stub image has no updater, so starting must fail")

	list, err := c.cli.ContainerList(ctx, container.ListOptions{
		All:     true,
		Filters: filters.NewArgs(filters.Arg("label", runtime.LabelUpdateHelper+"=true")),
	})
	require.NoError(t, err)
	var found *container.InspectResponse
	for _, item := range list {
		insp, err := c.cli.ContainerInspect(ctx, item.ID)
		require.NoError(t, err)
		if len(insp.Config.Cmd) == 1 && insp.Config.Cmd[0] == version {
			found = &insp
			break
		}
	}
	require.NotNil(t, found, "the helper container must have been created")
	t.Cleanup(func() {
		_ = c.cli.ContainerRemove(context.Background(), found.ID, container.RemoveOptions{Force: true})
	})

	cfg := found.Config
	assert.Equal(t, containerTestImage, cfg.Image)
	assert.Equal(t, []string{"/usr/local/bin/belune-update"}, []string(cfg.Entrypoint),
		"outside the bind-mounted install dir, or it resolves to the host's older copy")
	assert.Equal(t, []string{version}, []string(cfg.Cmd), "the target version is the sole argument")
	assert.Equal(t, "0:0", cfg.User)
	assert.Equal(t, "host", string(found.HostConfig.NetworkMode))
	assert.ElementsMatch(t,
		[]string{"/opt/belune-test:/opt/belune-test", "/var/run/docker.sock:/var/run/docker.sock"},
		found.HostConfig.Binds)
	for _, k := range []string{composeProjectLabel, composeServiceLabel} {
		v, ok := cfg.Labels[k]
		assert.True(t, ok && v == "", "%s must be present and empty", k)
	}
}
