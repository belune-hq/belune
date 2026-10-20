package mcpserver

import (
	"context"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/belune-hq/belune/internal/server/middleware"
	"github.com/belune-hq/belune/internal/store/generated"
)

const (
	defaultProjectsLimit = 50
	maxProjectsLimit     = 200
)

// project is the tool-facing shape of a project row — narrower than the
// generated row, which carries server_id and other fields with no reader
// value here.
type project struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Slug      string `json:"slug"`
	Shared    bool   `json:"shared"`
	CreatedAt string `json:"created_at"`
}

type listProjectsInput struct {
	Limit int `json:"limit,omitempty" jsonschema:"maximum number of projects to return, newest first (default 50, max 200)"`
}

func registerProjectTools(srv *mcp.Server, queries *generated.Queries) {
	mcp.AddTool(srv, &mcp.Tool{
		Name: "list_projects",
		Description: "List every project the caller's token can reach: every project on the " +
			"install for an admin token, or the token owner's own projects plus any shared with " +
			"them otherwise. A project-pinned token sees only its pinned projects. Bounded — " +
			"defaults to 50, capped at 200.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in listProjectsInput) (*mcp.CallToolResult, any, error) {
		limit := clampLimit(in.Limit, defaultProjectsLimit, maxProjectsLimit)
		projects, err := listProjectsForCaller(ctx, queries, limit)
		if err != nil {
			return nil, nil, err
		}
		return textResult(projects)
	})

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "get_project",
		Description: "Get one project by id, including whether it is shared with other Members.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in projectIDInput) (*mcp.CallToolResult, any, error) {
		id, err := parseUUID(in.ProjectID)
		if err != nil {
			return nil, nil, err
		}
		p, err := authorizeProject(ctx, queries, id)
		if err != nil {
			return nil, nil, err
		}
		return textResult(toProject(p.ID, p.Name, p.Slug, p.Shared, p.CreatedAt))
	})
}

func toProject(id pgtype.UUID, name, slug string, shared bool, createdAt pgtype.Timestamptz) project {
	return project{
		ID:        uuidToString(id),
		Name:      name,
		Slug:      slug,
		Shared:    shared,
		CreatedAt: formatTimestamp(createdAt),
	}
}

// listProjectsForCaller mirrors handler.Handler.ListProjects: an admin sees
// every project on the install, a member sees their own and any shared with
// them, and a project-pinned token is narrowed to its pin set. The rule is
// reimplemented here, not called into the handler package, because a tool
// call has no http.ResponseWriter to hand a handler method — and because
// internal/handler will need to import this package to wire the route,
// so the reverse import isn't available.
//
// The pin is pushed into SQL (via pinnedProjectUUIDs/sqlc.narg), not applied
// as a post-query Go-side filter: doing it after LIMIT already truncated the
// row set would risk silently dropping a project a pinned token is entitled
// to see, if it isn't among the `limit` most-recently-created rows.
func listProjectsForCaller(ctx context.Context, queries *generated.Queries, limit int) ([]project, error) {
	pinnedIDs, err := pinnedProjectUUIDs(ctx)
	if err != nil {
		return nil, internalError("failed to list projects", err)
	}

	role := middleware.RoleFromContext(ctx)
	if role == "admin" {
		rows, err := queries.ListAllProjectsLimit(ctx, generated.ListAllProjectsLimitParams{
			ProjectIds: pinnedIDs,
			RowLimit:   int32(limit),
		})
		if err != nil {
			return nil, internalError("failed to list projects", err)
		}
		return mapProjects(rows, func(p generated.ListAllProjectsLimitRow) (pgtype.UUID, string, string, bool, pgtype.Timestamptz) {
			return p.ID, p.Name, p.Slug, p.Shared, p.CreatedAt
		}), nil
	}

	var userID pgtype.UUID
	if err := userID.Scan(middleware.UserIDFromContext(ctx)); err != nil {
		return nil, internalError("failed to list projects", err)
	}

	rows, err := queries.ListProjectsByUserLimit(ctx, generated.ListProjectsByUserLimitParams{
		UserID:     userID,
		ProjectIds: pinnedIDs,
		RowLimit:   int32(limit),
	})
	if err != nil {
		return nil, internalError("failed to list projects", err)
	}
	return mapProjects(rows, func(p generated.ListProjectsByUserLimitRow) (pgtype.UUID, string, string, bool, pgtype.Timestamptz) {
		return p.ID, p.Name, p.Slug, p.Shared, p.CreatedAt
	}), nil
}

// mapProjects converts a slice of rows to the tool-facing shape. Always
// returns a non-nil slice so the tool's JSON output is "[]", never "null",
// when nothing matches.
func mapProjects[T any](rows []T, fields func(T) (pgtype.UUID, string, string, bool, pgtype.Timestamptz)) []project {
	out := make([]project, 0, len(rows))
	for _, r := range rows {
		id, name, slug, shared, createdAt := fields(r)
		out = append(out, toProject(id, name, slug, shared, createdAt))
	}
	return out
}
