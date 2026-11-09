package store_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/belune-hq/belune/internal/instances"
	"github.com/belune-hq/belune/internal/migrations"
	"github.com/belune-hq/belune/internal/store/generated"
	"github.com/belune-hq/belune/internal/testutil"
)

// 000081 moves a column DEFAULT, which governs future inserts only. The assertion that
// matters is the one someone would break by adding an UPDATE: an application that
// already exists as http + no path must come out of the migration unchanged, so it
// keeps the Processes-tab warning ("no path set, so nothing is checked") instead of
// being quietly relabelled into a state that looks configured on purpose.
//
// The test DB already sits at the latest migration, so the previous default is put
// back first and the migration's own SQL is then run over it: the shipped statement,
// not a copy of it.
func TestMigration000081_MovesTheDefaultForFutureRowsAndLeavesExistingOnesAlone(t *testing.T) {
	ctx := resetWAF(t)
	user, err := testQueries.CreateUser(ctx, generated.CreateUserParams{Email: "hd@test.com", PasswordHash: "h", Role: "admin"})
	require.NoError(t, err)
	project, err := testQueries.CreateProject(ctx, generated.CreateProjectParams{
		Name: "hd", Slug: "hd", UserID: user.ID, ServerID: testutil.LocalServerID(t, ctx, testQueries),
	})
	require.NoError(t, err)
	mk := func(slug string) generated.Application {
		app, err := testQueries.CreateApplication(ctx, generated.CreateApplicationParams{
			ProjectID: project.ID, Name: slug, Slug: slug, Type: "image", BuildType: "image",
		})
		require.NoError(t, err)
		return app
	}

	// Before: the 000054 default.
	_, err = testPool.Exec(ctx, `ALTER TABLE applications ALTER COLUMN health_check_type SET DEFAULT 'http'`)
	require.NoError(t, err)
	existing := mk("existing")
	require.Equal(t, "http", existing.HealthCheckType)
	require.False(t, existing.HealthCheckPath.Valid)

	sqlText, err := migrations.Files.ReadFile("000081_health_check_default_none.up.sql")
	require.NoError(t, err)
	_, err = testPool.Exec(ctx, string(sqlText))
	require.NoError(t, err)

	after, err := testQueries.GetApplication(ctx, existing.ID)
	require.NoError(t, err)
	assert.Equal(t, "http", after.HealthCheckType, "an existing row is NOT rewritten")
	assert.False(t, after.HealthCheckPath.Valid)
	assert.True(t, instances.ResolveHealth(after, generated.ApplicationProcessType{Routable: true}).Inert,
		"and it still reads as inert, so the warning keeps prompting")

	fresh := mk("fresh")
	assert.Equal(t, "none", fresh.HealthCheckType, "a new row defaults to none")
	assert.False(t, fresh.HealthCheckPath.Valid, "and gets no guessed path")
	assert.False(t, instances.ResolveHealth(fresh, generated.ApplicationProcessType{Routable: true}).Inert,
		"none is honest, not an inert http probe")
}
