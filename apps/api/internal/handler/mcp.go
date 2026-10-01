package handler

import (
	"net/http"

	"github.com/weiliang79/belune/internal/server/middleware"
)

// HandleMCP serves the MCP (Model Context Protocol) server. The route
// (POST /mcp) gates identity and the read-scope floor — RequireToken and
// RequireScope("read") both run before this is reached — but NOT
// RequirePinnedProjectMatch: this route has no {projectId} URL param for it to
// compare a pin against, and its scope gate cannot tell a read tool from a
// write one. Both are enforced inside internal/mcpserver, per tool call:
// project pins via pinAllows/authorizeProject/authorizeApplication/
// authorizeDatabase (see access.go), and scope via the declared table in
// scope.go. The transport and tools themselves live in internal/mcpserver.
//
// Deliberately excluded from the generated OpenAPI reference: it is a single
// stateless JSON-RPC endpoint, not a REST resource, so documenting it as one
// operation with an opaque body would mislead more than it would inform. See
// apidocSkipNonRESTPrefixes in apidoc_generate_test.go.
func (h *Handler) HandleMCP(w http.ResponseWriter, r *http.Request) {
	// Tools have no *http.Request; the client IP is the one request value an
	// audit row needs, so it travels on the context like role and pin do.
	r = r.WithContext(middleware.ContextWithClientIP(r.Context(), middleware.ClientIP(r)))
	h.mcpHandler.ServeHTTP(w, r)
}
