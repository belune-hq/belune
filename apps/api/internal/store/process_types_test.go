package store_test

import (
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/belune-hq/belune/internal/migrations"
	"github.com/belune-hq/belune/internal/store/generated"
	"github.com/belune-hq/belune/internal/testutil"
)

// The backfill is what makes Phase A invisible: every existing application must
// come out of the migration with one routable 'web' type and one instance named by
// its LEGACY container name, or the reconciler (A3) has no upstream for it and
// that app 502s on somebody's install.
//
// The test DB already sits at the latest migration, and nothing yet creates
// process-type rows on application create, so applications seeded here look
// exactly like pre-upgrade ones. Running the migration's own backfill statements
// over them tests the shipped SQL rather than a copy of it.
func TestMigration000079_BackfillCoversEveryApplication(t *testing.T) {
	ctx := resetWAF(t)
	user, err := testQueries.CreateUser(ctx, generated.CreateUserParams{Email: "bf@test.com", PasswordHash: "h", Role: "admin"})
	require.NoError(t, err)
	serverID := testutil.LocalServerID(t, ctx, testQueries)
	mkProject := func(slug string) pgtype.UUID {
		p, err := testQueries.CreateProject(ctx, generated.CreateProjectParams{Name: slug, Slug: slug, UserID: user.ID, ServerID: serverID})
		require.NoError(t, err)
		return p.ID
	}
	projA, projB := mkProject("proj-a"), mkProject("proj-b")

	type seed struct {
		project pgtype.UUID
		slug    string
		status  string
		deploy  bool
		want    string // expected instance status
	}
	seeds := []seed{
		{projA, "proj-a-api-11111111", "running", true, "running"},
		{projA, "proj-a-stopped-22222222", "stopped", true, "stopped"},
		{projA, "proj-a-fresh-33333333", "inactive", false, "pending"}, // never deployed
		{projA, "proj-a-sick-44444444", "unhealthy", true, "unhealthy"},
		{projA, "proj-a-broken-55555555", "error", false, "error"},
		{projB, "proj-b-api-66666666", "running", true, "running"}, // second project
	}
	ids := map[string]pgtype.UUID{}
	for _, s := range seeds {
		app, err := testQueries.CreateApplication(ctx, generated.CreateApplicationParams{
			ProjectID: s.project, Name: s.slug, Slug: s.slug, Type: "image", BuildType: "image",
		})
		require.NoError(t, err)
		_, err = testPool.Exec(ctx, `UPDATE applications SET status = $2 WHERE id = $1`, app.ID, s.status)
		require.NoError(t, err)
		if s.deploy {
			_, err = testQueries.CreateDeployment(ctx, generated.CreateDeploymentParams{
				ApplicationID: app.ID, Status: "success", TriggeredBy: "manual",
			})
			require.NoError(t, err)
		}
		ids[s.slug] = app.ID
	}

	sqlText, err := migrations.Files.ReadFile("000079_process_types_and_instances.up.sql")
	require.NoError(t, err)
	_, backfill, found := strings.Cut(string(sqlText), "INSERT INTO application_process_types")
	require.True(t, found, "migration lost its backfill")
	_, err = testPool.Exec(ctx, "INSERT INTO application_process_types"+backfill)
	require.NoError(t, err)

	for _, s := range seeds {
		types, err := testQueries.ListProcessTypesByApplication(ctx, ids[s.slug])
		require.NoError(t, err)
		require.Len(t, types, 1, s.slug)
		assert.Equal(t, "web", types[0].Name)
		assert.Equal(t, "", types[0].Command, "'' is the image's default CMD, i.e. today's container")
		assert.True(t, types[0].Routable)
		assert.EqualValues(t, 1, types[0].Replicas)

		inst, err := testQueries.ListInstancesByApplication(ctx, ids[s.slug])
		require.NoError(t, err)
		require.Len(t, inst, 1, s.slug)
		assert.Equal(t, s.slug, inst[0].ContainerName, "the legacy name is the application slug, never rebuilt")
		assert.EqualValues(t, 1, inst[0].Idx)
		assert.Equal(t, s.want, inst[0].Status)
		assert.False(t, inst[0].DeploymentID.Valid, "backfilled rows have no deployment")
		assert.Equal(t, types[0].ID, inst[0].ProcessTypeID)

		routable, err := testQueries.ListRoutableInstancesByApplication(ctx, ids[s.slug])
		require.NoError(t, err)
		require.Len(t, routable, 1)
		assert.Equal(t, inst[0].ID, routable[0].ID)
	}

	var apps, types, instances int
	require.NoError(t, testPool.QueryRow(ctx, `SELECT count(*) FROM applications`).Scan(&apps))
	require.NoError(t, testPool.QueryRow(ctx, `SELECT count(*) FROM application_process_types`).Scan(&types))
	require.NoError(t, testPool.QueryRow(ctx, `SELECT count(*) FROM application_instances`).Scan(&instances))
	assert.Equal(t, apps, types)
	assert.Equal(t, apps, instances)
}

// At most one routable type per application is a database guarantee, and the name
// pattern is too -- neither may depend on a service-layer check somebody skips.
func TestProcessTypes_ConstraintsHoldAtTheDatabase(t *testing.T) {
	ctx := resetWAF(t)
	user, err := testQueries.CreateUser(ctx, generated.CreateUserParams{Email: "pt@test.com", PasswordHash: "h", Role: "admin"})
	require.NoError(t, err)
	proj, err := testQueries.CreateProject(ctx, generated.CreateProjectParams{
		Name: "p", Slug: "p", UserID: user.ID, ServerID: testutil.LocalServerID(t, ctx, testQueries),
	})
	require.NoError(t, err)
	app, err := testQueries.CreateApplication(ctx, generated.CreateApplicationParams{
		ProjectID: proj.ID, Name: "a", Slug: "a", Type: "image", BuildType: "image",
	})
	require.NoError(t, err)
	mk := func(name string, routable bool) error {
		_, err := testQueries.CreateProcessType(ctx, generated.CreateProcessTypeParams{
			ApplicationID: app.ID, Name: name, Replicas: 1, Routable: routable,
		})
		return err
	}

	require.NoError(t, mk("web", true))
	assert.Error(t, mk("frontend", true), "a second routable type")
	require.NoError(t, mk("worker", false))
	assert.Error(t, mk("worker", false), "a duplicate name")
	for _, bad := range []string{"", "Web", "1web", "has_underscore", strings.Repeat("a", 21)} {
		assert.Error(t, mk(bad, false), "name %q", bad)
	}
	assert.NoError(t, mk(strings.Repeat("a", 20), false))

	// ListInstances* order by idx in SQL, not in the caller.
	web, err := testQueries.GetProcessTypeByName(ctx, generated.GetProcessTypeByNameParams{ApplicationID: app.ID, Name: "web"})
	require.NoError(t, err)
	for _, idx := range []int32{3, 1, 2} {
		_, err := testQueries.CreateInstance(ctx, generated.CreateInstanceParams{
			ApplicationID: app.ID, ProcessTypeID: web.ID, Idx: idx, ContainerName: "c", Status: "pending",
		})
		require.NoError(t, err)
	}
	got, err := testQueries.ListRoutableInstancesByApplication(ctx, app.ID)
	require.NoError(t, err)
	var order []int32
	for _, i := range got {
		order = append(order, i.Idx)
	}
	assert.Equal(t, []int32{1, 2, 3}, order)
}
