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
	"github.com/weiliang79/belune/internal/store/generated"
	"github.com/weiliang79/belune/internal/version"
)

// New builds the MCP server and wraps it in a stateless streamable-HTTP
// handler. Stateless + JSONResponse: every tool here is a plain
// request/response with nothing server-initiated, so there is no SSE stream,
// no session store, and nothing lost on a control-plane restart — each
// request stands alone, authenticated by its own Bearer token exactly like
// REST.
func New(queries *generated.Queries, runtimes runtime.Runtimes, audit Auditor) http.Handler {
	srv := newServer(queries, runtimes, audit)
	return mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server {
		return srv
	}, &mcp.StreamableHTTPOptions{
		Stateless:    true,
		JSONResponse: true,
	})
}

// newServer is split out of New so a test can enumerate the REAL registered
// tools rather than a hand-written list.
func newServer(queries *generated.Queries, runtimes runtime.Runtimes, audit Auditor) *mcp.Server {
	srv := mcp.NewServer(&mcp.Implementation{
		Name:    "belune",
		Version: version.Version,
	}, nil)

	srv.AddReceivingMiddleware(requireToolScope)

	registerProjectTools(srv, queries)
	registerApplicationTools(srv, queries)
	registerDatabaseTools(srv, queries)
	registerDeploymentTools(srv, queries)
	registerLogTools(srv, queries, runtimes)
	registerDomainTools(srv, queries)
	registerBackupTools(srv, queries)

	return srv
}
