package handler

import (
	"context"
	"net/http"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/belune-hq/belune/internal/server/middleware"
)

// ownerLookup resolves a resource's parent project, its owning user id, and
// whether that project is shared.
//
// projectID is what lets canAccessOwned compose a project-pinned token's pin.
// It is the resource's ACTUAL project, read from the row — never the
// {projectId} in the URL, which is what the caller supplied and is exactly
// what must not be trusted. Every lookup resolves a real one; a resource with
// no project does not belong on this type.
type ownerLookup func(ctx context.Context) (projectID, ownerID pgtype.UUID, shared bool, err error)

// canAccessOwned reports whether the current request's user may access a
// resource given its owner and whether its parent project is shared. Admins
// always pass; a shared project grants every Member the same access as its
// owner. Use this for read/use access — destructive operations (delete,
// transfer, changing sharing) must use isOwnerOnly instead, since sharing
// grants operational access but never ownership.
func (h *Handler) canAccessOwned(r *http.Request, getOwner ownerLookup) bool {
	return h.ownedAccess(r, getOwner, ownerMayAccess)
}

// ownedAccess is the body canAccessOwned and isOwnerOnly share: resolve the
// resource, enforce a project-pinned token's pin against the project the
// resource ACTUALLY belongs to, then apply the caller's ownership rule.
//
// ⚠️⚠️ The pin check is the half that was missing, and its absence was a live
// cross-project read AND write. middleware.RequirePinnedProjectMatch compares
// the pin to the {projectId} in the URL and nothing verified that the child in
// the path belonged to it — so a pinned token could pass its OWN project id
// with ANY application, database or domain id and reach it. The admin
// short-circuit below used to run first, so for an admin-role pinned token the
// resource was never even looked up and no pin check was possible.
//
// This is the same composition the WebSocket authorizer has always done
// (wsProjectAccess: PinAllows on the resolved project, AND the ownership
// rule). REST now matches it.
//
// ⚠️ The short-circuit is kept for the UNPINNED admin, which is every session
// and every unpinned token — the overwhelmingly common case, and the one that
// would otherwise start paying a lookup it never paid before. Only a pinned
// caller resolves the resource, and a pinned caller is rare by construction.
func (h *Handler) ownedAccess(r *http.Request, getOwner ownerLookup, allow func(context.Context, pgtype.UUID, bool) bool) bool {
	ctx := r.Context()
	pinned := middleware.TokenPinnedFromContext(ctx)
	if !pinned && middleware.RoleFromContext(ctx) == "admin" {
		return true
	}
	projectID, ownerID, shared, err := getOwner(ctx)
	if err != nil {
		return false
	}
	// PinAllows passes an unpinned caller, so this is one rule rather than a
	// branch that could drift from the fast path above. Every ownerLookup
	// resolves a real project — canAccessIntegration, the one resource with no
	// project, does not come through here.
	if !middleware.PinAllows(ctx, uuidToString(projectID)) {
		return false
	}
	return allow(ctx, ownerID, shared)
}

