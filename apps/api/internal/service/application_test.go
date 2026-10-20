package service_test

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/belune-hq/belune/internal/naming"
	"github.com/belune-hq/belune/internal/runtime"
	"github.com/belune-hq/belune/internal/service"
	"github.com/belune-hq/belune/internal/testutil"
)

func TestApplicationService_Create_FinalizesSlugAndWebhookSecret(t *testing.T) {
	t.Cleanup(func() { truncate(t) })

	_, project := seedUserAndProject(t)
	rt := &testutil.MockContainerRuntime{}
	svc := service.NewApplicationService(testPool, testQueries, runtime.NewLocalRuntimes(rt), testKeyring, t.TempDir(), nil)

	app, err := svc.Create(context.Background(), service.CreateApplicationParams{
		ProjectID:   project.ID,
		ProjectSlug: project.Slug,
		Name:        "My Web App",
		BaseSlug:    "web",
		Type:        "image",
		SourceImage: "nginx:alpine",
		BuildType:   "image",
	})
	require.NoError(t, err)

	// Final slug must be {projectSlug}-{baseSlug}-{id[:8]} so it is unique
	// across re-creations and stable for naming.ContainerName.
	idStr := uuidString(app.ID)
	wantPrefix := project.Slug + "-web-" + idStr[:8]
	assert.Equal(t, wantPrefix, app.Slug, "Create should finalize slug with id prefix")

	// Persisted row should match the returned struct (no double-write surprises).
	persisted, err := testQueries.GetApplication(context.Background(), app.ID)
	require.NoError(t, err)
	assert.Equal(t, wantPrefix, persisted.Slug)

	// A non-empty webhook secret is required for git push verification — the
	// service must never persist an application without one, even when the
	// caller didn't ask for git auth.
	// Stored encrypted, never as plaintext.
	assert.NotEmpty(t, persisted.WebhookSecretEncrypted)
	assert.False(t, persisted.WebhookSecret.Valid)
}

func TestApplicationService_Create_EncryptsGitToken(t *testing.T) {
	t.Cleanup(func() { truncate(t) })

	_, project := seedUserAndProject(t)
	rt := &testutil.MockContainerRuntime{}
	svc := service.NewApplicationService(testPool, testQueries, runtime.NewLocalRuntimes(rt), testKeyring, t.TempDir(), nil)

	const plaintextPAT = "ghp_supersecrettokenvalue123"
	app, err := svc.Create(context.Background(), service.CreateApplicationParams{
		ProjectID:   project.ID,
		ProjectSlug: project.Slug,
		Name:        "App With PAT",
		BaseSlug:    "patapp",
		Type:        "git",
		SourceRepo:  "https://github.com/example/repo",
		BuildType:   "dockerfile",
		GitToken:    plaintextPAT,
	})
	require.NoError(t, err)

	persisted, err := testQueries.GetApplication(context.Background(), app.ID)
	require.NoError(t, err)

	// Bytes on disk must not contain the plaintext PAT — that's the entire
	// point of the keyring. Also assert decryption round-trips so we know we
	// stored ciphertext, not garbage.
	require.NotEmpty(t, persisted.GitCredentialsEncrypted)
	assert.False(t, bytes.Contains(persisted.GitCredentialsEncrypted, []byte(plaintextPAT)),
		"raw plaintext PAT must never appear in stored bytes")

	decrypted, err := testKeyring.Decrypt(persisted.GitCredentialsEncrypted)
	require.NoError(t, err)
	assert.Equal(t, plaintextPAT, string(decrypted),
		"stored ciphertext should decrypt back to original PAT")
}

