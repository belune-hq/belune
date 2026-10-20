package service_test

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/belune-hq/belune/internal/service"
	"github.com/belune-hq/belune/internal/store/generated"
	"github.com/belune-hq/belune/internal/testutil"
)

// TestTokenCreate_PartialPinFailureRollsBackTheToken pins the transactional
// integrity Create depends on: the token row and its pin rows are written in
// one transaction (store.WithTx), so a pin insert that fails (here: a project
// id that doesn't exist, violating api_token_projects' foreign key) must
// leave NO token row behind either. A token that survived without its pins
// would be pinned=true with zero rows — reaching nothing, which happens to be
// safe — but the property under test is Create's atomicity itself: both
// writes commit together, or neither does. A future edit that split them back
// into two separate statements would leak a token row here even though this
// exact failure mode stays harmless.
func TestTokenCreate_PartialPinFailureRollsBackTheToken(t *testing.T) {
	t.Cleanup(func() { truncate(t) })
	ctx := context.Background()
	user, _ := seedUserAndProject(t)

	svc := service.NewTokenService(testPool, testQueries)

	var nonexistent pgtype.UUID
	require.NoError(t, nonexistent.Scan("00000000-0000-0000-0000-000000000000"))

	_, err := svc.Create(ctx, service.CreateTokenParams{
		UserID:      user.ID,
		Name:        "doomed",
		RoleAtIssue: "admin",
		Scopes:      []string{"read"},
		Pinned:      true,
		ProjectIDs:  []pgtype.UUID{nonexistent},
	})
	require.Error(t, err, "a pin insert violating the foreign key must fail Create")

	var count int
	require.NoError(t, testPool.QueryRow(ctx, "SELECT count(*) FROM api_tokens WHERE name = 'doomed'").Scan(&count))
	assert.Equal(t, 0, count, "the token row must not survive a failed pin insert — both writes commit together or neither does")
}

// TestTokenCreate_MultiPinPersistsExactSet pins the ordinary multi-project
// path: every requested id lands in api_token_projects, and Authenticate
// reads the same set back via GetAPITokenByHash's array_agg.
func TestTokenCreate_MultiPinPersistsExactSet(t *testing.T) {
	t.Cleanup(func() { truncate(t) })
	ctx := context.Background()
	user, projectA := seedUserAndProject(t)
	projectB, err := testQueries.CreateProject(ctx, generated.CreateProjectParams{
		Name:     "Second Project",
		Slug:     "proj-b-" + randomSuffix(t),
		UserID:   user.ID,
		ServerID: testutil.LocalServerID(t, ctx, testQueries),
	})
	require.NoError(t, err)

	svc := service.NewTokenService(testPool, testQueries)

	created, err := svc.Create(ctx, service.CreateTokenParams{
		UserID:      user.ID,
		Name:        "multi-pinned",
		RoleAtIssue: "admin",
		Scopes:      []string{"read"},
		Pinned:      true,
		ProjectIDs:  []pgtype.UUID{projectA.ID, projectB.ID},
	})
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{uuidString(projectA.ID), uuidString(projectB.ID)}, created.ProjectIDs)

	authed, err := svc.Authenticate(ctx, created.Plain)
	require.NoError(t, err)
	assert.True(t, authed.Pinned)
	assert.ElementsMatch(t, []string{uuidString(projectA.ID), uuidString(projectB.ID)}, authed.ProjectIDs)
}
