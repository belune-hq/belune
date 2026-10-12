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
// REST, attributed to the calling token.
//
// ⚠️ Pass the CANONICAL id — uuidToString(row.ID) — never the id string the
// caller sent. pgtype.UUID parses "1F82A18D-..." and "1f82a18d-..." alike, so
// echoing the argument wrote whichever spelling the caller used and a query
// for the canonical form missed the row. The caller here is an LLM, which is
// far likelier to vary spelling than a dashboard. Same class as the deploy
// task id canonicalisation earlier on this branch. The caller is an LLM and this log is
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
