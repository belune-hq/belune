package mcpserver

import (
	"context"

	"github.com/weiliang79/belune/internal/server/middleware"
)

// Auditor is the slice of service.AuditService the mutating tools need. An
// interface so this package does not depend on the service's lifecycle, and
// so a test can capture rows.
type Auditor interface {
	Log(userID, tokenID, ipAddress, action, resourceType, resourceID string, details map[string]any)
}

// auditTool records a tool-driven mutation exactly as handler.audit does for
// REST, attributed to the calling token. The caller is an LLM and this log is
// the operator's only record of what it did, so a mutating tool that does not
// call this must not ship. The client IP rides the context because tools have
// no *http.Request (see HandleMCP).
func auditTool(ctx context.Context, a Auditor, action, resourceType, resourceID string, details map[string]any) {
	if a == nil {
		return
	}
	a.Log(
		middleware.UserIDFromContext(ctx),
		middleware.TokenIDFromContext(ctx),
		middleware.ClientIPFromContext(ctx),
		action, resourceType, resourceID, details,
	)
}
