package middleware

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/google/uuid"

	"github.com/weiliang79/belune/internal/service"
)

type contextKey string

const (
	ctxUserID     contextKey = "user_id"
	ctxEmail      contextKey = "email"
	ctxRole       contextKey = "role"
	ctxTokenID    contextKey = "token_id"
	ctxScopes     contextKey = "scopes"
	ctxPinned     contextKey = "token_pinned"
	ctxProjectIDs contextKey = "token_project_ids"
)

// Auth returns a middleware that authenticates a session JWT or a personal
// access token. The PAT branch is a prefix check, not a new route group —
// every route already behind Auth gains PAT support with no further wiring.
//
// A PAT is only ever accepted from the Authorization header, never the
// session cookie extractToken also falls back to — unlike a JWT, a PAT is
// never meant to be an ambient browser credential, so it must come from
// somewhere a script explicitly put it.
func Auth(authService *service.AuthService, tokenService *service.TokenService) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if bearer, ok := bearerToken(r); ok && service.HasTokenPrefix(bearer) {
				tok, err := tokenService.Authenticate(r.Context(), bearer)
				if err != nil {
					if errors.Is(err, service.ErrInvalidAPIToken) {
						http.Error(w, `{"error":"invalid or expired token"}`, http.StatusUnauthorized)
						return
					}
					// A lookup failure (DB down, pool exhausted) is not the
					// token's fault — reporting it as 401 tells a CI/CLI
					// client its credential was rejected, and such clients
					// are built to react by rotating or deleting it instead
					// of retrying a transient failure.
					slog.Error("auth: token lookup failed", "error", err)
					http.Error(w, `{"error":"internal error"}`, http.StatusInternalServerError)
					return
				}

				ctx := context.WithValue(r.Context(), ctxUserID, uuid.UUID(tok.UserID.Bytes).String())
				ctx = context.WithValue(ctx, ctxRole, tok.EffectiveRole)
				ctx = context.WithValue(ctx, ctxTokenID, uuid.UUID(tok.TokenID.Bytes).String())
				ctx = context.WithValue(ctx, ctxScopes, tok.Scopes)
				ctx = context.WithValue(ctx, ctxPinned, tok.Pinned)
				if tok.Pinned {
					ctx = context.WithValue(ctx, ctxProjectIDs, tok.ProjectIDs)
				}
				next.ServeHTTP(w, r.WithContext(ctx))
				return
			}

			tokenString := extractToken(r)
			if tokenString == "" {
				http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
				return
			}

			claims, err := authService.ValidateToken(r.Context(), tokenString)
			if err != nil {
				http.Error(w, `{"error":"invalid or expired token"}`, http.StatusUnauthorized)
				return
			}

			ctx := context.WithValue(r.Context(), ctxUserID, claims.UserID)
			ctx = context.WithValue(ctx, ctxEmail, claims.Email)
			ctx = context.WithValue(ctx, ctxRole, claims.Role)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// bearerToken returns the Authorization header's Bearer value only — no
// cookie fallback. Used by the PAT branch, which must never accept a token
// from an ambient credential.
func bearerToken(r *http.Request) (string, bool) {
	auth := r.Header.Get("Authorization")
	if strings.HasPrefix(auth, "Bearer ") {
		return strings.TrimPrefix(auth, "Bearer "), true
	}
	return "", false
}

func extractToken(r *http.Request) string {
	// Check Authorization header first
	if v, ok := bearerToken(r); ok {
		return v
	}

	// Fall back to cookie
	cookie, err := r.Cookie("token")
	if err == nil {
		return cookie.Value
	}

	return ""
}

// RequireRole returns a middleware that checks the user's role against the allowed roles.
// Must be used after Auth middleware.
func RequireRole(roles ...string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			role := RoleFromContext(r.Context())
			for _, allowed := range roles {
				if role == allowed {
					next.ServeHTTP(w, r)
					return
				}
			}
			http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
		})
	}
}

