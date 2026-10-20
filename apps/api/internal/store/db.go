package store

import (
	"context"
	"embed"
	"fmt"
	"log/slog"
	"time"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/pgx/v5"
	"github.com/golang-migrate/migrate/v4/source/iofs"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/belune-hq/belune/internal/store/generated"
)

// migrationAdvisoryLockID is the Postgres advisory lock key acquired around
// the migration run. Picked as a deterministic constant so out-of-band tools
// (e.g. an operator running `migrate` manually) can use the same key to
// coordinate. golang-migrate's pgx driver also takes its own internal lock —
// this outer lock guards everything *around* m.Up() (e.g. future schema
// bootstrap steps) so they remain serialized too.
const migrationAdvisoryLockID int64 = 0x70616173_6d696772 // "belunemigr"

// migrationLockTimeout caps how long we'll wait for another instance's
// migration to finish on a multi-replica startup before giving up.
const migrationLockTimeout = 2 * time.Minute

// Connect creates a new PostgreSQL connection pool.
func Connect(databaseURL string) (*pgxpool.Pool, error) {
	config, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return nil, fmt.Errorf("parse database URL: %w", err)
	}

	pool, err := pgxpool.NewWithConfig(context.Background(), config)
	if err != nil {
		return nil, fmt.Errorf("create connection pool: %w", err)
	}

	if err := pool.Ping(context.Background()); err != nil {
		return nil, fmt.Errorf("ping database: %w", err)
	}

	return pool, nil
}

// RunMigrations runs all embedded SQL migrations against the database. A
// Postgres session-level advisory lock serialises concurrent invocations from
// multiple replicas: only one instance applies migrations; the others block
// until the lock is released, then no-op (m.Up returns ErrNoChange).
func RunMigrations(databaseURL string, migrationFS embed.FS) error {
	// Acquire the advisory lock on a *separate* connection so we control its
	// lifecycle independently of the migration driver's own pool. Hold for
	// the full migration; release on return.
	cfg, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return fmt.Errorf("parse database URL for migration lock: %w", err)
	}
	cfg.MaxConns = 1
	lockPool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		return fmt.Errorf("connect for migration lock: %w", err)
	}
	defer lockPool.Close()

	lockCtx, cancel := context.WithTimeout(context.Background(), migrationLockTimeout)
	defer cancel()

	conn, err := lockPool.Acquire(lockCtx)
	if err != nil {
		return fmt.Errorf("acquire migration lock connection: %w", err)
	}
	defer conn.Release()

	if _, err := conn.Exec(lockCtx, "SELECT pg_advisory_lock($1)", migrationAdvisoryLockID); err != nil {
		return fmt.Errorf("acquire migration advisory lock: %w", err)
	}
	defer func() {
		// Best-effort release on a fresh, short context so a cancelled
		// outer context still frees the lock.
		releaseCtx, releaseCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer releaseCancel()
		if _, err := conn.Exec(releaseCtx, "SELECT pg_advisory_unlock($1)", migrationAdvisoryLockID); err != nil {
			slog.Warn("migrations: failed to release advisory lock", "error", err)
		}
	}()

	src, err := iofs.New(migrationFS, ".")
	if err != nil {
		return fmt.Errorf("create migration source: %w", err)
	}

	m, err := migrate.NewWithSourceInstance("iofs", src, "pgx5://"+stripScheme(databaseURL))
	if err != nil {
		return fmt.Errorf("create migrate instance: %w", err)
	}

	if err := m.Up(); err != nil && err != migrate.ErrNoChange {
		return fmt.Errorf("run migrations: %w", err)
	}

	version, dirty, _ := m.Version()
	slog.Info("database migrations applied", "version", version, "dirty", dirty)
	return nil
}

// WithTx runs fn inside a transaction. Commits on success, rolls back on error.
func WithTx(ctx context.Context, pool *pgxpool.Pool, fn func(q *generated.Queries) error) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if err := fn(generated.New(tx)); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// stripScheme removes the "postgres://" or "postgresql://" scheme prefix
// so we can prepend "pgx5://" for the golang-migrate pgx driver.
func stripScheme(url string) string {
	for _, prefix := range []string{"postgresql://", "postgres://"} {
		if len(url) > len(prefix) && url[:len(prefix)] == prefix {
			return url[len(prefix):]
		}
	}
	return url
}
