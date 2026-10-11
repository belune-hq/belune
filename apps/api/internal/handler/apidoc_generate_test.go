// apidoc_generate_test.go generates apps/site/public/openapi.json — an
// OpenAPI 3.1 spec — by combining two independent sources of truth about the
// real API, neither of which a hand-written reference can get right at this
// scale (212 routes, 1,829 response field slots):
//
//  1. STATIC extraction (apidocExtractTypes) reads request/response Go types
//     directly via golang.org/x/tools/go/packages — a full go/types load of
//     this package, the same information `go vet` has. It resolves every
//     writeJSON call's data argument and every json.Decoder.Decode target to
//     its static type, then walks that type into a JSON Schema.
//  2. DYNAMIC probing (the same chi.Walk + PAT mechanism destroy_boundary_test.go
//     and reveal_boundary_test.go already use) tells us what static types
//     alone cannot: which scope a route needs. A scope requirement is a
//     runtime CLOSURE VALUE (RequireScope("write")) — no amount of reading
//     routes.go recovers "write" from that without actually calling the
//     route and reading the rejection message.
//
// Not a correctness test — a generation tool that happens to reuse the test
// harness (a real Postgres via testcontainers; Docker, Redis, and asynq are
// mocked, see testutil.SetupTestServer). Skipped unless GENERATE_API_REFERENCE=1
// is set, so it never runs as part of the ordinary suite. Invoke via
// `task generate:api-docs`.
//
// One probe per route: a ZERO-SCOPE token (scopes = []string{}, non-nil-but-
// empty — minted by inserting directly via queries.CreateAPIToken, since
// POST /api/tokens itself now rejects an empty scopes array).
// RequireScope/RequireScopeByMethod reject it with "token lacks required
// scope: X", naming the requirement directly — and the handler NEVER RUNS,
// because scopeSatisfies's loop over an empty slice can't match anything.
// This probe is SAFE ON EVERY ROUTE, no exceptions, and needs none of the
// id-less-mutating-route caution a probe that could execute a handler would.
// A route gated by RequireSession with no RequireScope* in front of it at
// all (e.g. the WS terminal tunnel) rejects the zero-scope token with a
// different message ("this action requires a session, not a personal access
// token") before scope ever comes into it — Scope reads as "none" for those.
//
// Role AND session-only gating are NOT probed at all — both are read
// structurally off the same chi.Walk call, for a reason specific to what
// reflection can and can't recover: a function-name check via
// runtime.FuncForPC sees that RequireRole or RequireSession is IN a chain,
// exact and free, no HTTP round trip. RequireSession is genuinely fixed
// (its presence is the whole story). RequireRole is variadic — its role
// arguments are closure-captured and reflection can't read them — so the
// role set comes from parsing routes.go instead (apidocRequireRoleSet),
// which is single-valued only because every call site agrees; a
// disagreement fails generation rather than guessing which route got which
// (internal/server/routes_test.go's TestRequireRoleCallsAgreeOnRoleSet is
// the same check at the source). An earlier
// version of this file probed RequireSession with a second, full-scope
// token instead (withholding the probe, and thus the answer, on id-less
// mutating routes) — before a peer review pointed out RequireSession is
// exactly as structurally readable as RequireRole already was, and that the
// permissive fallback for an unanswered probe (list a PAT scheme alongside
// session, since the restriction "may not exist") was actively dangerous in
// this direction: it had POST /api/tokens, POST /api/users and POST
// /api/users/invite — session-only in reality — documented as callable with
// a write-scoped PAT, on POST /api/tokens the exact self-mint escalation
// path #16 exists to close. RequireScope(required) is different from both:
// it's parameterized by a scope STRING captured in the closure, which a
// function name can't recover without reaching into unexported runtime
// internals — that's the one thing only a live probe can actually tell you,
// which is exactly why probing is still this generator's technique for
// scope, not a stylistic choice.
//
// SECURITY ENCODING: OpenAPI's `scopes` array on a security requirement is
// legal ONLY for oauth2/openIdConnect schemes — for http/bearer it MUST be
// empty in every ratified version of the spec (3.0.x through 3.2.0; a
// "MAY contain role names" relaxation exists in secondary sources but was
// never merged into a published spec). Belune has no OAuth2 authorization
// server for PATs (scopes are chosen at mint time in the dashboard, no
// flows/authorizationUrl/tokenUrl exist), so declaring oauth2 to unlock that
// array would make codegen attempt a handshake against endpoints that don't
// exist — strictly worse than not having it. Instead the requirement is
// carried in the SECURITY SCHEME NAME, named by what a token must SATISFY
// (not what it holds), reusing the scope lattice's own total order
// (metrics ⊂ read ⊂ deploy ⊂ write — see the comment on scopeGrants in
// middleware/scope.go): each scheme is named for the scope it satisfies —
// metrics/read/deploy/write — plus session. A route needing `read` lists
// ONLY `read` — a token holding read, deploy, OR write all satisfy it, so
// naming by minimum keeps it to one scheme per route instead of enumerating
// every tier that would also work. `session` is OR'd into every
// non-session-exclusive requirement, since RequireScope lets a session JWT
// pass unconditionally (ScopesFromContext returns nil for one).
//
// RequireRole gating is NOT a scheme. A role is not a credential — an
// earlier "adminRole" scheme typed as http/bearer made renderers offer a
// bearer field nobody can fill. r.Roles (the role set, from
// apidocRequireRoleSet) is emitted as the x-belune-roles vendor extension
// (apidocBuildDocument) and a "Requires the … role." sentence in the
// operation description (apidocRoleNote — derived, no "admin" literal); the
// site turns x-belune-roles into `roles:` page frontmatter and a sidebar
// badge. TestGenerateAPIReference's role invariant checks r.Roles against
// api-domains.json's " (Admin)" tag suffix, the one role signal a human
// curates independently.
package handler_test

import (
	"context"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/constant"
	"go/token"
	"go/types"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/tools/go/packages"

	"github.com/weiliang79/belune/internal/service"
	"github.com/weiliang79/belune/internal/store/generated"
	"github.com/weiliang79/belune/internal/testutil"
)

// apidocErrorMessage reads a response's "error" field if it's a 403, without
// assuming the body is any particular shape otherwise — a 200 on a list
// endpoint is a JSON ARRAY, not an object (testutil.ReadJSON would panic on
// exactly that), and this generator only ever needs the body on a 403.
func apidocErrorMessage(t *testing.T, resp *http.Response) string {
	t.Helper()
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		io.Copy(io.Discard, resp.Body) //nolint:errcheck
		return ""
	}
	raw, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	var body struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(raw, &body) != nil {
		return "" // non-JSON 403 (shouldn't happen on this router, but never fatal)
	}
	return body.Error
}

// apidocRoute is one route's fully observed (or structurally read, for the
// role and session flags) shape.
type apidocRoute struct {
	Method  string
	Path    string
	Handler string
	Scope   string // "read"/"write"/"deploy"/"metrics"/"none"; see the UNRECOGNIZED fallback below if nothing matched
	Session bool   // RequireSession read structurally from the middleware chain — see the top-of-file note on why this isn't probed
	// Roles is the role set a RequireRole in the chain demands, or nil if
	// there is none. Reflection only recovers that RequireRole is PRESENT,
	// never its closure-captured arguments — the set itself comes from
	// parsing routes.go (apidocRequireRoleSet), which is single-valued only
	// because every call site agrees (routes_test.go enforces that).
	Roles  []string
	Pinned bool // path contains {projectId} — RequirePinnedProjectMatch applies
	// Query and HandlerPinned come from the handler's BODY (apidocExtractBehaviour),
	// not the router. HandlerPinned is the pin enforced in-handler on routes with
	// no {projectId} for RequirePinnedProjectMatch to read — a real gate the route
	// registration shows nothing of.
	Query         []string
	HandlerPinned bool
}

// apidocRouteKey is "METHOD /path" — a stable, comparable identity for a
// route (apidocRoute itself stopped being a valid map key once Roles made it
// hold a slice). One (method, path) pair is one route.
func apidocRouteKey(r apidocRoute) string { return r.Method + " " + r.Path }

// apidocRequireRoleSet parses ../server/routes.go for every
// middleware.RequireRole(...) call and returns the role set they all name,
// sorted. RequireRole is variadic (an allowed-set, "any of these passes"),
// applied at group level via r.Use() — three call sites today, all
// ("admin"). Reflection can see RequireRole is in a route's middleware chain
// but not which arguments it carried, so attribution to individual routes is
// only possible while every call site agrees; a disagreement fails
// generation here rather than guessing (routes_test.go's
// TestRequireRoleCallsAgreeOnRoleSet is the same check at the source, so
// this normally can't be the thing that trips — but the generator must not
// depend on another package's test having run).
func apidocRequireRoleSet(t *testing.T) []string {
	t.Helper()
	src, err := os.ReadFile("../server/routes.go")
	require.NoError(t, err, "reading ../server/routes.go")
	calls := regexp.MustCompile(`middleware\.RequireRole\(([^)]*)\)`).FindAllStringSubmatch(string(src), -1)
	require.NotEmpty(t, calls, "no middleware.RequireRole(...) call found in routes.go — role detection would read a dead path")

	parse := func(argList string) []string {
		var roles []string
		for _, a := range strings.Split(argList, ",") {
			if a = strings.Trim(strings.TrimSpace(a), `"`); a != "" {
				roles = append(roles, a)
			}
		}
		sort.Strings(roles)
		return roles
	}

	want := parse(calls[0][1])
	for _, c := range calls[1:] {
		require.Equal(t, want, parse(c[1]), "middleware.RequireRole call sites name different role sets (%v vs %v) — the generator attributes ONE set to every RequireRole-gated route, so they must agree", want, parse(c[1]))
	}
	require.NotEmpty(t, want, "middleware.RequireRole(...) called with no role argument")
	return want
}

// apidocRoleNote renders a role set as the sentence appended to an
// operation's description — ["admin"] -> "Requires the Admin role.",
// ["admin","owner"] -> "Requires the Admin or Owner role." Nothing here
// hardcodes "admin"; the text is derived so a deliberate role rename in
// routes.go flows through byte-for-byte.
func apidocRoleNote(roles []string) string {
	titled := make([]string, len(roles))
	for i, r := range roles {
		titled[i] = strings.ToUpper(r[:1]) + r[1:]
	}
	return "Requires the " + strings.Join(titled, " or ") + " role."
}

// apidocSkipPublicPrefixes are the routes with no Auth() middleware at all —
// a PAT has no meaning on them (Authorization is either ignored, or for the
// deploy-hook/webhook routes not even a bearer credential at all), so there
// is no scope/session story to document. Small and stable: a new PUBLIC
// route is a rare, deliberate, security-relevant decision that gets scrutiny
// on its own, unlike the destroy/reveal boundaries this project has actually
// gotten wrong twice.
var apidocSkipPublicPrefixes = []string{
	"/healthz",
	"/api/auth/login",
	"/api/auth/login/verify",
	"/api/auth/setup",
	"/api/features",
	"/api/version",
	"/api/auth/refresh",
	"/api/auth/forgot-password",
	"/api/auth/reset-password",
	"/api/auth/invitation",
	"/api/auth/accept-invitation",
	"/api/webhooks/push",
	"/api/git/webhooks/",
	"/api/webhooks/deploy/",
	"/api/git/providers/github/manifest/callback",
	"/api/git/integrations/callback",
}

