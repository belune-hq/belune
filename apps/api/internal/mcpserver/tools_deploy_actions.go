package mcpserver

import (
	"context"
	"errors"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/weiliang79/belune/internal/service"
	"github.com/weiliang79/belune/internal/store/generated"
)

// triggeredDeployment is deliberately smaller than `deployment`: the row has
// only just been queued, so every build/health field is empty and listing them
// would read as data. The caller follows the deployment with list_deployments
// and get_deployment_logs.
type triggeredDeployment struct {
	DeploymentID  string `json:"deployment_id"`
	ApplicationID string `json:"application_id"`
	Status        string `json:"status"`
	TriggeredBy   string `json:"triggered_by"`
}

func registerDeployActionTools(srv *mcp.Server, queries *generated.Queries, deploys *service.DeployQueue, audit Auditor) {
	mcp.AddTool(srv, &mcp.Tool{
		Name: "trigger_deployment",
		Description: "Start a deployment of an application right now: it builds the application's configured source " +
			"(or pulls its image) and replaces the running container, which restarts the app. " +
			"This takes effect immediately and there is no confirmation step. " +
			"It does not change any setting, and it does not wait for the deployment to finish — it returns a " +
			"deployment_id with status \"pending\"; use list_deployments and get_deployment_logs to follow it. " +
			"If a deploy, build, reload, rebuild or rollback is already running for this application it fails " +
			"with a conflict rather than queueing a second one.",
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

		d, err := deploys.TriggerDeployment(ctx, app.ID, "manual")
		if err != nil {
			if errors.Is(err, service.ErrDeployInProgress) {
				return nil, nil, errors.New("a deployment is already in progress for this application; follow it with list_deployments rather than triggering another")
			}
			return nil, nil, internalError("failed to trigger deployment", err)
		}

		// Same action name as the REST handler, so one filter in the audit
		// log finds a deploy however it was started; the token id on the row
		// is what says it was an assistant.
		auditTool(ctx, audit, "deploy_application", "application", in.ApplicationID, nil)

		return textResult(triggeredDeployment{
			DeploymentID:  uuidToString(d.ID),
			ApplicationID: uuidToString(app.ID),
			Status:        d.Status,
			TriggeredBy:   d.TriggeredBy,
		})
	})
}