func TestApplicationService_Delete_StopsAndRemovesAllContainerNames(t *testing.T) {
	t.Cleanup(func() { truncate(t) })

	_, project := seedUserAndProject(t)
	rt := &testutil.MockContainerRuntime{}
	svc := service.NewApplicationService(testPool, testQueries, runtime.NewLocalRuntimes(rt), testKeyring, t.TempDir(), nil)

	app, err := svc.Create(context.Background(), service.CreateApplicationParams{
		ProjectID:   project.ID,
		ProjectSlug: project.Slug,
		Name:        "Doomed",
		BaseSlug:    "doom",
		Type:        "image",
		SourceImage: "nginx:alpine",
		BuildType:   "image",
	})
	require.NoError(t, err)

	idStr := uuidString(app.ID)
	currentName := naming.ContainerName(project.Slug, app.Slug, idStr)
	intermediateName := naming.IntermediateContainerName(project.Slug, idStr)
	oldName := naming.OldContainerName(idStr)

	require.NoError(t, svc.Delete(context.Background(), app.ID, project.Slug, app.Slug))

	// Delete must reach for all three legacy/current naming variants — the
	// migration history left previous containers under different names, and a
	// missed cleanup leaks resources permanently. Containment check, not
	// equality, because Delete also calls RemoveVolume on cache volumes via
	// other runtime methods unrelated to this assertion.
	for _, name := range []string{currentName, intermediateName, oldName} {
		assert.Contains(t, rt.StopCalls, name,
			"Delete should attempt to stop container variant %q", name)
		assert.Contains(t, rt.RemoveCalls, name,
			"Delete should attempt to remove container variant %q", name)
	}

	// DB row must be gone (this is how we know the cascade fires for
	// deployments / env vars / domains in production).
	_, err = testQueries.GetApplication(context.Background(), app.ID)
	require.Error(t, err)
	assert.True(t, strings.Contains(err.Error(), "no rows"),
		"GetApplication should return ErrNoRows after Delete; got %v", err)
}

// TestApplicationService_Delete_ErasesVolumeBackupArchives is the second live
// bug this release fixes, and it is the mirror image of the database one.
//
// application_volume_backups cascades away with the volumes, so before this the
// archives survived with no row anywhere recording their keys: unreachable,
// unprunable, and still billed. Deleting a database destroyed its objects;
// deleting an application left them behind.
//
// The assertion is on the ARCHIVE, not the row. The row disappearing is exactly
// what used to happen while the storage stayed — checking rows would have
// passed against the bug.
func TestApplicationService_Delete_ErasesVolumeBackupArchives(t *testing.T) {
	t.Cleanup(func() { truncate(t) })
	ctx := context.Background()

	_, project := seedUserAndProject(t)
	rt := &testutil.MockContainerRuntime{}
	svc := service.NewApplicationService(testPool, testQueries, runtime.NewLocalRuntimes(rt), testKeyring, t.TempDir(), nil)

	app, err := svc.Create(ctx, service.CreateApplicationParams{
		ProjectID:   project.ID,
		ProjectSlug: project.Slug,
		Name:        "With Volumes",
		BaseSlug:    "vols",
		Type:        "image",
		SourceImage: "nginx:alpine",
		BuildType:   "image",
	})
	require.NoError(t, err)

	var volumeID string
	require.NoError(t, testPool.QueryRow(ctx,
		`INSERT INTO application_volumes (application_id, name, mount_path)
		 VALUES ($1, 'data', '/data') RETURNING id`, uuidString(app.ID)).Scan(&volumeID))

	// A real archive on disk, standing in for the stored object. A local
	// destination writes exactly this, so erasing it is erasing the artifact.
	archive := filepath.Join(t.TempDir(), "volume-backup.tar.gz")
	require.NoError(t, os.WriteFile(archive, []byte("ARCHIVE"), 0o600))

	_, err = testPool.Exec(ctx,
		`INSERT INTO application_volume_backups (application_volume_id, status, local_path, size_bytes)
		 VALUES ($1, 'succeeded', $2, 7)`, volumeID, archive)
	require.NoError(t, err)

	require.NoError(t, svc.Delete(ctx, app.ID, project.Slug, app.Slug))

	_, statErr := os.Stat(archive)
	assert.True(t, os.IsNotExist(statErr),
		"the volume backup archive must be erased, not left behind once its row cascades away")
}