func apidocIsPublic(path string) bool {
	for _, p := range apidocSkipPublicPrefixes {
		if path == p || strings.HasPrefix(path, p) {
			return true
		}
	}
	return false
}

// apidocSkipNonRESTPrefixes are authenticated routes deliberately excluded
// from the generated reference because they are not REST resources — unlike
// apidocSkipPublicPrefixes, these are NOT unauthenticated, so they must not
// be added there (that list's meaning is specifically "no Auth() at all",
// and apidocSecurity derives real security documentation from it elsewhere).
//
// /mcp is a single stateless JSON-RPC-over-HTTP endpoint (see
// internal/mcpserver): its handler has no json.Decode/writeJSON call for the
// static type extractor to resolve (the request/response shape is per-tool,
// decided at the JSON-RPC layer, not a fixed Go struct), so documenting it
// as one POST operation would show an opaque, meaningless body. Its tools,
// not this route, are what need documenting — and MCP tool descriptions
// aren't OpenAPI operations.
var apidocSkipNonRESTPrefixes = []string{
	"/mcp",
}

func apidocIsNonREST(path string) bool {
	for _, p := range apidocSkipNonRESTPrefixes {
		if path == p || strings.HasPrefix(path, p) {
			return true
		}
	}
	return false
}

// apidocFuncName returns the short name of the function a value wraps —
// reliable for this because Go compiles one machine-code body per closure
// literal in the source, shared across every instantiation; the returned
// symbol name is the declaring function's, not per-call-site.
func apidocFuncName(v any) string {
	rv := reflect.ValueOf(v)
	if rv.Kind() != reflect.Func {
		return ""
	}
	full := runtime.FuncForPC(rv.Pointer()).Name()
	// "github.com/weiliang79/belune/internal/handler.(*Handler).GetProject-fm"
	// -> "GetProject". The "-fm" suffix marks a method value; trim it too.
	full = strings.TrimSuffix(full, "-fm")
	if i := strings.LastIndex(full, "."); i >= 0 {
		full = full[i+1:]
	}
	return full
}

// apidocMiddlewareContains reports whether any middleware in the chain was
// declared by (i.e. is a closure returned from) the named function — matched
// on the declaring function's own name, e.g. "RequireRole", regardless of
// package-path prefix or closure suffix.
func apidocMiddlewareContains(mws []func(http.Handler) http.Handler, declaredBy string) bool {
	for _, mw := range mws {
		name := runtime.FuncForPC(reflect.ValueOf(mw).Pointer()).Name()
		if strings.Contains(name, "."+declaredBy+".") {
			return true
		}
	}
	return false
}

// apidocZeroScopeToken inserts a token with a non-nil, empty scopes array
// directly via the queries layer — POST /api/tokens itself now rejects an
// empty scopes array (TestCreateAPIToken_RejectsEmptyScopes), so the public
// API can't mint this one. Same bypass createPinnedAPIToken already uses for
// pinned tokens; not a new pattern.
func apidocZeroScopeToken(t *testing.T, userID string) string {
	t.Helper()
	var uid pgtype.UUID
	require.NoError(t, uid.Scan(userID))

	plain, hash, err := service.GenerateToken()
	require.NoError(t, err)

	_, err = env.Queries.CreateAPIToken(context.Background(), generated.CreateAPITokenParams{
		UserID:      uid,
		Name:        "apidoc-zero-scope-probe",
		TokenHash:   hash,
		Scopes:      []string{},
		RoleAtIssue: "admin",
	})
	require.NoError(t, err)
	return plain
}

// substituteDummyIDs replaces every {param} segment with a well-formed but
// nonexistent UUID — the same technique destroy_boundary_test.go and
// reveal_boundary_test.go already use, so an id-having route 404s cleanly
// instead of touching a real row.
func substituteDummyIDs(route string) string {
	path := route
	for _, seg := range strings.Split(route, "/") {
		if strings.HasPrefix(seg, "{") {
			path = strings.Replace(path, seg, "00000000-0000-0000-0000-000000000000", 1)
		}
	}
	return path
}

// mustAuthMe fetches the caller's own user record via the one endpoint that
// doesn't need an id to look itself up.
func mustAuthMe(t *testing.T, sessionToken string) map[string]any {
	t.Helper()
	resp := env.DoRequest(t, "GET", "/api/auth/me", nil, testutil.AuthHeader(sessionToken))
	require.Equal(t, http.StatusOK, resp.StatusCode)
	return testutil.ReadJSON(t, resp)
}

func TestGenerateAPIReference(t *testing.T) {
	if os.Getenv("GENERATE_API_REFERENCE") != "1" {
		t.Skip("generation tool, not a correctness test — set GENERATE_API_REFERENCE=1 to run")
	}
	resetDB(t)

	adminToken := env.SetupAdmin(t, "apidoc-admin@test.com", "password123")
	adminUserID := extractID(mustAuthMe(t, adminToken)["id"])

	zeroScope := apidocZeroScopeToken(t, adminUserID)

	router, ok := env.Server.Config.Handler.(chi.Routes)
	require.True(t, ok, "test server handler must be walkable as chi.Routes")

	// The role set every RequireRole call in routes.go names — reflection
	// tells us RequireRole is in a chain, this tells us which roles.
	roleSet := apidocRequireRoleSet(t)

	var routes []apidocRoute
	err := chi.Walk(router, func(method, route string, handler http.Handler, mws ...func(http.Handler) http.Handler) error {
		// "/*" is chi's own internal NotFound/MethodNotAllowed fallback, not
		// an application route — it never reaches Auth() at all, which is
		// exactly why it shows up 200 for every method with no scope check.
		if route == "/*" || apidocIsPublic(route) || apidocIsNonREST(route) {
			return nil
		}

		row := apidocRoute{
			Method:  method,
			Path:    route,
			Handler: apidocFuncName(handler),
			Pinned:  strings.Contains(route, "{projectId}"),
			Session: apidocMiddlewareContains(mws, "RequireSession"),
		}
		if apidocMiddlewareContains(mws, "RequireRole") {
			row.Roles = roleSet
		}

		path := substituteDummyIDs(route)

		// Zero-scope probe. Safe on every route — the handler never runs,
		// since an empty scopes slice can't satisfy any RequireScope check.
		resp := env.DoRequest(t, method, path, nil, testutil.AuthHeader(zeroScope))
		status1 := resp.StatusCode
		msg := apidocErrorMessage(t, resp)
		switch {
		case strings.HasPrefix(msg, "token lacks required scope: "):
			row.Scope = strings.TrimPrefix(msg, "token lacks required scope: ")
		case msg == "this action requires a session, not a personal access token":
			// No RequireScope* sits in front of RequireSession on this route
			// (e.g. the WS terminal tunnel) — a PAT is rejected before scope
			// ever comes into it, at ANY scope including zero. Reading
			// routes.go would have called this "read" or "write" by the
			// blanket default; probing catches that there is no blanket
			// default here at all. row.Session is already true by this point
			// (set structurally above) — this is corroboration, not the
			// source of truth, and the require below turns a mismatch
			// between the two into a loud failure instead of a silently
			// wrong row.
			row.Scope = "none"
			require.True(t, row.Session, "%s %s: zero-scope probe hit the session-only rejection message, but RequireSession wasn't found structurally in its middleware chain", method, route)
		}
		if row.Scope == "" {
			// Every authenticated, non-public route in this codebase sits
			// behind some RequireScope* check — an empty result here means
			// the zero-scope probe was rejected for a DIFFERENT reason (or
			// wasn't rejected at all), which is itself worth surfacing
			// loudly rather than silently emitting an incomplete row.
			row.Scope = fmt.Sprintf("UNRECOGNIZED (%d %q)", status1, msg)
		}

		routes = append(routes, row)
		return nil
	})
	require.NoError(t, err)

	sort.Slice(routes, func(i, j int) bool {
		if routes[i].Path != routes[j].Path {
			return routes[i].Path < routes[j].Path
		}
		return routes[i].Method < routes[j].Method
	})

	t.Logf("apidoc: probed %d routes", len(routes))
	sessionRoutes := 0
	for _, r := range routes {
		if strings.HasPrefix(r.Scope, "UNRECOGNIZED") {
			t.Errorf("apidoc: %s %s — %s", r.Method, r.Path, r.Scope)
		}
		// RequireSession rejects every PAT unconditionally (see
		// middleware.RequireSession) — a session-gated route's emitted
		// security must never list a scope scheme (metrics/read/deploy/write)
		// as an alternative, or the spec documents an escalation that doesn't
		// exist. This is the invariant a peer review found broken for exactly
		// the three highest-stakes routes it could be broken for (POST
		// /api/tokens, POST /api/users, POST /api/users/invite) — checked
		// directly against the real router's middleware chain, not re-derived.
		if r.Session {
			sessionRoutes++
			for _, req := range apidocSecurity(r) {
				for scheme := range req {
					if apidocScopeScheme(scheme) != "" {
						t.Errorf("apidoc: %s %s is RequireSession-gated but its emitted security still lists %q — a personal access token would be wrongly documented as able to call it", r.Method, r.Path, scheme)
					}
				}
			}
		}
	}
	// Pinned, not a floor: 26 (destroy/restore + reveal + terminal +
	// mint/revoke-token routes, all pre-existing) + 9 (GET /api/tokens plus
	// the account/auth/TOTP routes gated afterward — a PAT manages
	// infrastructure, not the account itself; see routes.go). Was 38 with
	// GET/PUT /api/account/alert-preferences also gated — reverted (see
	// routes.go): alert preferences are infrastructure config, not account
	// security, and gating them bought no credential exposure or takeover
	// protection. Then 36 -> 35 when DELETE /api/certificates/{certificateId}
	// was un-gated (see routes.go): domains.certificate_id is ON DELETE
	// RESTRICT so only an unused cert is reachable, the route still requires
	// admin + write, and a cert is re-uploadable — the "unrecoverable stored
	// data" argument the session gate rested on doesn't hold for it, unlike
	// the rest of the destroy/restore set. Then 35 -> 42 when platform
	// configuration was gated (see routes.go): settings, SMTP config, service
	// restart, and the host shell — ListSettings/UpdateSettings, the three
	// SMTP endpoints, RestartService, CreateHostShellSession. Not a leaked
	// credential this time (GetSMTPSettings already masks the password,
	// ListSettings already skips it) — the reason is UpdateSettings writes
	// ANY key by name, and host_shell_enabled is one of those keys; a PAT
	// should never reach the switch that turns on the host shell.
	// GET /api/maintenance/server-ip stayed PAT-callable — a public fact, not
	// configuration. Then 42 -> 44 when PUT .../transfer and PUT .../sharing
	// were gated (see routes.go and grant_boundary_test.go): both hand out
	// project-owner-equivalent access — sharing extends it to every Member
	// with no membership check, transfer moves it outright — the same
	// administering-who-can-reach-what class as POST /api/users. Then
	// 44 -> 45 when POST /api/maintenance/update was added (v0.1.8
	// self-update, Phase 2): triple-gated the same way as the host shell it
	// sits beside — a token should never reach even the "password required"
	// response for a route that ends with the control-plane container being
	// replaced. This constant is deliberately edited only after watching the
	// test fail at the old value first (confirmed each time: 38 -> 36, 36 ->
	// 35, 35 -> 42, 42 -> 44, then 44 -> 45, and 47 -> 49 when PUT and DELETE
	// /api/quotas/{scope}/{scopeId} were re-gated — a quota decides whether a
	// project can deploy at all, so capping one is an operator decision, not
	// something a leaked token should reach), the same way a floor assertion is
	// meant to be moved. If this moves again, a route was added, removed, or
	// re-gated — worth an explicit look either way, the same reasoning
	// destroy_boundary_test.go and reveal_boundary_test.go's own sanity floors
	// give for their sets.
	require.Equal(t, 49, sessionRoutes, "expected exactly 49 RequireSession routes")

	sig, directives, behaviour, reg := apidocExtractTypes(t)
	apidocAssertEmbedsPromoted(t, reg)

	// Fold in what the handler bodies revealed. Done here rather than inside
	// the probe loop because the probe reads the ROUTER and this reads the
	// SOURCE — two different questions about the same route, kept separable.
	for i, r := range routes {
		b, ok := behaviour[r.Handler]
		if !ok {
			continue
		}
		routes[i].Query = b.Query
		// Only interesting where RequirePinnedProjectMatch is blind: with a
		// {projectId} in the path the middleware already enforces the pin and
		// r.Pinned says so.
		routes[i].HandlerPinned = b.Pinned && !r.Pinned
	}

	apidocAssertBehaviourExtracted(t, routes)

	doc := apidocBuildDocument(t, routes, sig, directives, reg)
	apidocWriteSpec(t, doc)
	// Per-domain site pages are generated separately, by
	// apps/site/scripts/generate-api-pages.mjs (fumadocs-openapi's own
	// generateFiles, run against this file) — see task generate:api-docs,
	// which runs both in sequence. Writing them from Go, guessing at
	// <APIPage>'s prop shape by hand, was tried first and abandoned: the
	// library's own generator wires the exact props (payload, operations,
	// webhooks) and imports correctly, which reading .d.ts files alone did
	// not reliably predict.
}

