package service_test

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/belune-hq/belune/internal/runtime"
	"github.com/belune-hq/belune/internal/service"
	"github.com/belune-hq/belune/internal/store"
	"github.com/belune-hq/belune/internal/store/generated"
	"github.com/belune-hq/belune/internal/testutil"
)

func adoptionFixture(t *testing.T, rt runtime.ContainerRuntime) (*service.ApplicationService, generated.Application) {
	t.Helper()
	truncate(t)
	t.Cleanup(func() { truncate(t) })
	_, project := seedUserAndProject(t)
	svc := service.NewApplicationService(testPool, testQueries, runtime.NewLocalRuntimes(rt), testKeyring, t.TempDir(), nil)
	app, err := svc.Create(context.Background(), service.CreateApplicationParams{ProjectID: project.ID, ProjectSlug: project.Slug, Name: "Adopt", BaseSlug: "adopt", Type: "image", BuildType: "image", SourceImage: "busybox:latest"})
	require.NoError(t, err)
	return svc, app
}
func adoptionParams(app generated.Application, name string) generated.CreateApplicationVolumeParams {
	return generated.CreateApplicationVolumeParams{ApplicationID: app.ID, Name: "data", MountPath: "/data", ExternalName: pgtype.Text{String: name, Valid: true}}
}

func TestAdoptionRetainsRegistryAfterDetachAndAppDeletion(t *testing.T) {
	ctx := context.Background()
	rt := &testutil.MockContainerRuntime{ListVolumes_: []runtime.VolumeInfo{{Name: "compose_pgdata"}}}
	svc, app := adoptionFixture(t, rt)
	vol, err := svc.AdoptVolume(ctx, adoptionParams(app, "compose_pgdata"))
	require.NoError(t, err)
	require.Empty(t, rt.CreateDataVolumeCalls, "adoption must not create or relabel the Docker volume")
	// FK protects retention even from a future incorrectly implemented release.
	_, err = testPool.Exec(ctx, `DELETE FROM adopted_volumes WHERE name=$1`, "compose_pgdata")
	var pgErr *pgconn.PgError
	require.ErrorAs(t, err, &pgErr)
	require.Equal(t, "23503", pgErr.Code)
	duplicate := adoptionParams(app, "compose_pgdata")
	duplicate.Name = "duplicate"
	duplicate.MountPath = "/other"
	_, err = testQueries.CreateApplicationVolume(ctx, duplicate)
	require.ErrorAs(t, err, &pgErr)
	require.Equal(t, "23505", pgErr.Code)
	require.NoError(t, testQueries.DeleteApplicationVolume(ctx, vol.ID))
	retained, err := testQueries.ListAdoptedVolumes(ctx)
	require.NoError(t, err)
	require.Len(t, retained, 1)
	// A retained name is attachable again, but cannot be shared concurrently.
	rt.ListAllContainers_ = []runtime.ContainerInfo{{Name: "same-app", VolumeNames: []string{"compose_pgdata"}, Labels: map[string]string{"managed-by": "belune", "application-id": uuidString(app.ID)}}}
	_, err = svc.AdoptVolume(ctx, adoptionParams(app, "compose_pgdata"))
	require.NoError(t, err)
	_, err = svc.AdoptVolume(ctx, adoptionParams(app, "compose_pgdata"))
	require.ErrorIs(t, err, service.ErrExternalVolumeClaimed)
	require.NoError(t, svc.Delete(ctx, app.ID, "project", app.Slug))
	assert.NotContains(t, rt.RemoveVolumeCalls, "compose_pgdata")
	retained, err = testQueries.ListAdoptedVolumes(ctx)
	require.NoError(t, err)
	require.Len(t, retained, 1)
}

func TestAdoptionRejectsUnsafeOrMissingVolumes(t *testing.T) {
	for _, tc := range []struct {
		name       string
		volumes    []runtime.VolumeInfo
		containers []runtime.ContainerInfo
		want       error
	}{
		{"typo", nil, nil, service.ErrExternalVolumeMissing},
		{"belune-data", nil, nil, service.ErrExternalVolumeManaged},
		{"unsafe name", nil, nil, service.ErrExternalVolumeName},
		{"system", []runtime.VolumeInfo{{Name: "system", Labels: map[string]string{"belune-system": "postgres"}}}, nil, service.ErrExternalVolumeManaged},
		{"managed-peer", []runtime.VolumeInfo{{Name: "managed-peer"}}, []runtime.ContainerInfo{{Name: "previous-app", VolumeNames: []string{"managed-peer"}, Labels: map[string]string{"managed-by": "belune", "application-id": "another-application"}}}, service.ErrExternalVolumeConsumer},
		{"helper", []runtime.VolumeInfo{{Name: "helper"}}, []runtime.ContainerInfo{{Name: "backup-helper", VolumeNames: []string{"helper"}, Labels: map[string]string{"managed-by": "belune"}}}, service.ErrExternalVolumeConsumer},
		{"foreign", []runtime.VolumeInfo{{Name: "foreign"}}, []runtime.ContainerInfo{{Name: "compose-db", Status: "exited", VolumeNames: []string{"foreign"}}}, service.ErrExternalVolumeInUse},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rt := &testutil.MockContainerRuntime{ListVolumes_: tc.volumes, ListAllContainers_: tc.containers}
			svc, app := adoptionFixture(t, rt)
			_, err := svc.AdoptVolume(context.Background(), adoptionParams(app, tc.name))
			require.ErrorIs(t, err, tc.want)
			rows, err := testQueries.ListAdoptedVolumes(context.Background())
			require.NoError(t, err)
			assert.Empty(t, rows)
			assert.Empty(t, rt.CreateDataVolumeCalls)
		})
	}
}

func TestAdoptionLockTimeoutPrecedesExistenceCheck(t *testing.T) {
	rt := &testutil.MockContainerRuntime{ListVolumes_: []runtime.VolumeInfo{{Name: "held"}}}
	svc, app := adoptionFixture(t, rt)
	held, release, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	go func() {
		done <- store.WithVolumeClaimsLock(context.Background(), testPool, func(context.Context, *generated.Queries) error { close(held); <-release; return nil })
	}()
	<-held
	defer func() { close(release); require.NoError(t, <-done) }()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_, err := svc.AdoptVolume(ctx, adoptionParams(app, "held"))
	require.ErrorIs(t, err, store.ErrVolumeClaimsBusy)
	assert.Zero(t, rt.VolumeListCalls, "existence checks before the lock reintroduce the prune/adopt race")
}
