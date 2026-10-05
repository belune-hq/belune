// Package mcpserver implements Belune's Model Context Protocol server. It is
// registered as a single stateless JSON-RPC-over-HTTP handler at POST /mcp
// (see internal/server/routes.go) so an AI assistant can inspect and operate
// projects, applications, deployments and infrastructure state through the
// same personal-access-token model a CI script would use.
//
// Destructive tools (delete, restore, anything dropping a volume or backup)
// are out of scope permanently: a token cannot destroy, so registerXTools
// functions in this package must never add one. Mutating tools are a later
// phase than the read tools, and each must declare the PAT scope it requires,
// because the /mcp route can only enforce the read floor. Tool handlers read
// request identity (role, user id, project pin) off the context via
// internal/server/middleware accessors — the SDK threads the originating
// *http.Request's context through to every tool call, so the same
// Auth/RequireScope/RequireToken chain that gates the route gates each tool
// invocation too.
package mcpserver

import (
	"net/http"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/weiliang79/belune/internal/runtime"
	"github.com/weiliang79/belune/internal/service"
	"github.com/weiliang79/belune/internal/store/generated"
	"github.com/weiliang79/belune/internal/version"
)

// Deps is everything the tools need. A struct rather than positional
// arguments because each mutating tool brings its own service, and a
// positional list of same-shaped pointers is how two get swapped.
type Deps struct {
	Queries  *generated.Queries
	Runtimes runtime.Runtimes
	Audit    Auditor
	Deploys  *service.DeployQueue
	Apps     *service.ApplicationService
	Env      *service.EnvVarService
}

// New builds the MCP server and wraps it in a stateless streamable-HTTP
// handler. Stateless + JSONResponse: every tool here is a plain
// request/response with nothing server-initiated, so there is no SSE stream,
// no session store, and nothing lost on a control-plane restart — each
// request stands alone, authenticated by its own Bearer token exactly like
// REST.
func New(d Deps) http.Handler {
	srv := newServer(d)
	return mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server {
		return srv
	}, &mcp.StreamableHTTPOptions{
		Stateless:    true,
		JSONResponse: true,
	})
}

// newServer is split out of New so a test can enumerate the REAL registered
// tools rather than a hand-written list.
func newServer(d Deps) *mcp.Server {
	srv := mcp.NewServer(&mcp.Implementation{
		Name:    "belune",
		Version: version.Version,
	}, nil)

	srv.AddReceivingMiddleware(requireToolScope)

	registerProjectTools(srv, d.Queries)
	registerApplicationTools(srv, d.Queries)
	registerDatabaseTools(srv, d.Queries)
	registerDeploymentTools(srv, d.Queries)
	registerLogTools(srv, d.Queries, d.Runtimes)
	registerDomainTools(srv, d.Queries)
	registerBackupTools(srv, d.Queries)
	registerDeployActionTools(srv, d.Queries, d.Deploys, d.Audit)
	registerApplicationActionTools(srv, d)
	registerApplicationLifecycleTools(srv, d)
	registerEnvVarTools(srv, d)

	return srv
}