// apidocDomain is one output page: a title plus the rows that classified
// into it, in probe order (already path-sorted by the caller). Used both to
// tag each operation (operation.Tags) and to group the per-domain stub .mdx
// pages fumadocs-openapi's <APIPage> renders from.
type apidocDomain struct {
	Slug, Title string
	Rows        []apidocRoute
}

// apidocDomainsPath is relative to this package's directory, mirroring
// apidocSpecPath's own reach into apps/site — this is the single source
// both this generator and apps/site/scripts/generate-api-pages.mjs read
// for domain slugs/tags/sidebar titles, replacing what used to be two
// independently hand-maintained lists (this file's own apidocDomainOrder,
// and the JS script's SLUG_BY_TITLE) that agreed only by discipline, not
// by construction.
const apidocDomainsPath = "../../../site/api-domains.json"

// apidocDomainEntry mirrors one node of apidocDomainsPath's JSON tree. A
// leaf (Tag non-empty) names a real OpenAPI tag; a container entry (Tag
// empty, Children non-empty — "git" is the only one today) exists purely
// for sidebar nesting on the site side, and apidocLoadDomains flattens
// through it, so this generator only ever sees leaves. Recursive, not
// hard-limited to one level, even though only "git" nests today.
//
// A Tag ending " (Admin)" is load-bearing: TestGenerateAPIReference's role
// invariant checks every route grouped under such a tag is actually
// RequireRole-gated (it's the one role signal not derived from r.Roles).
// The suffix stays literally "(Admin)" even if the role set is renamed —
// it's a grouping label, not derived text. The JSON file can't say any of
// this itself (no comments).
type apidocDomainEntry struct {
	Slug     string              `json:"slug"`
	Tag      string              `json:"tag,omitempty"`
	Title    string              `json:"title,omitempty"`
	Children []apidocDomainEntry `json:"children,omitempty"`
}

// apidocLoadDomains reads apidocDomainsPath and flattens it into the
// ordered (slug, tag) pairs apidocGroupByDomain has always worked with —
// apidocDomainOrder's old literal shape, now sourced from a file the site
// side reads too instead of a second hand-copy. A nested entry's own slug
// is prefixed by its ancestors' ("git/providers"), matching the
// folder-path convention generate-api-pages.mjs already uses — one
// slug-addressing scheme, not two that could drift apart. A slug named by
// a //apidoc:tag directive but missing here still panics in
// apidocGroupByDomain, same as always — getting THIS file wrong (a typo, a
// removed entry still referenced) is the one way that panic still fires;
// a route with no directive at all fails a different, permanent check
// instead (see apidocBuildDocument's uncategorized-must-be-empty
// assertion) now that a directive is the only way a route is classified.
func apidocLoadDomains(t *testing.T) []struct{ slug, title string } {
	t.Helper()
	raw, err := os.ReadFile(apidocDomainsPath)
	require.NoError(t, err, "reading %s", apidocDomainsPath)
	var entries []apidocDomainEntry
	require.NoError(t, json.Unmarshal(raw, &entries))

	var out []struct{ slug, title string }
	// slugByTag enforces tag uniqueness. Two entries may legitimately share
	// neither slug nor tag, but a REPEATED tag is always a mistake: the tag
	// is the join key between the two halves of this pipeline — Go groups
	// operations by it, and generate-api-pages.mjs maps it back to a folder
	// slug through a Map keyed on it. A duplicate silently resolves to
	// whichever entry is written last, so operations land in one folder and
	// the other renders empty, with no error anywhere. It is also invalid
	// OpenAPI: the spec's `tags` array would carry the name twice.
	slugByTag := map[string]string{}
	var walk func(prefix string, nodes []apidocDomainEntry)
	walk = func(prefix string, nodes []apidocDomainEntry) {
		for _, n := range nodes {
			slug := n.Slug
			if prefix != "" {
				slug = prefix + "/" + slug
			}
			if n.Tag != "" {
				if prev, dup := slugByTag[n.Tag]; dup {
					require.Failf(t, "duplicate tag in "+apidocDomainsPath,
						"tag %q is declared by both %q and %q — a tag names exactly one domain, and the duplicate would silently resolve to whichever is written last", n.Tag, prev, slug)
				}
				slugByTag[n.Tag] = slug
				out = append(out, struct{ slug, title string }{slug, n.Tag})
			}
			walk(slug, n.Children)
		}
	}
	walk("", entries)
	return out
}

// apidocRouteDomain resolves a route's domain slug from its handler's
// //apidoc:tag directive — the ONLY source now. apidocClassify, the
// path/scope-based priority list this replaced (25 ordered cases where
// position was load-bearing — "/terminal" only worked because it sat above
// "/applications" and "/api/projects" — plus scope/role-derived cases
// that existed only because nothing declared a tag), is retired entirely,
// not just unused: a directive removes the failure mode, so keeping the
// old heuristic around as a fallback would have kept it too. A handler
// with no directive (or one this run's probe never reached, e.g. a route
// added but not yet wired up) falls back to "uncategorized" — the
// permanent check in apidocBuildDocument fails generation if that bucket
// is ever non-empty, so this fallback is a loud failure waiting to happen,
// not a silent one.
func apidocRouteDomain(directives map[string]apidocDirective, r apidocRoute) string {
	d, ok := directives[r.Handler]
	if !ok || d.Tag == "" {
		return "uncategorized"
	}
	return d.Tag
}

func apidocGroupByDomain(t *testing.T, routes []apidocRoute, domains []struct{ slug, title string }, directives map[string]apidocDirective) []apidocDomain {
	t.Helper()
	bySlug := make(map[string]*apidocDomain, len(domains))
	for _, o := range domains {
		bySlug[o.slug] = &apidocDomain{Slug: o.slug, Title: o.title}
	}
	for _, r := range routes {
		slug := apidocRouteDomain(directives, r)
		d, ok := bySlug[slug]
		if !ok {
			panic(fmt.Sprintf("apidocRouteDomain returned %q (from a //apidoc:tag directive), which is not in %s", slug, apidocDomainsPath))
		}
		d.Rows = append(d.Rows, r)
	}
	var out []apidocDomain
	for _, o := range domains {
		d := *bySlug[o.slug]
		if len(d.Rows) > 0 {
			out = append(out, d)
		}
	}
	return out
}

// === Static extraction: request/response types via golang.org/x/tools/go/packages ===
//
// apidocSchema is a JSON Schema node, kept as a plain map rather than a rigid
// struct because JSON Schema's shape genuinely varies (object/array/$ref/
// primitive) — a struct would need a field for every keyword combination.
type apidocSchema map[string]any

// apidocOperationTypes holds one handler function's request/response schemas,
// as resolved by apidocExtractTypes. Keyed by handler function name (the same
// short name apidocFuncName recovers from the running router via reflection),
// so the static (types) and dynamic (probe) halves of this generator join on
// a name both sides compute the same way.
type apidocOperationTypes struct {
	Request   apidocSchema         // nil if the handler decodes no body
	Responses map[int]apidocSchema // status code -> response body schema
}

// apidocSchemaRegistry accumulates named component schemas (components.schemas
// in the emitted spec) as apidocWalkType resolves named struct types. 100
// named structs are reachable from only 94 distinct root response types, so
// defining each once and referencing it by $ref avoids duplicating shared
// types (most visibly generated.Database and friends) at every call site.
type apidocSchemaRegistry struct {
	schemas    map[string]apidocSchema
	inFlight   map[string]bool         // guards self-referential types against infinite recursion
	namedTypes map[string]*types.Named // every distinct named type walkType has resolved, special-cased or not — see recordNamed and TestAPIDocAllMarshalerTypesHandled
}

func newApidocSchemaRegistry() *apidocSchemaRegistry {
	return &apidocSchemaRegistry{schemas: map[string]apidocSchema{}, inFlight: map[string]bool{}, namedTypes: map[string]*types.Named{}}
}

