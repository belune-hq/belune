package mcpserver

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/belune-hq/belune/internal/store/generated"
)

const (
	defaultDeploymentsLimit = 20
	maxDeploymentsLimit     = 100
)

// deployment is the tool-facing shape of a deployment row. build_logs is
// deliberately omitted — like a container log tail, a build log has no
// bound on this row, so it is served by get_deployment_logs, which applies
// one, rather than tagging along on every list call.
type deployment struct {
	ID              string `json:"id"`
	Status          string `json:"status"`
	TriggeredBy     string `json:"triggered_by"`
	CommitSHA       string `json:"commit_sha,omitempty"`
	CommitMessage   string `json:"commit_message,omitempty"`
	CommitAuthor    string `json:"commit_author,omitempty"`
	ImageTag        string `json:"image_tag,omitempty"`
	ErrorMessage    string `json:"error_message,omitempty"`
	HealthStatus    string `json:"health_status,omitempty"`
	HealthMessage   string `json:"health_message,omitempty"`
	StartedAt       string `json:"started_at,omitempty"`
	BuildStartedAt  string `json:"build_started_at,omitempty"`
	BuildEndedAt    string `json:"build_ended_at,omitempty"`
	DeployStartedAt string `json:"deploy_started_at,omitempty"`
	FinishedAt      string `json:"finished_at,omitempty"`
}

func toDeployment(d generated.Deployment) deployment {
	return deployment{
		ID:              uuidToString(d.ID),
		Status:          d.Status,
		TriggeredBy:     d.TriggeredBy,
		CommitSHA:       d.CommitSha.String,
		CommitMessage:   d.CommitMessage.String,
		CommitAuthor:    d.CommitAuthor.String,
		ImageTag:        d.ImageTag.String,
		ErrorMessage:    d.ErrorMessage.String,
		HealthStatus:    d.HealthStatus.String,
		HealthMessage:   d.HealthMessage.String,
		StartedAt:       formatTimestamp(d.StartedAt),
		BuildStartedAt:  formatTimestamp(d.BuildStartedAt),
		BuildEndedAt:    formatTimestamp(d.BuildEndedAt),
		DeployStartedAt: formatTimestamp(d.DeployStartedAt),
		FinishedAt:      formatTimestamp(d.FinishedAt),
	}
}

type listDeploymentsInput struct {
	ApplicationID string `json:"application_id" jsonschema:"the application's id"`
	Limit         int    `json:"limit,omitempty" jsonschema:"maximum number of deployments to return, newest first (default 20, max 100)"`
}

type deploymentLogsInput struct {
	DeploymentID string `json:"deployment_id" jsonschema:"the deployment's id (from list_deployments)"`
	Tail         int    `json:"tail,omitempty" jsonschema:"number of most recent build-log lines to return (default 200, max 1000)"`
}

func registerDeploymentTools(srv *mcp.Server, queries *generated.Queries) {
	mcp.AddTool(srv, &mcp.Tool{
		Name: "get_deployment_logs",
		Description: "Tail a deployment's build log — the last N lines, not a stream. Use it to " +
			"find out why a deployment failed. One entry per line as \"<timestamp> <level> <message>\". " +
			"Returned verbatim apart from terminal colour codes: build output can contain secrets " +
			"printed by build steps. Defaults to 200 lines, capped at 1000.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in deploymentLogsInput) (*mcp.CallToolResult, any, error) {
		depID, err := parseUUID(in.DeploymentID)
		if err != nil {
			return nil, nil, err
		}
		row, err := queries.GetDeploymentLogAccess(ctx, depID)
		if err != nil {
			return nil, nil, notFoundOr(ctx, "deployment not found")
		}
		// Same pin + ownership check as authorizeApplication, inlined because
		// the access query already carries the owning project (as
		// get_application_logs does).
		if !pinAllows(ctx, uuidToString(row.ProjectID)) {
			return nil, nil, errAccessDenied
		}
		if !canAccessOwned(ctx, row.ProjectUserID, row.ProjectShared) {
			return nil, nil, errAccessDenied
		}

		tail := clampLimit(in.Tail, defaultLogTailLines, maxLogTailLines)
		text := "(no build log recorded for this deployment)"
		if row.BuildLogs.Valid && row.BuildLogs.String != "" {
			// Tail first, then reshape: the bounds apply to stored lines, and
			// formatting only ever shrinks them.
			text = stripANSI(formatBuildLog(tailLog(row.BuildLogs.String, tail, maxBuildLogBytes)))
		}
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: text}},
		}, nil, nil
	})

	mcp.AddTool(srv, &mcp.Tool{
		Name: "list_deployments",
		Description: "List an application's deployments, newest first: status, build outcome, " +
			"commit info, and the post-deploy health-check result. Bounded — defaults to 20, capped at 100.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in listDeploymentsInput) (*mcp.CallToolResult, any, error) {
		appID, err := parseUUID(in.ApplicationID)
		if err != nil {
			return nil, nil, err
		}
		app, err := queries.GetApplication(ctx, appID)
		if err != nil {
			return nil, nil, notFoundOr(ctx, "application not found")
		}
		if err := authorizeApplication(ctx, queries, app.ID, app.ProjectID); err != nil {
			return nil, nil, err
		}

		limit := clampLimit(in.Limit, defaultDeploymentsLimit, maxDeploymentsLimit)

		rows, err := queries.ListRecentDeploymentsByApplication(ctx, generated.ListRecentDeploymentsByApplicationParams{
			ApplicationID: appID,
			Limit:         int32(limit),
		})
		if err != nil {
			return nil, nil, internalError("failed to list deployments", err)
		}

		out := make([]deployment, 0, len(rows))
		for _, d := range rows {
			out = append(out, toDeployment(d))
		}
		return textResult(out)
	})
}

// formatBuildLog renders a stored build log for an assistant. A build log is
// NDJSON ({"ts","level","msg"} per line); the envelope is ~55 characters of
// scaffolding per line and the context window is the budget, so each line
// becomes "<ts> <level> <msg>" — what the dashboard shows a human. A line that
// is not a JSON object with a msg (stray or legacy content) passes through
// unchanged, as the frontend's parseLogBlob does.
func formatBuildLog(s string) string {
	lines := strings.Split(s, "\n")
	for i, line := range lines {
		var e struct {
			TS    string  `json:"ts"`
			Level string  `json:"level"`
			Msg   *string `json:"msg"`
		}
		if json.Unmarshal([]byte(line), &e) != nil || e.Msg == nil {
			continue
		}
		lines[i] = strings.TrimSpace(e.TS + " " + e.Level + " " + *e.Msg)
	}
	return strings.Join(lines, "\n")
}
