// Package instances is the one place that turns an application into the
// container(s) that belong to it.
//
// It exists so that no call site ever DERIVES a container name again. The name is
// a fact stored on application_instances.container_name -- legacy rows keep the
// old one (applications.slug), so nothing is renamed -- and anything that needs it
// asks here. It is a package of its own, taking only the generated queries, so the
// handler, service, worker, ws and mcpserver packages can all import it without a
// cycle.
//
// ⛔ NEVER parse a container name. It is an opaque handle on the row; anything
// that needs structure (which type, which index) reads labels.
package instances

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/belune-hq/belune/internal/store/generated"
)

// ErrNoInstance means the application has no instance row to talk to. A1's
// backfill and the creation path make this impossible, and "impossible" is how
// silent 502s ship, so it is an explicit, logged error that callers surface.
//
// ⛔ Do NOT recover from it by falling back to naming.ContainerName: that would
// hide exactly the missing-row bug this release is about, and make the reconciler
// look correct on an install whose rows are missing.
var ErrNoInstance = errors.New("application has no container instance")

// Querier is the slice of generated.Queries the resolver reads.
type Querier interface {
	ListInstancesByApplication(ctx context.Context, applicationID pgtype.UUID) ([]generated.ApplicationInstance, error)
	ListRoutableInstancesByApplication(ctx context.Context, applicationID pgtype.UUID) ([]generated.ApplicationInstance, error)
}

// ResolveOne returns the single container to talk to: the lowest idx among the
// RUNNING instances of the routable type, so 0.3.1 needs no change here (with one
// replica it is just "the only one").
//
// If none is recorded as running it returns the lowest idx of the routable type
// instead. Instance status is not driven until A4 -- a new application's row sits
// at 'pending' while the container runs -- and logs, exec and metrics must keep
// working against a stopped or crashed container, as they did when the name was
// derived. The caller's Docker call reports the real state.
func ResolveOne(ctx context.Context, q Querier, appID pgtype.UUID) (generated.ApplicationInstance, error) {
	rows, err := q.ListRoutableInstancesByApplication(ctx, appID)
	if err != nil {
		return generated.ApplicationInstance{}, fmt.Errorf("listing routable instances: %w", err)
	}
	if len(rows) == 0 {
		slog.Error("instance resolver: application has no routable instance row", "application_id", uuidString(appID))
		return generated.ApplicationInstance{}, ErrNoInstance
	}
	// rows are ORDER BY idx in SQL; do not re-sort here.
	for _, r := range rows {
		if r.Status == "running" {
			return r, nil
		}
	}
	return rows[0], nil
}

// ResolveAll returns every instance of the application, ordered by idx (the order
// comes from the query; it is not re-sorted here).
func ResolveAll(ctx context.Context, q Querier, appID pgtype.UUID) ([]generated.ApplicationInstance, error) {
	rows, err := q.ListInstancesByApplication(ctx, appID)
	if err != nil {
		return nil, fmt.Errorf("listing instances: %w", err)
	}
	if len(rows) == 0 {
		slog.Error("instance resolver: application has no instance rows", "application_id", uuidString(appID))
		return nil, ErrNoInstance
	}
	return rows, nil
}

func uuidString(u pgtype.UUID) string {
	if !u.Valid {
		return ""
	}
	b := u.Bytes
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