// walkType resolves a Go static type into a JSON Schema node. Six traps —
// four anticipated before this was written (see the top-of-file design note
// and project_api_reference_generator_design.md), a fifth ([]byte/
// json.RawMessage) found only by actually running the generator and reading
// its output, and a sixth (time.Time) found only by a peer review's
// go/types sweep of every response-surface type implementing
// json.Marshaler — exactly the argument this whole project makes for
// generation over hand-writing:
//
//  1. pgtype values implement json.Marshaler and marshal to a bare nullable
//     scalar — naive reflection would emit their internal struct shape
//     ({"String":"x","Valid":true}), which is wrong. Checked before any
//     other case, by exact type string, from a table verified by actually
//     marshaling each of the four pgtype types this response surface uses.
//  2. Embedded-struct promotion — handled in buildObjectSchema, not here.
//  3. map[string]any roots — handled by the AST-literal path in
//     apidocResolveResponseExpr, not here; by the time a map[string]any
//     reaches walkType (e.g. built from a variable, not an inline literal),
//     its static type carries no field information, so it becomes a
//     generic-but-honest {"type":"object","additionalProperties":{}}.
//  4. interface{} leaves are genuinely opaque — emitted as {} (unconstrained
//     JSON Schema), not guessed at.
//  5. encoding/json special-cases []byte (base64 STRING on the wire, not an
//     array of small integers — the generic slice rule would say otherwise)
//     and separately special-cases json.RawMessage (passed through as
//     embedded JSON verbatim, neither base64 nor an array). First generated
//     spec had 15 fields wrong this way (credentials_encrypted,
//     advanced_config, notification channel configs, audit log details, …) —
//     caught by reading the actual output, not by design review.
//  6. time.Time has only unexported fields (wall, ext, loc), so naive
//     reflection sees an empty struct — but it implements json.Marshaler and
//     marshals to an RFC3339 string, and the zero value marshals to a real
//     string ("0001-01-01T00:00:00Z"), never null; only *time.Time is
//     nullable, via the existing types.Pointer case below. Verified
//     empirically by a peer review, the same standard this file holds
//     itself to elsewhere — see TestAPIDocAllMarshalerTypesHandled, which
//     turns that review into a standing guard against a seventh.
func (reg *apidocSchemaRegistry) walkType(t types.Type) apidocSchema {
	if named, ok := t.(*types.Named); ok {
		reg.namedTypes[named.String()] = named // recorded before the special-case switch below, so pgtype/time.Time/json.RawMessage land here too, not just plain domain structs
	}
	switch t.String() {
	case "github.com/jackc/pgx/v5/pgtype.UUID":
		return apidocSchema{"type": []string{"string", "null"}, "format": "uuid"}
	case "github.com/jackc/pgx/v5/pgtype.Timestamptz":
		return apidocSchema{"type": []string{"string", "null"}, "format": "date-time"}
	case "github.com/jackc/pgx/v5/pgtype.Text":
		return apidocSchema{"type": []string{"string", "null"}}
	case "github.com/jackc/pgx/v5/pgtype.Int4":
		return apidocSchema{"type": []string{"integer", "null"}}
	case "time.Time":
		return apidocSchema{"type": "string", "format": "date-time"}
	case "encoding/json.RawMessage":
		// Passed through verbatim by json.RawMessage's own MarshalJSON — it
		// IS already-valid JSON on the wire, not a byte string. Genuinely
		// arbitrary shape (that's the whole point of using RawMessage), same
		// honest {} as an interface{} leaf.
		return apidocSchema{"description": "arbitrary JSON — see the handler for the exact shape"}
	}
	if strings.HasPrefix(t.String(), "github.com/jackc/pgx/v5/pgtype.") {
		// A pgtype value outside the four verified above — every pgtype type
		// implements json.Marshaler with its own wire shape, so guessing at
		// its struct fields would very likely be wrong. Fail loud instead:
		// this shows up as a visible schema description, not a silent
		// mis-emission, and names exactly what needs adding to the table.
		return apidocSchema{"description": "UNHANDLED pgtype: " + t.String() + " — add to the lookup table in apidoc_generate_test.go's walkType"}
	}
	if slice, ok := t.Underlying().(*types.Slice); ok {
		if b, ok := slice.Elem().Underlying().(*types.Basic); ok && b.Kind() == types.Uint8 {
			// []byte (including any named type underlain by it) — base64
			// string per encoding/json's default encoding, not an array.
			return apidocSchema{"type": "string", "format": "byte"}
		}
	}

	switch u := t.Underlying().(type) {
	case *types.Pointer:
		return apidocNullable(reg.walkType(u.Elem()))
	case *types.Slice:
		return apidocSchema{"type": "array", "items": reg.walkType(u.Elem())}
	case *types.Array:
		return apidocSchema{"type": "array", "items": reg.walkType(u.Elem())}
	case *types.Map:
		return apidocSchema{"type": "object", "additionalProperties": reg.walkType(u.Elem())}
	case *types.Struct:
		if named, ok := t.(*types.Named); ok {
			return reg.registerNamed(named, u)
		}
		return reg.buildObjectSchema(u) // anonymous struct literal type (rare) — inline it
	case *types.Basic:
		return apidocBasicSchema(u)
	case *types.Interface:
		return apidocSchema{} // genuinely opaque — trap #4, no static shape to report
	default:
		return apidocSchema{"description": "unhandled Go type shape: " + t.String()}
	}
}

// apidocNullable wraps a schema to also permit null, the JSON Schema 2020-12
// (OpenAPI 3.1) way — a "type" array gains "null"; a $ref (which JSON Schema
// allows sibling keywords for as of 2020-12, but wrapping is the more widely
// understood idiom) becomes an anyOf against {"type":"null"}.
func apidocNullable(s apidocSchema) apidocSchema {
	if ref, ok := s["$ref"]; ok {
		return apidocSchema{"anyOf": []apidocSchema{{"$ref": ref}, {"type": "null"}}}
	}
	switch v := s["type"].(type) {
	case string:
		s["type"] = []string{v, "null"}
	case []string:
		for _, x := range v {
			if x == "null" {
				return s
			}
		}
		s["type"] = append(v, "null")
	}
	return s // no "type" key at all (e.g. {} for interface{}) already permits null
}

// registerNamed resolves a named struct type, registering it into the
// component schema registry at most once (self-referential and repeatedly-
// reached types both short-circuit via inFlight/the existing entry) and
// returning a $ref to it.
func (reg *apidocSchemaRegistry) registerNamed(named *types.Named, u *types.Struct) apidocSchema {
	name := named.Obj().Name()
	ref := apidocSchema{"$ref": "#/components/schemas/" + name}
	if reg.inFlight[name] {
		return ref
	}
	if _, ok := reg.schemas[name]; ok {
		return ref
	}
	reg.inFlight[name] = true
	schema := reg.buildObjectSchema(u)
	delete(reg.inFlight, name)
	reg.schemas[name] = schema
	return ref
}

// apidocAssertEmbedsPromoted closes the embedded-promotion class for good.
//
// encoding/json promotes an embedded struct's exported fields onto the parent,
// and this generator has gotten that wrong TWICE: first by emitting a nested
// object that doesn't exist on the wire (trap #2, databaseResponse), then by
// dropping unexported embeds whole because the !f.Exported() test ran before
// the f.Embedded() branch — silently truncating five schemas by 43 fields, a
// defect invisible until someone happened to ask what one of those responses
// actually looked like.
//
// Both were fixes to buildObjectSchema. Neither left anything behind that
// would catch a third variant. This does: for every named struct the registry
// resolved, every promoting embed's properties must ALSO appear on the parent.
// It reads the finished schemas rather than re-deriving them, so it cannot
// share a bug with the walk it checks — the same reasoning as
// TestAPIDocAllMarshalerTypesHandled, which closed the custom-marshaler class
// after time.Time was found the same accidental way.
func apidocAssertEmbedsPromoted(t *testing.T, reg *apidocSchemaRegistry) {
	t.Helper()

	// Snapshot first: propertiesOf -> walkType can insert into namedTypes, and
	// ranging a map while writing it leaves the new entries' visitation
	// undefined.
	type entry struct {
		name  string
		named *types.Named
	}
	snapshot := make([]entry, 0, len(reg.namedTypes))
	for k, v := range reg.namedTypes {
		snapshot = append(snapshot, entry{k, v})
	}
	sort.Slice(snapshot, func(i, j int) bool { return snapshot[i].name < snapshot[j].name })

	var missing []string
	for _, e := range snapshot {
		st, ok := e.named.Underlying().(*types.Struct)
		if !ok {
			continue
		}
		parentProps, ok := reg.propertiesOf(reg.walkType(e.named))
		if !ok {
			continue // not an object schema (a special-cased marshaler type)
		}
		for i := 0; i < st.NumFields(); i++ {
			f := st.Field(i)
			if !f.Embedded() || !apidocPromotesFields(f.Type()) {
				continue
			}
			// A json-tagged embed NESTS under that name instead of promoting,
			// so it is correctly absent from the parent's own properties.
			if _, tagged := reflect.StructTag(st.Tag(i)).Lookup("json"); tagged {
				continue
			}
			embeddedProps, ok := reg.propertiesOf(reg.walkType(f.Type()))
			if !ok {
				continue
			}
			for k := range embeddedProps {
				if _, present := parentProps[k]; !present {
					missing = append(missing, fmt.Sprintf(
						"%s: missing %q, which encoding/json promotes from embedded %s",
						e.named.Obj().Name(), k, f.Type()))
				}
			}
		}
	}
	sort.Strings(missing)
	require.Empty(t, missing,
		"schema(s) omit fields that encoding/json promotes from an embedded struct — buildObjectSchema dropped or nested an embed it should have flattened")
}

// apidocPromotesFields reports whether an embedded field's type is one whose
// exported fields encoding/json promotes into the parent — a struct, or a
// pointer to one. An embedded field of unexported NON-struct type (type myInt
// int) is not marshaled at all, so the distinction is load-bearing, not
// defensive.
func apidocPromotesFields(t types.Type) bool {
	if p, ok := t.Underlying().(*types.Pointer); ok {
		t = p.Elem()
	}
	_, ok := t.Underlying().(*types.Struct)
	return ok
}

// buildObjectSchema walks a struct's exported fields into an object schema.
// Handles trap #2: an embedded field with no json tag is PROMOTED by
// encoding/json — its own fields appear at the parent's top level on the
// wire, not nested under a field named after the embedded type. Naive
// extraction would emit a nested object that doesn't exist
// (databaseResponse embeds generated.Database with no tag; 6 call sites).
//
// ⚠️ An EMBEDDED field's name IS its type's name, so embedding an unexported
// type (appVolumeBackupConfigResponse embeds volumeBackupConfigResponse)
// makes the FIELD unexported — yet encoding/json still promotes that struct's
// exported fields. An earlier version tested !f.Exported() first and dropped
// those embeds whole, silently truncating five schemas by 43 fields between
// them. databaseResponse escaped only because generated.Database happens to
// be exported. Hence the f.Embedded() carve-out below: skip an unexported
// field ONLY when it is not a promoting embed.
func (reg *apidocSchemaRegistry) buildObjectSchema(u *types.Struct) apidocSchema {
	props := apidocSchema{}
	for i := 0; i < u.NumFields(); i++ {
		f := u.Field(i)
		if !f.Exported() && !(f.Embedded() && apidocPromotesFields(f.Type())) {
			continue
		}
		jsonTag, hasJSONTag := reflect.StructTag(u.Tag(i)).Lookup("json")
		name, _, _ := strings.Cut(jsonTag, ",")

		if f.Embedded() && !hasJSONTag {
			if embeddedProps, ok := reg.propertiesOf(reg.walkType(f.Type())); ok {
				for k, v := range embeddedProps {
					props[k] = v
				}
			}
			continue
		}
		if name == "-" {
			continue // explicitly excluded from the wire
		}
		if name == "" {
			name = f.Name() // no json tag at all -> encoding/json uses the Go field name verbatim
		}
		props[name] = reg.walkType(f.Type())
	}
	return apidocSchema{"type": "object", "properties": props}
}

