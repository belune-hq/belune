package service

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/belune-hq/belune/internal/runtime"
	"github.com/belune-hq/belune/internal/store/generated"
)

// Placement is a property of the project, so any resource's host is one join
// away. These helpers are for paths that hold an id and nothing else; where a
// row already carries server_id — anything read through a query that joins
// projects — pass it to Runtimes.For directly and skip the round trip.

// RuntimeForApplication returns the runtime for the host an application runs on.
func RuntimeForApplication(ctx context.Context, q *generated.Queries, rts runtime.Runtimes, appID pgtype.UUID) (runtime.ContainerRuntime, error) {
	serverID, err := q.GetServerIDForApplication(ctx, appID)
	if err != nil {
		return nil, fmt.Errorf("resolving the server for application %s: %w", uuidToString(appID), err)
	}
	return rts.For(ctx, serverID)
}

// RuntimeForDatabase returns the runtime for the host a managed database runs on.
func RuntimeForDatabase(ctx context.Context, q *generated.Queries, rts runtime.Runtimes, dbID pgtype.UUID) (runtime.ContainerRuntime, error) {
	serverID, err := q.GetServerIDForDatabase(ctx, dbID)
	if err != nil {
		return nil, fmt.Errorf("resolving the server for database %s: %w", uuidToString(dbID), err)
	}
	return rts.For(ctx, serverID)
}

// RuntimeForProject returns the runtime for the host a project's resources run
// on. Prefer it over a per-resource lookup when acting on a whole project.
func RuntimeForProject(ctx context.Context, q *generated.Queries, rts runtime.Runtimes, projectID pgtype.UUID) (runtime.ContainerRuntime, error) {
	project, err := q.GetProject(ctx, projectID)
	if err != nil {
		return nil, fmt.Errorf("resolving the server for project %s: %w", uuidToString(projectID), err)
	}
	return rts.For(ctx, project.ServerID)
}
