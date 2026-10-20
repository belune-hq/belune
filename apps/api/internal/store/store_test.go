package store_test

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/belune-hq/belune/internal/store"
	"github.com/belune-hq/belune/internal/store/generated"
	"github.com/belune-hq/belune/internal/testutil"
)

var (
	testPool    *pgxpool.Pool
	testQueries *generated.Queries
)

func TestMain(m *testing.M) {
	pool, queries, teardown := testutil.SetupTestDB()
	testPool = pool
	testQueries = queries
	code := m.Run()
	teardown()
	os.Exit(code)
}

// TestWithTx_CommitsOnSuccess verifies that fn's writes are visible outside
// the transaction when fn returns nil.
func TestWithTx_CommitsOnSuccess(t *testing.T) {
	t.Cleanup(func() { _ = testutil.TruncateAll(context.Background(), testPool) })
	ctx := context.Background()

	err := store.WithTx(ctx, testPool, func(q *generated.Queries) error {
		_, err := q.CreateUser(ctx, generated.CreateUserParams{
			Email:        "tx-commit@test.com",
			PasswordHash: "hash",
			Role:         "user",
		})
		return err
	})
	require.NoError(t, err, "WithTx should commit when fn succeeds")

	user, err := testQueries.GetUserByEmail(ctx, "tx-commit@test.com")
	require.NoError(t, err, "committed row must be visible outside the transaction")
	assert.Equal(t, "tx-commit@test.com", user.Email)
	assert.Equal(t, "user", user.Role)
}

// TestWithTx_RollsBackOnError verifies that fn's writes are discarded when fn
// returns a non-nil error, and that the original error is propagated to the caller.
func TestWithTx_RollsBackOnError(t *testing.T) {
	t.Cleanup(func() { _ = testutil.TruncateAll(context.Background(), testPool) })
	ctx := context.Background()

	sentinel := errors.New("intentional rollback")
	err := store.WithTx(ctx, testPool, func(q *generated.Queries) error {
		if _, insertErr := q.CreateUser(ctx, generated.CreateUserParams{
			Email:        "tx-rollback@test.com",
			PasswordHash: "hash",
			Role:         "user",
		}); insertErr != nil {
			return insertErr
		}
		return sentinel
	})
	assert.ErrorIs(t, err, sentinel, "WithTx must propagate fn's error unchanged")

	_, err = testQueries.GetUserByEmail(ctx, "tx-rollback@test.com")
	assert.ErrorIs(t, err, pgx.ErrNoRows, "rolled-back row must not be visible")
}

// TestUserCRUD verifies the CreateUser / GetUserByID / GetUserByEmail round-trip
// to catch any schema–query mismatch in the generated sqlc code.
func TestUserCRUD(t *testing.T) {
	t.Cleanup(func() { _ = testutil.TruncateAll(context.Background(), testPool) })
	ctx := context.Background()

	created, err := testQueries.CreateUser(ctx, generated.CreateUserParams{
		Email:        "crud@test.com",
		PasswordHash: "bcrypt-hash",
		Role:         "admin",
		FirstName:    "Ada",
		LastName:     "Lovelace",
	})
	require.NoError(t, err)
	require.True(t, created.ID.Valid, "created user must have a non-nil ID")

	byID, err := testQueries.GetUserByID(ctx, created.ID)
	require.NoError(t, err, "GetUserByID must return the created user")
	assert.Equal(t, "crud@test.com", byID.Email)
	assert.Equal(t, "admin", byID.Role)
	assert.Equal(t, "Ada", byID.FirstName)
	assert.Equal(t, "Lovelace", byID.LastName)

	byEmail, err := testQueries.GetUserByEmail(ctx, "crud@test.com")
	require.NoError(t, err, "GetUserByEmail must return the created user")
	assert.Equal(t, created.ID, byEmail.ID)
}

// TestGetUserByEmail_NotFound verifies that querying for a non-existent email
// returns pgx.ErrNoRows rather than a generic error.
func TestGetUserByEmail_NotFound(t *testing.T) {
	ctx := context.Background()
	_, err := testQueries.GetUserByEmail(ctx, "no-such-user@test.com")
	assert.ErrorIs(t, err, pgx.ErrNoRows)
}