// propertiesOf dereferences a schema back to its "properties" map, following
// a $ref into the registry if needed — used only for embedding-promotion,
// where a named embedded type's fields need to be merged into its parent
// rather than kept as its own nested $ref.
func (reg *apidocSchemaRegistry) propertiesOf(s apidocSchema) (apidocSchema, bool) {
	if ref, ok := s["$ref"].(string); ok {
		target, ok := reg.schemas[strings.TrimPrefix(ref, "#/components/schemas/")]
		if !ok {
			return nil, false // self-referential embedding — pathological, shouldn't occur
		}
		s = target
	}
	props, ok := s["properties"].(apidocSchema)
	return props, ok
}

func apidocBasicSchema(b *types.Basic) apidocSchema {
	switch {
	case b.Info()&types.IsInteger != 0:
		return apidocSchema{"type": "integer"}
	case b.Info()&types.IsFloat != 0:
		return apidocSchema{"type": "number"}
	case b.Info()&types.IsBoolean != 0:
		return apidocSchema{"type": "boolean"}
	case b.Info()&types.IsString != 0:
		return apidocSchema{"type": "string"}
	default:
		return apidocSchema{}
	}
}

// apidocIsHandlerReceiver reports whether a function declaration's receiver
// is `*Handler` — the type every route's handler method is declared on.
func apidocIsHandlerReceiver(expr ast.Expr) bool {
	star, ok := expr.(*ast.StarExpr)
	if !ok {
		return false
	}
	id, ok := star.X.(*ast.Ident)
	return ok && id.Name == "Handler"
}

// apidocConstInt resolves a constant integer expression (an http.StatusOK
// identifier, a literal 429, etc.) via the type-checker's own constant
// folding — works for any of net/http's Status* constants without a
// hand-typed name table, since go/types already resolves what they equal.
func apidocConstInt(info *types.Info, expr ast.Expr) (int, bool) {
	tv, ok := info.Types[expr]
	if !ok || tv.Value == nil {
		return 0, false
	}
	i, ok := constant.Int64Val(tv.Value)
	if !ok {
		return 0, false
	}
	return int(i), true
}

// apidocResolveResponseExpr resolves one writeJSON call's data argument to a
// schema. Trap #3: when the argument is an inline map[string]any{...}
// literal, its STATIC TYPE carries zero field information (every such root
// resolves to the identical "map[string]any") — the actual shape only exists
// in the AST as literal keys and per-value expressions, so this is a second
// code path, not a variation of walkType's struct walk. A map[string]any
// reached through a variable instead (built up across several statements
// before the writeJSON call) falls through to the static-type path and gets
// an honest, generic {"type":"object","additionalProperties":{}} rather than
// a guessed-at shape — a known, deliberate precision limit for that specific
// sub-case, not a silent gap: nothing here claims a wrong shape.
func apidocResolveResponseExpr(reg *apidocSchemaRegistry, info *types.Info, expr ast.Expr) apidocSchema {
	if lit, ok := expr.(*ast.CompositeLit); ok {
		if _, isMapType := lit.Type.(*ast.MapType); isMapType {
			if schema, ok := apidocWalkMapLiteral(reg, info, lit); ok {
				return schema
			}
		}
	}
	t := info.TypeOf(expr)
	if t == nil {
		return apidocSchema{"description": "unresolved response type"}
	}
	return reg.walkType(t)
}

func apidocWalkMapLiteral(reg *apidocSchemaRegistry, info *types.Info, lit *ast.CompositeLit) (apidocSchema, bool) {
	props := apidocSchema{}
	for _, elt := range lit.Elts {
		kv, ok := elt.(*ast.KeyValueExpr)
		if !ok {
			return nil, false
		}
		keyLit, ok := kv.Key.(*ast.BasicLit)
		if !ok {
			return nil, false
		}
		key, err := strconv.Unquote(keyLit.Value)
		if err != nil {
			return nil, false
		}
		valType := info.TypeOf(kv.Value)
		if valType == nil {
			return nil, false
		}
		props[key] = reg.walkType(valType)
	}
	return apidocSchema{"type": "object", "properties": props}, true
}

// apidocDirective is one handler's //apidoc: directive comment. Four keys,
// all optional except tag:
//
//   - tag: the route's domain slug — the ONLY source of a route's domain
//     classification (apidocClassify's old path/scope heuristic is retired,
//     see apidocRouteDomain). Mandatory for every documented handler.
//   - title: sidebar label and operation summary; falls back to
//     apidocTitleFromOperationID when empty.
//   - description: editorial lead sentence; composed with — never replaced
//     by — the derived pinning note (see apidocOperationDescription).
//   - order: an integer sidebar sort weight for this operation within its
//     domain, emitted as the x-belune-order vendor extension and consumed
//     by the site's generate-api-pages.mjs, which re-sorts each domain's
//     page list by it. Order is a *int — nil, and absent from the spec
//     entirely, unless the directive set it: the spec's paths object is a
//     Go map that serializes in key order regardless, so a vendor
//     extension is the only channel a curated per-operation order can
//     reach the site on. Weight semantics and the default value live where
//     they're applied, on the JS side.
//
// Directives are for EDITORIAL / PRESENTATION FACTS ONLY — a name or a
// display order, never a permission. There is deliberately no //apidoc:admin
// or anything asserting scope, session gating, or role: those stay derived
// from the probe and the middleware chain, same as before. A directive is a
// claim, a claim can be wrong, and "docs assert a restriction the code
// doesn't enforce" is the exact failure this generator exists to prevent —
// the same class as the three routes once documented as PAT-callable when
// RequireSession actually rejected them.
type apidocDirective struct {
	Tag, Title, Description string
	Order                   *int // nil unless //apidoc:order set it
}

// apidocDirectivePrefix has NO space after "//" — it's a directive line,
// not prose, so it can sit inside an otherwise ordinary doc comment
// without reading as part of it (CLAUDE.md's "comments explain why, not
// what" is about prose comments; this is closer to //go:generate or
// //nolint:, an established idiom for a machine-read comment).
const apidocDirectivePrefix = "//apidoc:"

// apidocParseDirective scans a doc comment for //apidoc: lines, in any
// position relative to ordinary prose in the same comment group — see the
// package-level doc comment on ServeMetrics in metrics.go for what a real
// one looks like next to prose. A nil doc (no comment at all) returns the
// zero value, which apidocRouteDomain reads as "no directive." Returns an
// error for a malformed directive (today: an //apidoc:order whose value
// isn't an integer) rather than ignoring it — a typo'd sort weight should
// be heard about, not silently treated as "no opinion."
func apidocParseDirective(doc *ast.CommentGroup) (apidocDirective, error) {
	var d apidocDirective
	if doc == nil {
		return d, nil
	}
	for _, c := range doc.List {
		rest, ok := strings.CutPrefix(c.Text, apidocDirectivePrefix)
		if !ok {
			continue
		}
		key, value, _ := strings.Cut(rest, " ")
		switch key {
		case "tag":
			d.Tag = value
		case "title":
			d.Title = value
		case "description":
			d.Description = value
		case "order":
			n, err := strconv.Atoi(strings.TrimSpace(value))
			if err != nil {
				return d, fmt.Errorf("%sorder %q: not an integer", apidocDirectivePrefix, value)
			}
			d.Order = &n
		}
	}
	return d, nil
}

// apidocExtractTypes loads ./internal/handler (this package) via go/packages
// — a full go/types load, the same information `go vet` has — and resolves
// every *Handler method's writeJSON calls and json.Decoder.Decode target to
// a static type, PLUS (new) each method's //apidoc: directive, off the same
// FuncDecl.Doc packages.NeedSyntax already makes available. "." as both Dir
// and pattern: `go test` always runs with the working directory set to the
// package under test, so "." IS apps/api/internal/handler already; no
// absolute path needed.
// apidocAssertBehaviourExtracted pins what body-reading buys, so a refactor
// that quietly breaks the extraction shows up as a failure rather than as a
// reference that silently goes back to documenting nothing.
//
// Called from inside TestGenerateAPIReference rather than as its own Test
// function, deliberately: a separate test can be skipped by a -run pattern on
// the very run that regenerates the spec, and this must not be skippable
// independently of the thing it guards.
func apidocAssertBehaviourExtracted(t *testing.T, routes []apidocRoute) {
	t.Helper()
	byHandler := map[string]apidocRoute{}
	for _, r := range routes {
		byHandler[r.Handler] = r
	}

	has := func(handler, param string) bool {
		for _, q := range byHandler[handler].Query {
			if q == param {
				return true
			}
		}
		return false
	}

	// ⚠️ The two that decide whether data is destroyed. DELETE .../databases/{id}
	// with and without ?delete_backups=true is the keep-or-destroy choice the
	// tombstone work exists to offer, and it was invisible in the reference.
	// These are the reason this extraction is worth having at all.
	assert.True(t, has("DeleteDatabase", "delete_backups"),
		"delete_backups decides whether a deleted database's backups survive — it must be documented")
	assert.True(t, has("DeleteApplicationVolume", "delete_data"),
		"delete_data decides whether a volume's contents are destroyed — it must be documented")

	// Propagation across two hops: ListApplicationLogs reads no query parameter
	// of its own. "session" comes from listContainerLogs (a *Handler method it
	// delegates to) and "limit" from parseLogspagination (a plain package
	// function that one calls) — a handler-bodies-only pass finds neither.
	assert.True(t, has("ListApplicationLogs", "session"),
		"query params must propagate from a delegated *Handler method")
	assert.True(t, has("ListApplicationLogs", "limit"),
		"query params must propagate through a plain helper function too")

	// The indirect spelling. listContainerLogs does q := r.URL.Query() and then
	// q.Get("level"), which a syntax match on r.URL.Query().Get(...) misses
	// entirely — and did, before the check moved to the receiver's TYPE.
	assert.True(t, has("ListDatabaseLogs", "level"),
		"the q := r.URL.Query() spelling must be recognised, not just the chained one")

	// A required parameter is still a parameter: this route 400s without it.
	assert.True(t, has("ListUsableCertificates", "hostname"),
		"a required query parameter must be named even though required-ness is not derived")

	// In-handler pins: routes with no {projectId} for RequirePinnedProjectMatch to
	// read, where the handler enforces the token's pin itself. Every one of
	// these is a real gate the route registration shows nothing of.
	for _, handler := range []string{
		"GetGlobalDeployments", "ListProjects", "CreateProject",
		"InstantiateTemplate", "ListDomainTLSStatus",
	} {
		r, ok := byHandler[handler]
		if !assert.True(t, ok, "%s should be a documented route", handler) {
			continue
		}
		assert.True(t, r.HandlerPinned,
			"%s enforces the project pin in its body — the reference must say so", handler)
		assert.False(t, r.Pinned,
			"%s has no {projectId}; if it grew one this assertion is the wrong shape", handler)
	}

	// Floor, not an exact count: a new route with a query parameter should not
	// have to edit this, but the extraction collapsing to nothing should fail.
	withQuery := 0
	for _, r := range routes {
		if len(r.Query) > 0 {
			withQuery++
		}
	}
	assert.GreaterOrEqual(t, withQuery, 15,
		"query-parameter extraction has collapsed — it found %d routes, and was finding 20", withQuery)
}