// RequireSession returns a middleware that rejects a PAT-authenticated
// request, accepting only a session JWT. Must be used after Auth.
//
// A PAT minting or revoking another PAT is a self-propagation path a scope
// alone cannot close: token creation is a legitimate "write" action, so even
// after scope enforcement lands a write-scoped token could still use it to
// mint itself a longer-lived (or never-expiring) replacement and delete the
// original — surviving a password reset entirely, since RevokeUserSessions
// only touches JWT refresh tokens. Same reasoning TOTP enroll/disable already
// applies to itself. This is also the seam PR4's "tokens cannot destroy"
// boundary is expected to reuse for its own destructive routes.
func RequireSession() func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if TokenIDFromContext(r.Context()) != "" {
				http.Error(w, `{"error":"this action requires a session, not a personal access token"}`, http.StatusForbidden)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// RequireToken returns a middleware that rejects a session-authenticated
// request, accepting only a personal access token. Must be used after Auth.
// The mirror image of RequireSession.
//
// MCP clients are machines, not a human at a browser — accepting a session
// JWT on /mcp would blur the boundary the PAT model draws, and CSRF already
// exempts the Bearer branch (see csrf.go), so a JWT arriving there would skip
// that protection with no compensating control. There is no legitimate
// reason for a dashboard session to call it.
func RequireToken() func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if TokenIDFromContext(r.Context()) == "" {
				http.Error(w, `{"error":"this action requires a personal access token, not a session"}`, http.StatusForbidden)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// UserIDFromContext returns the authenticated user's ID from the request context.
func UserIDFromContext(ctx context.Context) string {
	v, _ := ctx.Value(ctxUserID).(string)
	return v
}

// EmailFromContext returns the authenticated user's email from the request context.
// Empty for a PAT-authenticated request — nothing reads it today, and a token
// lookup does not carry the owner's email along for a value nothing uses.
func EmailFromContext(ctx context.Context) string {
	v, _ := ctx.Value(ctxEmail).(string)
	return v
}

// RoleFromContext returns the authenticated user's role from the request
// context — for a PAT this is already the effective role (min(role at issue,
// the owner's role now)), not the owner's raw current role.
func RoleFromContext(ctx context.Context) string {
	v, _ := ctx.Value(ctxRole).(string)
	return v
}

// TokenIDFromContext returns the authenticating PAT's id, or "" for a session
// JWT. h.audit reads this to attribute a mutation to the token that made it
// rather than silently reading as a human action.
func TokenIDFromContext(ctx context.Context) string {
	v, _ := ctx.Value(ctxTokenID).(string)
	return v
}

// ContextWithUserID and ContextWithTokenID put an identity into a context the
// way Auth does. The keys are unexported, so without these the only way to
// reach code that reads them is through Auth itself — a real JWT, a real token
// row and a database — which is far too much apparatus for something that only
// needs to know WHICH KIND of caller it is looking at. rateLimitByCaller's test
// is the first consumer; see internal/server/rate_limit_test.go.
//
// ⚠️⚠️ Be precise about WHY this is safe, because the obvious answer is wrong.
// It is NOT that the gates downstream re-check everything: RequireScope and
// RequireScopeByMethod pass unconditionally when ScopesFromContext is nil (a
// session implies every scope), and PinAllows returns true for a nil pin set —
// so a context built here carries an unpinned, unrestricted-scope identity. It
// has no role, and nothing here can give it one.
//
// What makes it safe is narrower and stronger: nothing in production calls
// these (the only callers are in rate_limit_test.go), and every route runs Auth
// first, which layers its own values over whatever context arrived. The one
// value Auth's session branch does NOT set is ctxTokenID, so a planted token id
// would survive — and that fails CLOSED, because RequireSession then rejects
// the request as a token.
//
// ⛔ So do not reach for these in production code to "present an identity".
// Authorization here is a property of the middleware chain, not of the context.
func ContextWithUserID(ctx context.Context, userID string) context.Context {
	return context.WithValue(ctx, ctxUserID, userID)
}

// ContextWithTokenID is ContextWithUserID for a PAT's id.
func ContextWithTokenID(ctx context.Context, tokenID string) context.Context {
	return context.WithValue(ctx, ctxTokenID, tokenID)
}

// ContextWithScopes is the setter counterpart of ScopesFromContext, for tests
// of code that gates on scope without running the Auth middleware.
//
// ⚠️ Same caveat as the two above, and it bites harder here: a context built
// by this helper carries whatever scopes the test names, so it is presenting
// an authorization decision rather than an identity. Test-only for the same
// reason — nothing in production calls it, and every route runs Auth first,
// which overwrites ctxScopes with the real token's scopes.
func ContextWithScopes(ctx context.Context, scopes []string) context.Context {
	return context.WithValue(ctx, ctxScopes, scopes)
}

// ScopesFromContext returns the authenticating PAT's scopes, or nil for a
// session JWT (a session implies every scope — callers should treat a nil
// slice from a JWT request as "unrestricted", not "no scopes").
func ScopesFromContext(ctx context.Context) []string {
	v, _ := ctx.Value(ctxScopes).([]string)
	return v
}

// TokenPinnedFromContext reports whether the authenticating PAT is
// project-pinned — false for an unpinned token (reaches every project its
// owner can access) and for a session, which is never pinned.
func TokenPinnedFromContext(ctx context.Context) bool {
	v, _ := ctx.Value(ctxPinned).(bool)
	return v
}

// TokenProjectsFromContext returns nil when the request is unpinned (no
// filtering: every project the owner can reach), or the authenticating PAT's
// pinned project ids otherwise — non-nil but possibly empty, when every
// project it was pinned to has since become unreachable (deleted, or the
// owner lost access). Callers must treat nil and "non-nil, empty" as
// different answers: nil means "don't filter," empty means "filter to
// nothing." Project reach follows current access, evaluated at use time —
// this is read fresh from the token row on every request, never cached past
// it.
func TokenProjectsFromContext(ctx context.Context) []string {
	if !TokenPinnedFromContext(ctx) {
		return nil
	}
	v, _ := ctx.Value(ctxProjectIDs).([]string)
	if v == nil {
		return []string{}
	}
	return v
}
