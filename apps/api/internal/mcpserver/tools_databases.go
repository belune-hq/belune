package mcpserver

import (
	"context"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/belune-hq/belune/internal/store/generated"
)

// database is the tool-facing shape of a database row — omits
// credentials_encrypted (ciphertext an MCP client has no use for and no
// reason to be handed) and the backup/restore command strings (internal
// plumbing, not something a caller needs to inspect a database).
type database struct {
	ID           string `json:"id"`
	ProjectID    string `json:"project_id"`
	Type         string `json:"type"`
	Name         string `json:"name"`
	Slug         string `json:"slug"`
	Version      string `json:"version"`
	Status       string `json:"status"`
	Image        string `json:"image,omitempty"`
	InternalHost string `json:"internal_host,omitempty"`
	InternalPort int32  `json:"internal_port,omitempty"`
	CreatedAt    string `json:"created_at"`
}

func toDatabase(d generated.Database) database {
	return database{
		ID:           uuidToString(d.ID),
		ProjectID:    uuidToString(d.ProjectID),
		Type:         d.Type,
		Name:         d.Name,
		Slug:         d.Slug,
		Version:      d.Version,
		Status:       d.Status,
		Image:        d.Image.String,
		InternalHost: d.InternalHost.String,
		InternalPort: d.InternalPort.Int32,
		CreatedAt:    formatTimestamp(d.CreatedAt),
	}
}

func registerDatabaseTools(srv *mcp.Server, queries *generated.Queries) {
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "list_databases",
		Description: "List every managed database in a project.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in projectIDInput) (*mcp.CallToolResult, any, error) {
		projectID, err := parseUUID(in.ProjectID)
		if err != nil {
			return nil, nil, err
		}
		if _, err := authorizeProject(ctx, queries, projectID); err != nil {
			return nil, nil, err
		}

		rows, err := queries.ListDatabasesByProject(ctx, projectID)
		if err != nil {
			return nil, nil, internalError("failed to list databases", err)
		}
		out := make([]database, 0, len(rows))
		for _, d := range rows {
			out = append(out, toDatabase(d))
		}
		return textResult(out)
	})

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "get_database",
		Description: "Get one managed database by id. Never includes connection credentials.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in databaseIDInput) (*mcp.CallToolResult, any, error) {
		id, err := parseUUID(in.DatabaseID)
		if err != nil {
			return nil, nil, err
		}
		db, err := queries.GetDatabase(ctx, id)
		if err != nil {
			return nil, nil, notFoundOr(ctx, "database not found")
		}
		if err := authorizeDatabase(ctx, queries, db.ID, db.ProjectID); err != nil {
			return nil, nil, err
		}
		return textResult(toDatabase(db))
	})
}