// apidocBehaviour is what a handler's BODY reveals that its route registration
// cannot: the query parameters it reads, and whether it enforces the token's
// project pin itself.
type apidocBehaviour struct {
	Query  []string // query parameter names, sorted
	Pinned bool     // calls middleware.TokenProjectsFromContext/TokenPinnedFromContext/PinAllows
}

// apidocExtractBehaviour walks every function in the package — not only route
// handlers — and propagates what it finds up through call edges.
//
// Walking everything is the point. The reads are not all in handlers: limit and
// offset live in parsePagination/parseLogspagination, and listContainerLogs
// holds six more on behalf of four routes that delegate to it. A pass limited to
// handler bodies would document the endpoints that happen to inline their
// parsing and silently miss the ones factored properly.
//
// Both syntactic forms are caught by ONE rule, via the type checker rather than
// the syntax: r.URL.Query().Get("x") and the q := r.URL.Query(); q.Get("x")
// spelling both end in a .Get on a net/url.Values, so matching the RECEIVER's
// type covers both and any third spelling someone writes later. Matching syntax
// instead would have missed the second form — and did, on the first attempt.
//
// ⚠️ Only literal keys are recovered. A computed key cannot be named in a
// document, and guessing one would be worse than the silence it replaces.
func apidocExtractBehaviour(pkg *packages.Package, handlerMethods map[string]bool) map[string]apidocBehaviour {
	type node struct {
		query   map[string]bool
		pinned  bool
		callees []string
	}
	nodes := map[string]*node{}

	for _, file := range pkg.Syntax {
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			n := &node{query: map[string]bool{}}
			ast.Inspect(fn.Body, func(x ast.Node) bool {
				call, ok := x.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok {
					// A bare call — a package-level helper such as
					// parsePagination(r). Record the edge so its reads
					// propagate to whoever calls it.
					if id, ok := call.Fun.(*ast.Ident); ok {
						n.callees = append(n.callees, id.Name)
					}
					return true
				}
				switch {
				case sel.Sel.Name == "Get" && len(call.Args) == 1 && apidocIsURLValues(pkg.TypesInfo, sel.X):
					if lit, ok := apidocStringLit(call.Args[0]); ok {
						n.query[lit] = true
					}
				case sel.Sel.Name == "TokenProjectsFromContext", sel.Sel.Name == "TokenPinnedFromContext", sel.Sel.Name == "PinAllows":
					n.pinned = true
				}
				// h.someOtherHandler(...) — the delegate edge the response
				// extractor already relies on, reused here.
				if id, ok := sel.X.(*ast.Ident); ok && id.Name == "h" && handlerMethods[sel.Sel.Name] {
					n.callees = append(n.callees, sel.Sel.Name)
				}
				return true
			})
			nodes[fn.Name.Name] = n
		}
	}

	// Fixed-point propagation along call edges, same shape as the response
	// merge below. Bounded rather than recursive so a cycle cannot hang the
	// generator; depth 2 is all the codebase needs today (route ->
	// listContainerLogs -> parseLogspagination).
	for range 5 {
		changed := false
		for _, n := range nodes {
			for _, callee := range n.callees {
				c, ok := nodes[callee]
				if !ok {
					continue
				}
				for k := range c.query {
					if !n.query[k] {
						n.query[k] = true
						changed = true
					}
				}
				if c.pinned && !n.pinned {
					n.pinned = true
					changed = true
				}
			}
		}
		if !changed {
			break
		}
	}

	out := map[string]apidocBehaviour{}
	for name, n := range nodes {
		if len(n.query) == 0 && !n.pinned {
			continue
		}
		keys := make([]string, 0, len(n.query))
		for k := range n.query {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		out[name] = apidocBehaviour{Query: keys, Pinned: n.pinned}
	}
	return out
}

// apidocIsURLValues reports whether an expression is a net/url.Values, which is
// what both spellings of a query lookup call .Get on.
func apidocIsURLValues(info *types.Info, expr ast.Expr) bool {
	tv := info.TypeOf(expr)
	if tv == nil {
		return false
	}
	named, ok := tv.(*types.Named)
	if !ok {
		return false
	}
	obj := named.Obj()
	return obj != nil && obj.Name() == "Values" && obj.Pkg() != nil && obj.Pkg().Path() == "net/url"
}

// apidocStringLit unwraps a plain string literal, or reports false for anything
// computed.
func apidocStringLit(expr ast.Expr) (string, bool) {
	lit, ok := expr.(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return "", false
	}
	v, err := strconv.Unquote(lit.Value)
	if err != nil {
		return "", false
	}
	return v, true
}

func apidocExtractTypes(t *testing.T) (map[string]apidocOperationTypes, map[string]apidocDirective, map[string]apidocBehaviour, *apidocSchemaRegistry) {
	t.Helper()
	cfg := &packages.Config{
		Mode: packages.NeedName | packages.NeedFiles | packages.NeedSyntax | packages.NeedTypes |
			packages.NeedTypesInfo | packages.NeedDeps | packages.NeedImports,
		Dir: ".",
	}
	pkgs, err := packages.Load(cfg, ".")
	require.NoError(t, err)
	require.Len(t, pkgs, 1, "expected exactly one package for \".\"")
	pkg := pkgs[0]
	require.Empty(t, pkg.Errors, "static extraction requires a clean compile: %v", pkg.Errors)

	// First pass: every *Handler method name, so a call like h.listContainerLogs(...)
	// can be recognized as a delegate to ANOTHER handler method (as opposed to a
	// call to h.queries.X or an unrelated helper) — found necessary only by
	// running the generator: several route handlers (e.g. ListApplicationLogs)
	// have no writeJSON/Decode call of their own at all, delegating entirely to
	// a shared, non-route *Handler method that does.
	handlerMethods := map[string]bool{}
	for _, file := range pkg.Syntax {
		for _, decl := range file.Decls {
			if fn, ok := decl.(*ast.FuncDecl); ok && fn.Recv != nil && len(fn.Recv.List) == 1 && apidocIsHandlerReceiver(fn.Recv.List[0].Type) {
				handlerMethods[fn.Name.Name] = true
			}
		}
	}

	// Second pass: behavioural facts the ROUTER cannot show, read from the
	// handler bodies this extractor already walks for types.
	//
	// Both are things the CODE enforces, which is the line this generator draws
	// between deriving and declaring. A query parameter is read or it isn't; a
	// pin is enforced or it isn't. (Prose in a Go doc comment is neither — it is
	// written for a Go reader and nothing makes it true of the API, which is why
	// descriptions stay an explicit //apidoc:description and are not promoted
	// from doc comments.)
	behaviour := apidocExtractBehaviour(pkg, handlerMethods)

	reg := newApidocSchemaRegistry()
	result := map[string]apidocOperationTypes{}
	directives := map[string]apidocDirective{}
	delegates := map[string][]string{}

	for _, file := range pkg.Syntax {
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil || fn.Recv == nil || len(fn.Recv.List) != 1 {
				continue
			}
			if !apidocIsHandlerReceiver(fn.Recv.List[0].Type) {
				continue
			}
			d, err := apidocParseDirective(fn.Doc)
			require.NoErrorf(t, err, "parsing //apidoc: directives on handler %s", fn.Name.Name)
			if d.Tag != "" {
				directives[fn.Name.Name] = d
			}

			ops := apidocOperationTypes{Responses: map[int]apidocSchema{}}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				if id, ok := call.Fun.(*ast.Ident); ok && id.Name == "writeJSON" && len(call.Args) == 3 {
					if status, ok := apidocConstInt(pkg.TypesInfo, call.Args[1]); ok {
						ops.Responses[status] = apidocResolveResponseExpr(reg, pkg.TypesInfo, call.Args[2])
					}
					// A dynamic (non-constant) status is left undocumented for
					// that call site rather than guessed — 2 such call sites
					// per the design doc's static-extraction count.
					return true
				}
				if sel, ok := call.Fun.(*ast.SelectorExpr); ok {
					if sel.Sel.Name == "Decode" && len(call.Args) == 1 {
						if unary, ok := call.Args[0].(*ast.UnaryExpr); ok {
							if tv := pkg.TypesInfo.TypeOf(unary.X); tv != nil {
								ops.Request = reg.walkType(tv)
							}
						}
						return true
					}
					if id, ok := sel.X.(*ast.Ident); ok && id.Name == "h" && handlerMethods[sel.Sel.Name] && sel.Sel.Name != fn.Name.Name {
						delegates[fn.Name.Name] = append(delegates[fn.Name.Name], sel.Sel.Name)
					}
				}
				return true
			})

			result[fn.Name.Name] = ops
		}
	}

	// Fixed-point merge: a handler with nothing of its own inherits its
	// delegate(s)' resolved responses/request — bounded iteration since a
	// delegate can itself delegate one level further (ListApplicationLogs ->
	// listContainerLogs, observed depth 1, but not assumed to be the only
	// depth that will ever exist).
	for range 5 {
		changed := false
		for name, ops := range result {
			if len(ops.Responses) > 0 && ops.Request != nil {
				continue
			}
			for _, dep := range delegates[name] {
				depOps, ok := result[dep]
				if !ok {
					continue
				}
				for status, schema := range depOps.Responses {
					if _, exists := ops.Responses[status]; !exists {
						ops.Responses[status] = schema
						changed = true
					}
				}
				if ops.Request == nil && depOps.Request != nil {
					ops.Request = depOps.Request
					changed = true
				}
			}
			result[name] = ops
		}
		if !changed {
			break
		}
	}

	for name, ops := range result {
		if len(ops.Responses) == 0 && ops.Request == nil {
			delete(result, name) // no signal at all, direct or delegated — apidocBuildDocument's zero-value lookup on a missing key is equivalent, dropping it just keeps the map small
		}
	}
	// directives is NOT pruned this way — unlike result, a handler's domain
	// classification doesn't depend on whether static extraction found any
	// request/response type worth keeping, and pruning it the same way
	// would silently lose a real directive on a handler with no writeJSON/
	// Decode call of its own (a pure delegate, say).
	return result, directives, behaviour, reg
}

// apidocKnownMarshalerTypes are the type strings walkType already
// special-cases because they implement json.Marshaler with a wire shape
// naive struct reflection would get wrong — the literal set from walkType's
// "Six traps" doc comment, not re-derived, so TestAPIDocAllMarshalerTypesHandled
// actually checks the switch statement's coverage rather than assuming it.
var apidocKnownMarshalerTypes = map[string]bool{
	"github.com/jackc/pgx/v5/pgtype.UUID":        true,
	"github.com/jackc/pgx/v5/pgtype.Timestamptz": true,
	"github.com/jackc/pgx/v5/pgtype.Text":        true,
	"github.com/jackc/pgx/v5/pgtype.Int4":        true,
	"time.Time":                                  true,
	"encoding/json.RawMessage":                   true,
}

