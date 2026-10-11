package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/weiliang79/belune/internal/server/middleware"
	"github.com/weiliang79/belune/internal/store/generated"
)

var errAccessDenied = errors.New("access denied")

// internalError logs the real cause server-side and returns a generic
// message as the tool's error. Returning err directly would embed its text
// — a raw DB error can name a table, column or constraint — verbatim in
// CallToolResult's content, sent to whichever client is connected. This is
// the same boundary handler.writeError already draws for the REST API;
// nothing in this package logged failures before this existed, so an
// operator had no server-side signal either.
func internalError(msg string, err error) error {
	slog.Error("mcpserver: "+msg, "error", err)
	return errors.New(msg)
}

// notFoundOr reports a resource lookup failure as errAccessDenied for a
// non-admin caller, and as the real notFoundMsg for an admin — mirroring
// handler.canAccessOwned, which treats an owner-lookup failure as a plain
// access failure for anyone but an admin. Collapsing "doesn't exist" into
// "access denied" for a non-admin is deliberate: distinguishing the two
// would let a token use a resource's mere existence as a cross-tenant
// oracle, which canAccessOwned was built to prevent on the REST side.
func notFoundOr(ctx context.Context, notFoundMsg string) error {
	if middleware.RoleFromContext(ctx) == "admin" {
		return errors.New(notFoundMsg)
	}
	return errAccessDenied
}

// uuidToString renders a nullable UUID as its string form, or "" (which
// callers pair with an `omitempty` json tag, like formatTimestamp) when the
// column was never set. Every current caller passes a NOT NULL column, so
// this branch is dead today — but it is the one shared id-to-string
// conversion used across every tool file, and the alternative is a future
// nullable-UUID field silently rendering as the all-zeros id instead of
// omitting itself.
func uuidToString(u pgtype.UUID) string {
	if !u.Valid {
		return ""
	}
	return uuid.UUID(u.Bytes).String()
}

func parseUUID(s string) (pgtype.UUID, error) {
	var id pgtype.UUID
	if err := id.Scan(s); err != nil {
		return id, errors.New("invalid id")
	}
	return id, nil
}

// formatTimestamp renders a nullable timestamp as RFC3339, or "" (which
// callers pair with an `omitempty` json tag) when the column was never set.
func formatTimestamp(t pgtype.Timestamptz) string {
	if !t.Valid {
		return ""
	}
	return t.Time.Format(time.RFC3339)
}

// textResult wraps a value as the tool's sole text content block, the shape
// every tool in this package returns. A tool-level failure should be
// returned as the handler's error instead of calling this — the SDK packs a
// returned error into CallToolResult with IsError set, which is how a
// caller's client distinguishes "tool ran, here's the answer" from "the call
// failed" without inspecting content itself.
func textResult(v any) (*mcp.CallToolResult, any, error) {
	body, err := json.Marshal(v)
	if err != nil {
		return nil, nil, internalError("failed to encode result", err)
	}
	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: string(body)}},
	}, nil, nil
}

// canAccessOwned mirrors handler.canAccessOwned: an admin always passes, a
// shared project grants every Member the same access as its owner, and
// otherwise only the owner passes. Kept in this package (not called from
// internal/handler) because internal/handler imports this package to wire
// the /mcp route, so the reverse import isn't available.
func canAccessOwned(ctx context.Context, ownerID pgtype.UUID, shared bool) bool {
	if middleware.RoleFromContext(ctx) == "admin" {
		return true
	}
	if shared {
		return true
	}
	var userID pgtype.UUID
	userID.Scan(middleware.UserIDFromContext(ctx))
	return ownerID == userID
}

// pinAllows is middleware.PinAllows under the name every call site in this
// package already uses. Every tool that resolves a project id must call
// this: unlike a REST route, an MCP tool call has no {projectId} URL param
// for middleware.RequirePinnedProjectMatch to enforce the pin against, so each
// tool does it itself, once per call.
func pinAllows(ctx context.Context, projectID string) bool {
	return middleware.PinAllows(ctx, projectID)
}

