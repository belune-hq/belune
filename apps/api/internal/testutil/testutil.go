package testutil

import (
	"context"
	"fmt"
	"log"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/weiliang79/belune/internal/migrations"
	"github.com/weiliang79/belune/internal/store/generated"
)

// SetupTestDB spins up a PostgreSQL 16 testcontainer, runs migrations,
// and returns a connection pool, queries, and a teardown function.
// Uses log.Fatal instead of t.Fatalf since this is called from TestMain.
func SetupTestDB() (*pgxpool.Pool, *generated.Queries, func()) {
	ctx := context.Background()

	pgContainer, err := postgres.Run(ctx,
		"postgres:16-alpine",
		postgres.WithDatabase("belune_test"),
		postgres.WithUsername("test"),
		postgres.WithPassword("test"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).
				WithStartupTimeout(30*time.Second),
		),
	)
	if err != nil {
		log.Fatalf("failed to start postgres container: %v", err)
	}

	connStr, err := pgContainer.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		log.Fatalf("failed to get connection string: %v", err)
	}

	pool, err := pgxpool.New(ctx, connStr)
	if err != nil {
		log.Fatalf("failed to create pool: %v", err)
	}

	runMigrations(pool)

	queries := generated.New(pool)

	teardown := func() {
		pool.Close()
		if err := pgContainer.Terminate(ctx); err != nil {
			log.Printf("failed to terminate postgres container: %v", err)
		}
	}

	return pool, queries, teardown
}

func runMigrations(pool *pgxpool.Pool) {
	ctx := context.Background()

	entries, err := migrations.Files.ReadDir(".")
	if err != nil {
		log.Fatalf("failed to read migration files: %v", err)
	}

	// Sort to ensure order
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].Name() < entries[j].Name()
	})

	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		// Only run up migrations
		if !strings.HasSuffix(entry.Name(), ".up.sql") {
			continue
		}
		content, err := migrations.Files.ReadFile(entry.Name())
		if err != nil {
			log.Fatalf("failed to read migration %s: %v", entry.Name(), err)
		}
		if _, err := pool.Exec(ctx, string(content)); err != nil {
			log.Fatalf("failed to run migration %s: %v", entry.Name(), err)
		}
		fmt.Printf("  migration: %s ✓\n", entry.Name())
	}
}

// TruncateAll truncates all tables for test isolation.
//
// ⚠️ quotas is listed EXPLICITLY and must stay listed: quotas.scope_id is
// polymorphic (a user id or a project id, no foreign key), so CASCADE from
// users or projects cannot reach it. Omitted, a quota written by one test
// outlived it as an orphan row and every later test in the package ran against
// a cap it never set — which is how a test asserting the full quota list saw
// two extra rows and failed only when run after its neighbours.
func TruncateAll(ctx context.Context, pool *pgxpool.Pool) error {
	_, err := pool.Exec(ctx, `
		TRUNCATE users, projects, applications, deployments, databases, domains, env_vars, container_logs, request_logs, git_integrations, git_provider_configs, domain_route_features, audit_logs, refresh_tokens, login_attempts, password_reset_tokens, invitations, alert_preferences, quotas, quota_alert_state, backup_runs CASCADE
	`)
	return err
}