// TestAPIDocAllMarshalerTypesHandled guards against a silent seventh trap.
// time.Time (trap #6, see walkType's doc comment) shipped unhandled past
// this generator's own hand-review and was only caught by a peer's go/types
// sweep of the whole reachable response surface for types implementing
// json.Marshaler — the exact bug class this test now closes off structurally
// instead of relying on the next review catching it too.
//
// It reuses apidocExtractTypes's own walk (recordNamed, a side effect of
// every walkType call, whether or not that call takes the special-cased
// path) to get the real reachable named-type set, then, for each one, checks
// whether it or its pointer implements json.Marshaler — the pointer check
// matters, since some of these (and future ones) may have pointer-receiver
// MarshalJSON methods. Anything that does must already be in
// apidocKnownMarshalerTypes; walkType's naive struct-field reflection is
// wrong for anything else that reaches this state.
func TestAPIDocAllMarshalerTypesHandled(t *testing.T) {
	if os.Getenv("GENERATE_API_REFERENCE") != "1" {
		t.Skip("set GENERATE_API_REFERENCE=1 to run — see task generate:api-docs")
	}

	_, _, _, reg := apidocExtractTypes(t)

	cfg := &packages.Config{Mode: packages.NeedTypes | packages.NeedDeps | packages.NeedImports}
	pkgs, err := packages.Load(cfg, "encoding/json")
	require.NoError(t, err)
	require.Len(t, pkgs, 1)
	require.Empty(t, pkgs[0].Errors)
	marshalerObj := pkgs[0].Types.Scope().Lookup("Marshaler")
	require.NotNil(t, marshalerObj, "encoding/json.Marshaler not found")
	marshalerIface, ok := marshalerObj.Type().Underlying().(*types.Interface)
	require.True(t, ok)

	var unhandled []string
	for name, named := range reg.namedTypes {
		if apidocKnownMarshalerTypes[name] {
			continue
		}
		if types.Implements(named, marshalerIface) || types.Implements(types.NewPointer(named), marshalerIface) {
			unhandled = append(unhandled, name)
		}
	}
	sort.Strings(unhandled)
	require.Empty(t, unhandled, "these types implement json.Marshaler but aren't special-cased in walkType — their default struct-reflection shape is very likely wrong; add each to the switch in walkType and to apidocKnownMarshalerTypes")
}

// === OpenAPI 3.1 document assembly ===

type oasDocument struct {
	OpenAPI    string                 `json:"openapi"`
	Info       oasInfo                `json:"info"`
	Servers    []oasServer            `json:"servers"`
	Tags       []oasTag               `json:"tags"`
	Paths      map[string]oasPathItem `json:"paths"`
	Components oasComponents          `json:"components"`
}
type oasTag struct {
	Name string `json:"name"`
}
type oasInfo struct {
	Title       string `json:"title"`
	Version     string `json:"version"`
	Description string `json:"description"`
}
type oasServer struct {
	URL         string `json:"url"`
	Description string `json:"description,omitempty"`
}
type oasPathItem map[string]oasOperation
type oasParameter struct {
	Name     string       `json:"name"`
	In       string       `json:"in"`
	Required bool         `json:"required"`
	Schema   apidocSchema `json:"schema"`
}
type oasOperation struct {
	OperationID string   `json:"operationId"`
	Summary     string   `json:"summary,omitempty"`
	Description string   `json:"description,omitempty"`
	Tags        []string `json:"tags,omitempty"`
	// Order carries an //apidoc:order directive's sort weight, emitted only
	// when a handler set one (nil otherwise, so x-belune-order is absent
	// from every operation by default and a spec with no order directives is
	// byte-identical to one written before this key existed). The site's
	// generate-api-pages.mjs re-sorts each domain's sidebar page list by it —
	// the paths object here is a Go map and serializes in key order no matter
	// the route slice's order, so this extension is the only way a curated
	// per-operation order reaches the site.
	Order *int `json:"x-belune-order,omitempty"`
	// Roles is r.Roles (the role set a RequireRole in the chain demands),
	// emitted as x-belune-roles only when non-empty. It replaces the
	// adminRole security scheme — a role is not a credential. The KEY is
	// fixed ("x-belune-roles"), the role is in the VALUE, so a consumer
	// doesn't need to know the role in advance to look it up.
	// generate-api-pages.mjs's isAdminGated reads it back to write each
	// page's `roles:` frontmatter, which drives the sidebar badge; the
	// description carries an "Requires the … role." sentence for the page
	// body (apidocRoleNote — derived, no "admin" literal).
	Roles       []string               `json:"x-belune-roles,omitempty"`
	Parameters  []oasParameter         `json:"parameters,omitempty"`
	RequestBody *oasRequestBody        `json:"requestBody,omitempty"`
	Responses   map[string]oasResponse `json:"responses"`
	Security    []map[string][]string  `json:"security"`
}
type oasRequestBody struct {
	Required bool                    `json:"required"`
	Content  map[string]oasMediaType `json:"content"`
}
type oasResponse struct {
	Description string                  `json:"description"`
	Content     map[string]oasMediaType `json:"content,omitempty"`
}
type oasMediaType struct {
	Schema apidocSchema `json:"schema"`
}
type oasComponents struct {
	Schemas         map[string]apidocSchema      `json:"schemas"`
	SecuritySchemes map[string]oasSecurityScheme `json:"securitySchemes"`
}
type oasSecurityScheme struct {
	Type        string `json:"type"`
	Scheme      string `json:"scheme"`
	Description string `json:"description"`
}

// apidocTitleWordBoundary1/2 split a PascalCase identifier into words,
// acronym-aware: a lowercase-to-uppercase transition is always a boundary
// (ListDatabase -> List Database), but a RUN of uppercase letters is kept
// together except at its own final letter, where the next word actually
// starts (ListAPITokens -> List API Tokens, not List A P I Tokens). Two
// passes, applied in this order, is the standard technique for this.
var (
	apidocTitleWordBoundary1 = regexp.MustCompile(`([a-z0-9])([A-Z])`)
	apidocTitleWordBoundary2 = regexp.MustCompile(`([A-Z]+)([A-Z][a-z])`)
)

// apidocTitleFromOperationID gives fumadocs-openapi's generated pages a
// clean title. Without this, it falls back to its own operationId splitter
// for both the page <h1> and every sidebar entry — found live, not assumed:
// the fallback splits on EVERY capital letter uniformly, mangling
// "ListAPITokens" into "List A P I Tokens" in the sidebar.
func apidocTitleFromOperationID(id string) string {
	s := apidocTitleWordBoundary1.ReplaceAllString(id, "$1 $2")
	s = apidocTitleWordBoundary2.ReplaceAllString(s, "$1 $2")
	return s
}

// apidocScopeScheme validates a probed scope requirement and returns the
// security-scheme name for it — which is just the scope string itself
// (metrics/read/deploy/write), so this is really a whitelist: a scope not in
// the lattice returns "" and apidocSecurity turns that into a loud
// UNRECOGNIZED. The scheme is named by the MINIMUM scope that satisfies the
// route (Belune's scopes are a total order, metrics ⊂ read ⊂ deploy ⊂
// write; scopeGrants in middleware/scope.go), so a route needing `read`
// lists only `read` even though a `write` token also passes — one scheme
// per route instead of every tier that would work. Also the canonical
// "is this string one of the four scope schemes" test — see the
// session-gate invariant in TestGenerateAPIReference.
func apidocScopeScheme(scope string) string {
	switch scope {
	case "metrics", "read", "deploy", "write":
		return scope
	default:
		return ""
	}
}

// apidocSecurity builds one operation's security requirement. OpenAPI's
// per-requirement `scopes` array is legal only for oauth2/openIdConnect
// schemes (MUST be empty otherwise, in every ratified spec version) — Belune
// has no OAuth2 authorization server for PATs, so the requirement is carried
// in the SCHEME NAME instead (see apidocScopeScheme), with `session` OR'd
// into every non-session-exclusive requirement (RequireScope lets a session
// JWT pass unconditionally).
//
// RequireRole gating is deliberately NOT here. A role is not a credential —
// modelling it as a bearer scheme (as an earlier "adminRole" scheme did)
// made renderers offer a bearer field nobody can fill. r.Roles is emitted as
// the x-belune-roles vendor extension and stated in the operation
// description instead; see apidocBuildDocument and apidocOperationDescription.
func apidocSecurity(r apidocRoute) []map[string][]string {
	switch {
	case r.Scope == "none" && r.Session:
		// No RequireScope* at all — only RequireSession gates it (e.g. the WS
		// terminal tunnel). No PAT alternative exists at any scope.
		return []map[string][]string{{"session": {}}}
	case r.Session:
		// A scope requirement exists too (probe 1 still resolved one), but
		// RequireSession rejects every PAT regardless — the scope is real but
		// moot for authorization purposes, so only session is listed.
		return []map[string][]string{{"session": {}}}
	default:
		scheme := apidocScopeScheme(r.Scope)
		if scheme == "" {
			return nil // UNRECOGNIZED — TestGenerateAPIReference already fails loudly on this
		}
		return []map[string][]string{{scheme: {}}, {"session": {}}}
	}
}

func apidocParameters(path string, query []string) []oasParameter {
	var params []oasParameter
	for _, seg := range strings.Split(path, "/") {
		if strings.HasPrefix(seg, "{") && strings.HasSuffix(seg, "}") {
			params = append(params, oasParameter{
				Name: seg[1 : len(seg)-1], In: "path", Required: true,
				Schema: apidocSchema{"type": "string", "format": "uuid"},
			})
		}
	}

	// Query parameters, from what the handler actually reads
	// (apidocExtractBehaviour). Every one is emitted as OPTIONAL and typed
	// string, deliberately:
	//
	// Required-ness is not visible at the read. `hostname` is required because
	// ListUsableCertificates 400s on empty, `project_id` is a filter, and
	// `limit` belongs to a shared helper — three different contracts behind one
	// identical Get call. Claiming any of them is required would be a guess,
	// and this generator's whole premise is that it states only what it can
	// show. Naming the parameters is already the large improvement over the
	// silence it replaces; per-parameter required/typed detail is a later,
	// separate question.
	//
	// The same goes for the type: a value arrives as a string and the handler
	// decides what it means, so string is what the wire actually carries.
	for _, name := range query {
		params = append(params, oasParameter{
			Name: name, In: "query", Required: false,
			Schema: apidocSchema{"type": "string"},
		})
	}
	return params
}

// apidocOperationDescription composes a //apidoc:description directive (an
// EDITORIAL claim) with the caveats a schema alone can't carry (DERIVED
// facts the generator discovered). Editorial text always comes first, since
// it's the more useful framing for a reader; each derived note is APPENDED
// and can never be suppressed by a directive — a declaration must not be
// able to turn off something the generator found, same rule as directives
// not carrying permissions.
//
// Two derived notes, in this FIXED order (don't append a third arbitrarily —
// decide where it reads):
//
//  1. project-pinning (r.Pinned) — has no OpenAPI representation at all
//  2. role gating (r.Roles) — the adminRole security scheme was removed for
//     being a fake credential, and the sidebar badge is sidebar-only, so
//     without this sentence a role-gated operation's PAGE body would state
//     the requirement nowhere. Text is apidocRoleNote(r.Roles), derived, so
//     a role rename in routes.go flows through with no literal to update.
func apidocOperationDescription(r apidocRoute, directive apidocDirective) string {
	var notes []string
	// Either kind of pin: the middleware's, or the handler's own for a route
	// with no {projectId} param for that middleware to compare against.
	if r.Pinned || r.HandlerPinned {
		notes = append(notes, "Scoped to a project-pinned token's pin set — a project outside it is rejected, regardless of scope.")
	}
	if len(r.Roles) > 0 {
		notes = append(notes, apidocRoleNote(r.Roles))
	}
	return strings.TrimSpace(directive.Description + " " + strings.Join(notes, " "))
}