// pinnedProjectUUIDs resolves the caller's pin set, if any, to []pgtype.UUID
// suitable for a query's sqlc.narg('project_ids')::uuid[] — nil means
// "unpinned, don't filter" and passes through as SQL NULL; a non-nil (even
// empty) slice means "match exactly these ids, or nothing" via ANY(), which
// is how a token pinned to since-deleted projects correctly sees nothing
// rather than falling back to everything. Used by tools whose pin-filtering
// is pushed into SQL alongside a LIMIT (list_projects, list_domain_tls_status)
// rather than applied as a post-query Go-side narrowing: doing it in SQL
// means LIMIT can never truncate away a row a pinned token is allowed to see
// before the pin gets a chance to narrow the result to it.
func pinnedProjectUUIDs(ctx context.Context) ([]pgtype.UUID, error) {
	ids := middleware.TokenProjectsFromContext(ctx)
	if ids == nil {
		return nil, nil
	}
	out := make([]pgtype.UUID, len(ids))
	for i, id := range ids {
		if err := out[i].Scan(id); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// clampLimit normalizes a caller-supplied limit argument: <= 0 falls back to
// def, and anything over max is capped to it. Shared by every bounded list
// tool (list_projects, list_domain_tls_status, list_deployments,
// list_project_backups) so "bounded" means the same thing everywhere in
// this package.
func clampLimit(requested, def, max int) int {
	if requested <= 0 {
		return def
	}
	if requested > max {
		return max
	}
	return requested
}

// authorizeProject checks pin + ownership for a project id supplied
// directly as a tool argument, and returns the fetched row so callers
// building a project DTO (get_project) don't fetch it twice.
func authorizeProject(ctx context.Context, queries *generated.Queries, projectID pgtype.UUID) (generated.Project, error) {
	if !pinAllows(ctx, uuidToString(projectID)) {
		return generated.Project{}, errAccessDenied
	}
	project, err := queries.GetProject(ctx, projectID)
	if err != nil {
		return generated.Project{}, notFoundOr(ctx, "project not found")
	}
	if !canAccessOwned(ctx, project.UserID, project.Shared) {
		return generated.Project{}, errAccessDenied
	}
	return project, nil
}

// authorizeApplication checks pin + ownership given an application's own id
// and its parent project id — both already in hand from whatever row lookup
// (GetApplication, GetApplicationWithProjectSlug) the caller did to do its
// own job, so this never fetches the application itself.
func authorizeApplication(ctx context.Context, queries *generated.Queries, applicationID, projectID pgtype.UUID) error {
	if !pinAllows(ctx, uuidToString(projectID)) {
		return errAccessDenied
	}
	owner, err := queries.GetApplicationOwnerUserID(ctx, applicationID)
	if err != nil {
		return notFoundOr(ctx, "application not found")
	}
	if !canAccessOwned(ctx, owner.UserID, owner.Shared) {
		return errAccessDenied
	}
	return nil
}

// authorizeDatabase mirrors authorizeApplication for a database.
func authorizeDatabase(ctx context.Context, queries *generated.Queries, databaseID, projectID pgtype.UUID) error {
	if !pinAllows(ctx, uuidToString(projectID)) {
		return errAccessDenied
	}
	owner, err := queries.GetDatabaseOwnerUserID(ctx, databaseID)
	if err != nil {
		return notFoundOr(ctx, "database not found")
	}
	if !canAccessOwned(ctx, owner.UserID, owner.Shared) {
		return errAccessDenied
	}
	return nil
}

// projectIDInput is the shared argument shape for every tool scoped to one
// project (get_project, list_applications, list_databases,
// list_project_backups).
type projectIDInput struct {
	ProjectID string `json:"project_id" jsonschema:"the project's id"`
}

// applicationIDInput is the shared argument shape for every tool scoped to
// one application (get_application, list_deployments, get_application_logs).
type applicationIDInput struct {
	ApplicationID string `json:"application_id" jsonschema:"the application's id"`
}

// databaseIDInput is the shared argument shape for every tool scoped to one
// database (get_database).
type databaseIDInput struct {
	DatabaseID string `json:"database_id" jsonschema:"the database's id"`
}