// ownerMayAccess is the ownership/sharing rule behind canAccessOwned, on values
// the caller has already resolved: an admin passes, a shared project passes for
// every Member, otherwise only the owner does. It reads the caller from ctx
// rather than an *http.Request so the WebSocket channel authorizer — which has
// no per-message request, only the handshake's context — applies the very same
// rule instead of a copy that can drift. Deliberately does NOT look at a
// project-pinned token's pin: that is a separate axis (token reach, not
// ownership), composed by each caller — see wsProjectAccess.
func ownerMayAccess(ctx context.Context, ownerID pgtype.UUID, shared bool) bool {
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

// isOwnerOnly reports whether the current request's user owns the resource
// returned by getOwner. Admins always pass; unlike canAccessOwned, a shared
// project does NOT grant this — sharing widens operational access but never
// ownership, so this is what gates destroying the resource itself (as
// opposed to routine, recreatable config underneath it).
func (h *Handler) isOwnerOnly(r *http.Request, getOwner ownerLookup) bool {
	return h.ownedAccess(r, getOwner, ownerIsCaller)
}

// ownerIsCaller is isOwnerOnly's rule, in ownerMayAccess's shape so both can
// go through ownedAccess: an admin passes, otherwise only the owner — sharing
// deliberately does NOT, which is what makes this the destructive-operation
// gate.
func ownerIsCaller(ctx context.Context, ownerID pgtype.UUID, _ bool) bool {
	if middleware.RoleFromContext(ctx) == "admin" {
		return true
	}
	var userID pgtype.UUID
	userID.Scan(middleware.UserIDFromContext(ctx))
	return ownerID == userID
}

func (h *Handler) applicationOwner(applicationID pgtype.UUID) ownerLookup {
	return func(ctx context.Context) (pgtype.UUID, pgtype.UUID, bool, error) {
		row, err := h.queries.GetApplicationOwnerUserID(ctx, applicationID)
		return row.ProjectID, row.UserID, row.Shared, err
	}
}

func (h *Handler) databaseOwner(databaseID pgtype.UUID) ownerLookup {
	return func(ctx context.Context) (pgtype.UUID, pgtype.UUID, bool, error) {
		row, err := h.queries.GetDatabaseOwnerUserID(ctx, databaseID)
		return row.ProjectID, row.UserID, row.Shared, err
	}
}

func (h *Handler) domainOwner(domainID pgtype.UUID) ownerLookup {
	return func(ctx context.Context) (pgtype.UUID, pgtype.UUID, bool, error) {
		row, err := h.queries.GetDomainOwnerUserID(ctx, domainID)
		return row.ProjectID, row.UserID, row.Shared, err
	}
}

// canAccessApplication checks if the current user owns, or shares access to,
// the application's parent project. Admins bypass the check.
func (h *Handler) canAccessApplication(r *http.Request, applicationID pgtype.UUID) bool {
	return h.canAccessOwned(r, h.applicationOwner(applicationID))
}

// isApplicationOwner checks if the current user owns the application's parent
// project. Admins bypass the check. Shared access does NOT pass — gates
// deleting the application itself and its volumes, not routine operation.
func (h *Handler) isApplicationOwner(r *http.Request, applicationID pgtype.UUID) bool {
	return h.isOwnerOnly(r, h.applicationOwner(applicationID))
}

// canAccessDatabase checks if the current user owns, or shares access to, the
// database's parent project. Admins bypass the check.
func (h *Handler) canAccessDatabase(r *http.Request, databaseID pgtype.UUID) bool {
	return h.canAccessOwned(r, h.databaseOwner(databaseID))
}

// isDatabaseOwner checks if the current user owns the database's parent
// project. Admins bypass the check. Shared access does NOT pass — gates
// deleting the database itself and its backups, not routine operation.
func (h *Handler) isDatabaseOwner(r *http.Request, databaseID pgtype.UUID) bool {
	return h.isOwnerOnly(r, h.databaseOwner(databaseID))
}

// canAccessIntegration checks if the current user created the git connection.
// Admins bypass the check. Git integrations are user-level resources with no
// project relationship, so project sharing never applies here.
func (h *Handler) canAccessIntegration(r *http.Request, integrationID pgtype.UUID) bool {
	// ⚠️ Deliberately NOT through canAccessOwned. That composes a project pin,
	// and a git integration has no project for one to match — routing it
	// through there would need a "no project" carve-out in the hot path, and
	// the API-reference generator (which detects pinning from the reachable
	// body) would then publish "scoped to a project-pinned token's pin set"
	// on these routes, which is the opposite of the truth. Caught by watching
	// it do exactly that.
	//
	// Behaviour is unchanged from before the pin composition existed: the
	// ownership rule alone. Whether a pinned token should reach project-less
	// resources is a real question, and a separate decision from this fix.
	ownerID, err := h.gitIntegrationSvc.OwnerID(r.Context(), integrationID)
	if err != nil {
		return false
	}
	return ownerMayAccess(r.Context(), ownerID, false)
}

// canAccessDomain checks if the current user owns, or shares access to, the
// domain's parent application's parent project. Admins bypass the check.
func (h *Handler) canAccessDomain(r *http.Request, domainID pgtype.UUID) bool {
	return h.canAccessOwned(r, h.domainOwner(domainID))
}

// canAttachGitIntegration reports whether the current user may attach the
// given git integration to an application. A zero/invalid id (none supplied)
// always passes — there is nothing to check. Otherwise this is exactly
// canAccessIntegration: git integrations are user-level, and project sharing
// does not extend to them, so a shared member reading another user's real
// git_integration_id off a shared application's JSON must not be able to
// attach it to an application in a project of their own.
func (h *Handler) canAttachGitIntegration(r *http.Request, integrationID pgtype.UUID) bool {
	if !integrationID.Valid {
		return true
	}
	return h.canAccessIntegration(r, integrationID)
}

// isDomainOwner checks if the current user owns the domain's parent
// application's parent project. Admins bypass the check. Shared access does
// NOT pass — gates removing the domain itself, not routine operation.
func (h *Handler) isDomainOwner(r *http.Request, domainID pgtype.UUID) bool {
	return h.isOwnerOnly(r, h.domainOwner(domainID))
}
