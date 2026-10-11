package middleware

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"

	"github.com/go-chi/chi/v5"
)

// scopeGrants maps a required scope to the token scopes that satisfy it.
// "write" is the umbrella: it satisfies a "read" or "deploy" requirement too,
// since a general-purpose token (today, every token — no picker existed
// before this) can already do everything a narrower one can. "read" in turn
// satisfies "metrics", since metrics are just a narrower slice of what a
// general read already covers. "deploy" ALSO satisfies "read" — the design's
// own CI use case ("let CI deploy app X") almost always polls the deployment
// afterward, so a deploy-only token that could trigger a deploy but never
// observe it would be a self-inflicted footgun, not a meaningful narrowing.
// Following from that same edge, "deploy" also satisfies "metrics" — it
// already satisfies "read", which satisfies "metrics", so metrics not
// following transitively made "read" and "deploy" incomparable for no
// reason (a deploy token could poll full deployment history but 403 on a
// narrower metrics snapshot). This makes the four scopes a true total order:
// metrics ⊂ read ⊂ deploy ⊂ write. The remaining direction never holds:
// deploy does NOT imply write (a CI deploy token must not also be able to
// rewrite env vars or delete a backup), and metrics does not imply read,
// deploy, or write (a Prometheus scrape token must not be able to read
// arbitrary project data or trigger a deploy) — capability only ever
// shrinks along those edges, never round-trips.
var scopeGrants = map[string][]string{
	"read":    {"read", "write", "deploy"},
	"write":   {"write"},
	"deploy":  {"deploy", "write"},
	"metrics": {"metrics", "read", "write", "deploy"},
}

func scopeSatisfies(tokenScopes []string, required string) bool {
	granting := scopeGrants[required]
	for _, s := range tokenScopes {
		for _, g := range granting {
			if s == g {
				return true
			}
		}
	}
	return false
}

// writeForbiddenScope writes the standard "token lacks scope" 403. Kept as
// one place so RequireScope and RequireScopeByMethod produce an identical
// body regardless of how the requirement was derived.
func writeForbiddenScope(w http.ResponseWriter, required string) {
	http.Error(w, `{"error":"token lacks required scope: `+required+`"}`, http.StatusForbidden)
}

// RequireScope returns a middleware that rejects a PAT lacking the given
// scope. A session JWT always passes — ScopesFromContext returns nil for one,
// and a session implies every scope. Must be used after Auth.
func RequireScope(required string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			scopes := ScopesFromContext(r.Context())
			if scopes == nil {
				next.ServeHTTP(w, r)
				return
			}
			if !scopeSatisfies(scopes, required) {
				writeForbiddenScope(w, required)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// RequireScopeByMethod returns a middleware that requires "read" for a safe
// method (GET/HEAD/OPTIONS) and "write" for everything else. This is the
// default every ordinary CRUD route gets — a new route added under a group
// carrying this middleware needs no per-route scope annotation to be safe by
// default. Routes needing a narrower or different scope (deploy actions,
// metrics reads) are registered in their own group with RequireScope instead
// of this one — see routes.go.
func RequireScopeByMethod() func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			required := "write"
			switch r.Method {
			case http.MethodGet, http.MethodHead, http.MethodOptions:
				required = "read"
			}
			scopes := ScopesFromContext(r.Context())
			if scopes == nil {
				next.ServeHTTP(w, r)
				return
			}
			if !scopeSatisfies(scopes, required) {
				writeForbiddenScope(w, required)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// PinAllows reports whether a project-pinned token may reach projectID —
// true for an unpinned token or one pinned to a set that includes projectID.
// A pinned token whose set is empty (every project it was pinned to has
// since become unreachable) allows nothing — an empty intersection, never a
// fallback to unrestricted. Shared by RequirePinnedProjectMatch below (which
// compares against a {projectId} URL param) and internal/mcpserver's tools,
// which have no URL param to compare against and call this directly, once
// per tool call, instead.
func PinAllows(ctx context.Context, projectID string) bool {
	ids := TokenProjectsFromContext(ctx)
	if ids == nil {
		return true
	}
	return slices.Contains(ids, projectID)
}

// DenyPinnedToken refuses a project-pinned token outright, with reason in the
// body. For an install-wide surface that cannot be expressed in terms of a pin
// at all — the audit log, whose rows carry a polymorphic resource reference and
// no project — so there is nothing to narrow to and serving it whole would be a
// cross-project disclosure.
//
// ⚠️ Prefer narrowing where a project can be resolved; this is the answer only
// when it cannot. Sessions and unpinned tokens pass through untouched, so the
// dashboard is unaffected. Must be used after Auth.
func DenyPinnedToken(reason string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if TokenPinnedFromContext(r.Context()) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusForbidden)
				// Marshalled rather than interpolated: reason is a caller's
				// string, and a quote or backslash in one would emit a
				// malformed body that a client parses as a transport error
				// instead of a 403 with a reason. Every call site passes a
				// constant today — this keeps that from being load-bearing.
				// handler.writeError is unreachable from here (import cycle).
				_ = json.NewEncoder(w).Encode(map[string]string{"error": reason})
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// RequirePinnedProjectMatch returns a middleware that rejects a request whose
// {projectId} URL param does not match a project-pinned PAT's pin. A route
// with no projectId param, or a token that isn't pinned (every existing
// token today, and a session always), passes through untouched. Must be used
// after Auth, and after chi has parsed URL params (i.e. inside the route
// tree, not before it).
//
// ⚠️⚠️ It compares the pin to the URL AND NOTHING ELSE. It does not check that
// a {applicationId}, {databaseId} or {domainId} further along the path belongs
// to that project, and it cannot — it has no database. Until v0.1.19 it was
// named RequireProjectAccess, and that name is why this gap went unnoticed on
// ~92 nested route registrations: reviewers, comments and handlers all read
// "project access is handled" and stopped looking. A pinned token could pass
// its OWN project id with ANY child id and read, or write, the resource.
//
// The parentage half lives in handler.ownedAccess, which resolves the
// resource's ACTUAL project and runs middleware.PinAllows against that. Both
// are needed: this one is applied by route-group registration and so cannot be
// forgotten, and it refuses before any database work; that one is the only
// place that can know what the child really belongs to.
func RequirePinnedProjectMatch() func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			requested := chi.URLParam(r, "projectId")
			if requested == "" {
				next.ServeHTTP(w, r)
				return
			}
			if !PinAllows(r.Context(), requested) {
				http.Error(w, `{"error":"token is pinned to a different project"}`, http.StatusForbidden)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
