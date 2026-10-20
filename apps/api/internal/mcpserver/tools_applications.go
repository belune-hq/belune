package mcpserver

import (
	"context"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/belune-hq/belune/internal/service"
	"github.com/belune-hq/belune/internal/store/generated"
)

// application is the tool-facing shape of an application row — like
// applicationResponse in internal/handler, it lists fields explicitly rather
// than embedding the generated row, which carries four secret-adjacent
// columns (webhook secret, encrypted git credentials, both deploy-hook token
// columns) with no reader value here.
type application struct {
	ID              string `json:"id"`
	ProjectID       string `json:"project_id"`
	Name            string `json:"name"`
	Slug            string `json:"slug"`
	Type            string `json:"type"`
	Status          string `json:"status"`
	BuildType       string `json:"build_type"`
	SourceRepo      string `json:"source_repo,omitempty"`
	SourceImage     string `json:"source_image,omitempty"`
	Branch          string `json:"branch,omitempty"`
	HealthCheckPath string `json:"health_check_path,omitempty"`
	// ContainerPort is the application's own port; omitted when unset, in which
	// case it resolves from the first domain and then 8080.
	ContainerPort int32 `json:"container_port,omitempty"`
	// PendingChange is service.ApplicationPendingChange, the same rule the
	// REST wire shape uses: "source" outranks "config" (a deploy applies
	// both), and it's suppressed before the application's first deploy so a
	// fresh app doesn't read "needs redeploy" from birth.
	PendingChange  string `json:"pending_change,omitempty"`
	LastDeployedAt string `json:"last_deployed_at,omitempty"`
	CreatedAt      string `json:"created_at"`
}

func toApplication(a generated.Application) application {
	out := application{
		ID:              uuidToString(a.ID),
		ProjectID:       uuidToString(a.ProjectID),
		Name:            a.Name,
		Slug:            a.Slug,
		Type:            a.Type,
		Status:          a.Status,
		BuildType:       a.BuildType,
		SourceRepo:      a.SourceRepo.String,
		SourceImage:     a.SourceImage.String,
		Branch:          a.Branch.String,
		HealthCheckPath: a.HealthCheckPath.String,
		ContainerPort:   a.ContainerPort.Int32,
		LastDeployedAt:  formatTimestamp(a.LastDeployedAt),
		CreatedAt:       formatTimestamp(a.CreatedAt),
		PendingChange:   service.ApplicationPendingChange(a),
	}
	return out
}

func registerApplicationTools(srv *mcp.Server, queries *generated.Queries) {
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "list_applications",
		Description: "List every application in a project.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in projectIDInput) (*mcp.CallToolResult, any, error) {
		projectID, err := parseUUID(in.ProjectID)
		if err != nil {
			return nil, nil, err
		}
		if _, err := authorizeProject(ctx, queries, projectID); err != nil {
			return nil, nil, err
		}

		rows, err := queries.ListApplicationsByProject(ctx, projectID)
		if err != nil {
			return nil, nil, internalError("failed to list applications", err)
		}
		out := make([]application, 0, len(rows))
		for _, a := range rows {
			out = append(out, toApplication(a))
		}
		return textResult(out)
	})

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "get_application",
		Description: "Get one application by id: its source, build settings, status, and whether it has an unapplied source or config change pending.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in applicationIDInput) (*mcp.CallToolResult, any, error) {
		id, err := parseUUID(in.ApplicationID)
		if err != nil {
			return nil, nil, err
		}
		app, err := queries.GetApplication(ctx, id)
		if err != nil {
			return nil, nil, notFoundOr(ctx, "application not found")
		}
		if err := authorizeApplication(ctx, queries, app.ID, app.ProjectID); err != nil {
			return nil, nil, err
		}
		return textResult(toApplication(app))
	})
}
