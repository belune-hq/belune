package mcpserver

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/weiliang79/belune/internal/server/middleware"
)

// toolScopes declares the PAT scope every tool requires. POST /mcp is one
// route carrying every tool, and its RequireScope("read") gate is satisfied by
// a read, write OR deploy token — so it is only the floor, and cannot tell a
// read tool from a write one. This table is the actual control, enforced for
// every tools/call by requireToolScope. It is the same shape as the v0.1.12
// WebSocket escalation (one entry point, many capabilities, a route gate
// mistaken for authorization), so scope is declared explicitly here and never
// inferred from a tool's name.
//
// Fail closed: a tool absent from this table is refused, so a tool registered
// later without a scope decision is denied rather than open. The test lists
// the server's REAL registered tools and requires an exact match with this
// table in both directions.
var toolScopes = map[string]string{
	"list_projects":          "read",
	"get_project":            "read",
	"list_applications":      "read",
	"get_application":        "read",
	"list_databases":         "read",
	"get_database":           "read",
	"list_deployments":       "read",
	"get_deployment_logs":    "read",
	"get_application_logs":   "read",
	"list_domain_tls_status": "read",
	"list_project_backups":   "read",
	"trigger_deployment":     "deploy",
	"update_application":     "write",
	"start_application":      "write",
	"stop_application":       "write",
}

var errScopeDenied = errors.New("token lacks required scope")

// requireToolScope gates tools/call on toolScopes. It runs as receiving
// middleware rather than as a helper each handler must remember to call, so a
// handler cannot forget it. Other methods (initialize, tools/list, ping) pass
// through: listing tool names reveals nothing the public docs do not.
func requireToolScope(next mcp.MethodHandler) mcp.MethodHandler {
	return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
		if method != "tools/call" {
			return next(ctx, method, req)
		}
		params, ok := req.GetParams().(*mcp.CallToolParamsRaw)
		if !ok {
			return nil, errScopeDenied
		}
		required, declared := toolScopes[params.Name]
		if !declared {
			return nil, fmt.Errorf("unknown tool %q", params.Name)
		}
		// A session JWT never reaches /mcp (RequireToken), so nil scopes here
		// is anomalous and is refused rather than read as "unrestricted".
		scopes := middleware.ScopesFromContext(ctx)
		if !middleware.ScopesSatisfy(scopes, required) {
			// Scope is a property of the caller's own token, so saying it
			// back leaks nothing (unlike a pin or ownership refusal, which
			// must stay indistinguishable from absence) — and an assistant
			// can act on it by telling the user which scope to grant.
			held := "none"
			if len(scopes) > 0 {
				held = strings.Join(scopes, ", ")
			}
			return nil, fmt.Errorf("the %s tool requires the %q scope; this token has: %s", params.Name, required, held)
		}
		return next(ctx, method, req)
	}
}