const apidocSpecDescription = `Generated from the running API by probing every registered route with a personal access token and statically resolving every request/response Go type — not hand-written, and not a reading of routes.go. See the API Access guide for how to authenticate and the scope model.

**"required" is intentionally omitted from every request schema.** Field optionality in this codebase is enforced imperatively (e.g. ` + "`if req.Name == \"\" { ... }`" + `), not type-encoded — most optional fields aren't even pointers. Inferring "required" from arbitrary validation code is a fundamentally fuzzier problem than the static type resolution the rest of this spec relies on, so it is left honestly unspecified rather than risked wrong.

**Project-pinning has no OpenAPI representation** — a token pinned to a set of projects, rejected on any project outside that set, is noted in the affected operations' descriptions, not encoded structurally.`

func apidocSecuritySchemes() map[string]oasSecurityScheme {
	return map[string]oasSecurityScheme{
		"metrics": {Type: "http", Scheme: "bearer", Description: "Personal access token whose scope satisfies `metrics` — metrics, read, deploy, or write all qualify (Belune's scopes form a total order)."},
		"read":    {Type: "http", Scheme: "bearer", Description: "Personal access token whose scope satisfies `read` — read, deploy, or write all qualify."},
		"deploy":  {Type: "http", Scheme: "bearer", Description: "Personal access token whose scope satisfies `deploy` — deploy or write qualify."},
		"write":   {Type: "http", Scheme: "bearer", Description: "Personal access token whose scope satisfies `write`."},
		"session": {Type: "http", Scheme: "bearer", Description: "Dashboard session JWT — satisfies every scope requirement unconditionally."},
		// No role scheme: a role isn't a credential. Role gating is
		// x-belune-roles + a description sentence — see apidocSecurity.
	}
}

// apidocBuildDocument assembles the full OpenAPI document from the probed
// routes and the statically-extracted request/response types, joining the
// two on handler function name.
func apidocBuildDocument(t *testing.T, routes []apidocRoute, sig map[string]apidocOperationTypes, directives map[string]apidocDirective, reg *apidocSchemaRegistry) oasDocument {
	t.Helper()

	domains := apidocLoadDomains(t)
	grouped := apidocGroupByDomain(t, routes, domains, directives)

	// Permanent invariant, not a one-time migration check: every documented
	// route's handler must carry a //apidoc:tag directive. A route with no
	// directive lands in "uncategorized" (apidocRouteDomain's fallback)
	// instead of panicking outright — same "never silently drop a route"
	// reasoning apidocClassify's old default case had — but leaving it
	// there is now always a bug, not just untidy, since a directive is the
	// ONLY way a route gets classified anymore. Fails loud here rather than
	// shipping an "Uncategorized" tag nobody meant to keep.
	for _, d := range grouped {
		if d.Slug != "uncategorized" {
			continue
		}
		var missing []string
		for _, r := range d.Rows {
			missing = append(missing, fmt.Sprintf("%s %s (%s)", r.Method, r.Path, r.Handler))
		}
		require.Empty(t, missing, "routes with no //apidoc:tag directive on their handler")
	}

	// Permanent invariant: the role signal the docs show must agree with the
	// RequireRole middleware fact. Role gating is emitted as x-belune-roles
	// (op.Roles, straight from r.Roles) and rendered as the sidebar badge —
	// all one derivation, so checking x-belune-roles against r.Roles would be
	// vacuous. api-domains.json's " (Admin)" tag suffix is the ONE role
	// signal a human curates independently of r.Roles, so it's what this
	// checks, both directions:
	//
	//   - every route under a " (Admin)"-suffixed tag must be RequireRole-gated
	//     (a mixed domain mis-tagged, or a route in a role-gated domain that
	//     lost its gate, surfaces here)
	//   - the RequireRole-gated routes whose tag is NOT "(Admin)"-suffixed —
	//     the ones that take an item-level badge in an otherwise-mixed section
	//     — are pinned to exactly roleGatedInMixedDomain. A 7th appearing, or
	//     one of these moving/losing its gate, becomes a deliberate edit to
	//     that list rather than silent drift (same intent as the 35-session pin).
	//
	// The " (Admin)" suffix in api-domains.json is load-bearing for this
	// check — JSON can't carry a comment saying so, hence this one. It stays
	// literally "(Admin)" even if the role set is renamed: it's a human
	// grouping label, not derived text.
	{
		// Empty, and that is the correct state rather than a stale list: every
		// RequireRole-gated route now sits under a " (Admin)"-suffixed tag, so
		// the DOMAIN states the restriction and no item-level badge has to.
		// GET /api/domains/tls was the last entry and left when it stopped being
		// role-gated at all (it is role-SCOPED now — admins see every domain,
		// members their own). Keep the machinery: the next route that lands
		// role-gated in a mixed domain must be a deliberate line here, not drift.
		roleGatedInMixedDomain := map[string]bool{}
		var overstated, unpinned []string
		foundInMixed := map[string]bool{}
		for _, d := range grouped {
			adminTag := strings.HasSuffix(d.Title, " (Admin)")
			for _, r := range d.Rows {
				key := apidocRouteKey(r)
				switch {
				case adminTag && len(r.Roles) == 0:
					overstated = append(overstated, key)
				case !adminTag && len(r.Roles) > 0 && !r.Session:
					// !r.Session: a session-gated role route has no page and
					// no sidebar entry, so there's no rendered signal to pin.
					if roleGatedInMixedDomain[key] {
						foundInMixed[key] = true
					} else {
						unpinned = append(unpinned, key)
					}
				}
			}
		}
		require.Empty(t, overstated, "route(s) under a \" (Admin)\"-suffixed tag that RequireRole does NOT gate — api-domains.json overstates the restriction (fix the tag or the route)")
		require.Empty(t, unpinned, "RequireRole-gated route(s) in a non-\"(Admin)\" domain and not in roleGatedInMixedDomain — if the item-level badge is intended, add them there")
		var movedOrUngated []string
		for key := range roleGatedInMixedDomain {
			if !foundInMixed[key] {
				movedOrUngated = append(movedOrUngated, key)
			}
		}
		require.Empty(t, movedOrUngated, "roleGatedInMixedDomain lists route(s) the probe no longer sees as RequireRole-gated outside an \"(Admin)\" tag — moved, renamed, or lost the gate")
	}

	domainTitleByRoute := map[string]string{}
	for _, d := range grouped {
		for _, r := range d.Rows {
			domainTitleByRoute[apidocRouteKey(r)] = d.Title
		}
	}

	reg.schemas["Error"] = apidocSchema{
		"type":       "object",
		"properties": apidocSchema{"error": apidocSchema{"type": "string"}},
	}
	errorRef := apidocSchema{"$ref": "#/components/schemas/Error"}

	paths := map[string]oasPathItem{}
	seenOperationIDs := map[string]bool{}
	for _, r := range routes {
		item, ok := paths[r.Path]
		if !ok {
			item = oasPathItem{}
		}

		opID := r.Handler
		if seenOperationIDs[opID] {
			opID = r.Handler + "_" + strings.ToLower(r.Method)
		}
		seenOperationIDs[opID] = true

		directive := directives[r.Handler]
		summary := directive.Title
		if summary == "" {
			// r.Handler, not opID — opID can carry a disambiguation suffix
			// (_get/_post) on the rare handler reused across two routes; the
			// title should stay clean regardless.
			summary = apidocTitleFromOperationID(r.Handler)
		}

		op := oasOperation{
			OperationID: opID,
			Summary:     summary,
			Tags:        []string{domainTitleByRoute[apidocRouteKey(r)]},
			Order:       directive.Order,
			Roles:       r.Roles,
			Description: apidocOperationDescription(r, directive),
			Parameters:  apidocParameters(r.Path, r.Query),
			Security:    apidocSecurity(r),
			Responses:   map[string]oasResponse{},
		}

		ops := sig[r.Handler]
		if ops.Request != nil {
			op.RequestBody = &oasRequestBody{
				Required: true, // a Decode call exists — an empty/missing body fails it; this is a mechanical fact, unlike per-field "required"
				Content:  map[string]oasMediaType{"application/json": {Schema: ops.Request}},
			}
		}
		for status, schema := range ops.Responses {
			op.Responses[strconv.Itoa(status)] = oasResponse{
				Description: http.StatusText(status),
				Content:     map[string]oasMediaType{"application/json": {Schema: schema}},
			}
		}
		if len(op.Responses) == 0 {
			// No writeJSON call resolved for this handler (a 204, a redirect,
			// a dynamic status code, or a genuine extraction gap) — an
			// honestly-empty response entry beats inventing a 200 shape that
			// might not exist.
			op.Responses["default"] = oasResponse{Description: "Not resolved by static extraction — see the handler for the exact response."}
		}
		op.Responses["403"] = oasResponse{Description: "Forbidden", Content: map[string]oasMediaType{"application/json": {Schema: errorRef}}}

		item[strings.ToLower(r.Method)] = op
		paths[r.Path] = item
	}

	// Top-level tags, in apidocDomainsPath's curated order — the SOURCE
	// fumadocs-openapi's own per-tag page generation reads for page order
	// too, so the site's sidebar and this spec's own tag list stay in the
	// one order this generator defines, rather than falling back to
	// whatever order operations happen to appear in. Reuses `grouped` from
	// above (not a second apidocGroupByDomain call) — same "only domains
	// that actually classified a route this run" filtering the
	// uncategorized check above already relied on: an empty tag here
	// becomes an empty, unlisted .mdx page from
	// apps/site/scripts/generate-api-pages.mjs, reachable by direct URL
	// despite meta.json never listing it.
	var tags []oasTag
	for _, d := range grouped {
		tags = append(tags, oasTag{Name: d.Title})
	}

	return oasDocument{
		OpenAPI: "3.1.0",
		Info: oasInfo{
			Title:       "Belune API",
			Version:     "1.0.0", // the spec's own version, independent of Belune's release tag — bump by hand on a shape change; NOT a generation timestamp, which would fail the CI freshness check every day even with zero real drift
			Description: apidocSpecDescription,
		},
		Servers: []oasServer{{URL: "https://belune.example.com", Description: "Replace with your own Belune instance's origin."}},
		Tags:    tags,
		Paths:   paths,
		Components: oasComponents{
			Schemas:         reg.schemas,
			SecuritySchemes: apidocSecuritySchemes(),
		},
	}
}

// apidocSpecPath is relative to this package's directory, the same
// cross-module pattern apidocMDXOutDir (below) and
// internal/tlsstatus/expiry_parity_test.go already use. Public, not under
// content/docs, so it's a stable URL third parties can consume directly —
// the same shape apps/site/public/versions.json already is.
const apidocSpecPath = "../../../site/public/openapi.json"

func apidocWriteSpec(t *testing.T, doc oasDocument) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(apidocSpecPath), 0o755))
	b, err := json.MarshalIndent(doc, "", "  ")
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(apidocSpecPath, append(b, '\n'), 0o644))
}
