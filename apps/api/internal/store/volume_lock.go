package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/belune-hq/belune/internal/store/generated"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Adoption and pruning share this transaction lock so a prune snapshot cannot
// predate a new preservation claim. Commit, rollback and disconnect release it.
const volumeClaimsLockID int64 = 0x42454c564f4c

var ErrVolumeClaimsBusy = errors.New("a cleanup or volume operation is in progress; try again")

func WithVolumeClaimsLock(ctx context.Context, pool *pgxpool.Pool, fn func(context.Context, *generated.Queries) error) error {
	if pool == nil {
		return errors.New("volume claims lock requires a database")
	}
	opCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	tx, err := pool.Begin(opCtx)
	if err != nil {
		return fmt.Errorf("begin volume claims transaction: %w", err)
	}
	defer func() {
		releaseCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = tx.Rollback(releaseCtx)
	}()
	q := generated.New(tx)
	lockCtx, lockCancel := context.WithTimeout(opCtx, 3*time.Second)
	err = q.AcquireVolumeClaimsLock(lockCtx, volumeClaimsLockID)
	lockCancel()
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(lockCtx.Err(), context.DeadlineExceeded) {
			return ErrVolumeClaimsBusy
		}
		return fmt.Errorf("acquire volume claims lock: %w", err)
	}
	if err := fn(opCtx, q); err != nil {
		return err
	}
	return tx.Commit(opCtx)
}
