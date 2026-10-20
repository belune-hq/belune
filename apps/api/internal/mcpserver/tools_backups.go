package mcpserver

import (
	"context"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/belune-hq/belune/internal/store/generated"
)

const (
	defaultBackupsLimit = 20
	maxBackupsLimit     = 100
)

// backupRun is the tool-facing shape of one row of a project's backup
// activity feed — database backups and application volume backups
// interleaved, newest first. Omits Log: a backup command's output has no
// bound on this row, the same reason build_logs is left off deployment.
type backupRun struct {
	ID           string `json:"id"`
	Kind         string `json:"kind"` // "database" | "volume"
	ResourceID   string `json:"resource_id"`
	ResourceName string `json:"resource_name"`
	// AppName is set only for a "volume" row — the owning application's name.
	AppName    string `json:"app_name,omitempty"`
	Status     string `json:"status"`
	SizeBytes  int64  `json:"size_bytes"`
	HasRemote  bool   `json:"has_remote"`
	StartedAt  string `json:"started_at,omitempty"`
	FinishedAt string `json:"finished_at,omitempty"`
	Error      string `json:"error,omitempty"`
}

type listProjectBackupsInput struct {
	ProjectID string `json:"project_id" jsonschema:"the project's id"`
	Limit     int    `json:"limit,omitempty" jsonschema:"maximum number of backup runs to return, newest first (default 20, max 100)"`
}

func registerBackupTools(srv *mcp.Server, queries *generated.Queries) {
	mcp.AddTool(srv, &mcp.Tool{
		Name: "list_project_backups",
		Description: "List recent backup runs across a project's databases and application " +
			"volumes, newest first. Bounded — defaults to 20, capped at 100.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in listProjectBackupsInput) (*mcp.CallToolResult, any, error) {
		projectID, err := parseUUID(in.ProjectID)
		if err != nil {
			return nil, nil, err
		}
		if _, err := authorizeProject(ctx, queries, projectID); err != nil {
			return nil, nil, err
		}

		limit := clampLimit(in.Limit, defaultBackupsLimit, maxBackupsLimit)

		rows, err := queries.ListProjectBackupActivity(ctx, generated.ListProjectBackupActivityParams{
			ProjectID: projectID,
			Limit:     int32(limit),
		})
		if err != nil {
			return nil, nil, internalError("failed to list backups", err)
		}

		out := make([]backupRun, 0, len(rows))
		for _, b := range rows {
			out = append(out, backupRun{
				ID:           uuidToString(b.ID),
				Kind:         b.Kind,
				ResourceID:   uuidToString(b.ResourceID),
				ResourceName: b.ResourceName,
				AppName:      b.AppName.String,
				Status:       b.Status,
				SizeBytes:    b.SizeBytes,
				HasRemote:    b.RemoteKey.Valid,
				StartedAt:    formatTimestamp(b.StartedAt),
				FinishedAt:   formatTimestamp(b.FinishedAt),
				Error:        b.Error.String,
			})
		}
		return textResult(out)
	})
}
