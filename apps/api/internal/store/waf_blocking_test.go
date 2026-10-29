package store_test

import (
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/belune-hq/belune/internal/store/generated"
	"github.com/belune-hq/belune/internal/testutil"
)

// A hostname can be split across applications by path and the WAF sees the whole
// host, so it blocks only when EVERY application serving it opted in.
func TestListWAFBlockingHostnames_AHostBlocksOnlyWhenEveryApplicationOptedIn(t *testing.T) {
	ctx := resetWAF(t)
	user, err := testQueries.CreateUser(ctx, generated.CreateUserParams{Email: "wafb@test.com", PasswordHash: "h", Role: "admin"})
	require.NoError(t, err)
	proj, err := testQueries.CreateProject(ctx, generated.CreateProjectParams{
		Name: "p", Slug: "p", UserID: user.ID, ServerID: testutil.LocalServerID(t, ctx, testQueries),
	})
	require.NoError(t, err)
	mkApp := func(slug string, blocking bool) pgtype.UUID {
		app, err := testQueries.CreateApplication(ctx, generated.CreateApplicationParams{
			ProjectID: proj.ID, Name: slug, Slug: slug, Type: "image", BuildType: "image",
		})
		require.NoError(t, err)
		assert.False(t, app.WafBlocking, "an application is detection-only until it opts in")
		_, err = testPool.Exec(ctx, `UPDATE applications SET waf_blocking = $2 WHERE id = $1`, app.ID, blocking)
		require.NoError(t, err)
		return app.ID
	}
	mkDomain := func(app pgtype.UUID, host, path string) {
		_, err := testQueries.CreateDomain(ctx, generated.CreateDomainParams{
			ApplicationID: app, Hostname: host, Path: path, SslMode: "off",
			ContainerPort: pgtype.Int4{Int32: 8080, Valid: true},
		})
		require.NoError(t, err)
	}

	on, off, alsoOn := mkApp("on", true), mkApp("off", false), mkApp("also-on", true)
	mkDomain(on, "blocked.test", "/")
	mkDomain(off, "detect.test", "/")
	mkDomain(on, "split.test", "/")
	mkDomain(off, "split.test", "/api") // shares the host with an app that did not opt in
	mkDomain(on, "both.test", "/")
	mkDomain(alsoOn, "both.test", "/api")

	got, err := testQueries.ListWAFBlockingHostnames(ctx)
	require.NoError(t, err)
	assert.Equal(t, []string{"blocked.test", "both.test"}, got)
}
