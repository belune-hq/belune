package mcpserver

import (
	"context"
	"errors"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/weiliang79/belune/internal/service"
)

type envVarEntry struct {
	Key      string `json:"key" jsonschema:"variable name: letters, digits and underscores, not starting with a digit"`
	Value    string `json:"value" jsonschema:"the value to store"`
	IsSecret *bool  `json:"is_secret,omitempty" jsonschema:"mark the value secret (hidden in listings). Omit it to keep an existing variable's current setting; a new variable defaults to secret"`
}

type setEnvVarsInput struct {
	ApplicationID string        `json:"application_id" jsonschema:"the application's id"`
	Variables     []envVarEntry `json:"variables" jsonschema:"the variables to set; only these are touched"`
}

type setEnvVarsResult struct {
	Variables []service.EnvVarResult `json:"variables"`
	Note      string                 `json:"note"`
}

func registerEnvVarTools(srv *mcp.Server, d Deps) {
	mcp.AddTool(srv, &mcp.Tool{
		Name: "set_application_env_vars",
		Description: "Set one or more environment variables on an application. This ADDS or UPDATES only the variables " +
			"you list: every other variable on the application is left exactly as it is, and this tool never removes a " +
			"variable. To change one variable, pass only that one. Values are stored encrypted and cannot be read back " +
			"through this tool. A new variable defaults to secret unless you pass is_secret=false; an existing variable " +
			"keeps its secret setting unless you pass is_secret. The change is saved immediately with no confirmation " +
			"step, but the running application keeps its old values until it is redeployed — call trigger_deployment " +
			"to apply them. It does not delete variables, and it cannot show you existing values.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in setEnvVarsInput) (*mcp.CallToolResult, any, error) {
		id, err := parseUUID(in.ApplicationID)
		if err != nil {
			return nil, nil, err
		}
		app, err := d.Queries.GetApplication(ctx, id)
		if err != nil {
			return nil, nil, notFoundOr(ctx, "application not found")
		}
		if err := authorizeApplication(ctx, d.Queries, app.ID, app.ProjectID); err != nil {
			return nil, nil, err
		}

		vars := make([]service.EnvVarSet, len(in.Variables))
		for i, v := range in.Variables {
			vars[i] = service.EnvVarSet{Key: v.Key, Value: v.Value, IsSecret: v.IsSecret}
		}
		results, err := d.Env.Merge(ctx, app.ID, vars)
		if err != nil {
			var invalid *service.InvalidEnvVarsError
			if errors.As(err, &invalid) {
				return nil, nil, invalid
			}
			return nil, nil, internalError("failed to save environment variables", err)
		}

		// Same action, resource type and details shape as the REST replace, so
		// one filter finds an environment change however it was made; the token
		// id on the row is what says it was an assistant. Key names only: the
		// log is the operator's record of what the assistant did, and must not
		// become a second copy of the secrets. A merge never removes, so
		// "removed" is always empty and is left out.
		created, updated := []string{}, []string{}
		for _, r := range results {
			if r.Created {
				created = append(created, r.Key)
			} else {
				updated = append(updated, r.Key)
			}
		}
		auditTool(ctx, d.Audit, "update_env_vars", "env_var", in.ApplicationID,
			map[string]any{"created": created, "updated": updated, "removed": []string{}})

		return textResult(setEnvVarsResult{
			Variables: results,
			Note:      "saved, not applied — the running application keeps its old values until trigger_deployment",
		})
	})
}
