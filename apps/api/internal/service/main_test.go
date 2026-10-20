package service_test

import (
	"context"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/belune-hq/belune/internal/pkg/crypto"
	"github.com/belune-hq/belune/internal/store/generated"
	"github.com/belune-hq/belune/internal/testutil"
)

var (
	testPool    *pgxpool.Pool
	testQueries *generated.Queries
	testKeyring *crypto.Keyring
)

func TestMain(m *testing.M) {
	pool, queries, teardown := testutil.SetupTestDB()
	testPool = pool
	testQueries = queries

	keyring, err := crypto.ParseKeyringEnv("", testutil.TestEncryptionKey, "")
	if err != nil {
		teardown()
		panic("setup test keyring: " + err.Error())
	}
	testKeyring = keyring

	code := m.Run()
	teardown()
	os.Exit(code)
}

// seedUserAndProject inserts a fresh user + project into the test DB and
// returns their rows. Each call uses a unique slug suffix so multiple
// fixtures within the same test do not collide.
func seedUserAndProject(t *testing.T) (generated.User, generated.Project) {
	t.Helper()
	ctx := context.Background()

	suffix := randomSuffix(t)

	user, err := testQueries.CreateUser(ctx, generated.CreateUserParams{
		Email:        "svc-" + suffix + "@test.com",
		PasswordHash: "x",
		Role:         "admin",
	})
	require.NoError(t, err)

	project, err := testQueries.CreateProject(ctx, generated.CreateProjectParams{
		Name:     "Test Project",
		Slug:     "proj-" + suffix,
		UserID:   user.ID,
		ServerID: testutil.LocalServerID(t, ctx, testQueries),
	})
	require.NoError(t, err)

	return user, project
}

func randomSuffix(t *testing.T) string {
	t.Helper()
	tok, err := crypto.GenerateWebhookSecret()
	require.NoError(t, err)
	return tok[:8]
}

// truncate clears all tables; call from t.Cleanup so each test runs against a
// fresh DB without paying for a new container.
func truncate(t *testing.T) {
	t.Helper()
	require.NoError(t, testutil.TruncateAll(context.Background(), testPool))
}

// uuidString renders a pgtype.UUID as the canonical 8-4-4-4-12 hex form.
// Service code uses the same shape for asynq task IDs and slug suffixes, so
// tests need to compute it without depending on unexported helpers.
func uuidString(u pgtype.UUID) string {
	b := u.Bytes
	const hex = "0123456789abcdef"
	out := make([]byte, 36)
	idx := 0
	for i, by := range b {
		if i == 4 || i == 6 || i == 8 || i == 10 {
			out[idx] = '-'
			idx++
		}
		out[idx] = hex[by>>4]
		out[idx+1] = hex[by&0x0f]
		idx += 2
	}
	return string(out)
}
