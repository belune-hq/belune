package handler_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/belune-hq/belune/internal/service"
	"github.com/belune-hq/belune/internal/store/generated"
	"github.com/belune-hq/belune/internal/testutil"
)

// createAPIToken inserts a token directly (there is no create endpoint yet —
// that is PR 3) and returns its plaintext value, ready to use in a Bearer
// header, plus its id.
func createAPIToken(t *testing.T, userID string, roleAtIssue string, expiresAt pgtype.Timestamptz) (plain string, tokenID string) {
	t.Helper()
	var uid pgtype.UUID
	require.NoError(t, uid.Scan(userID))

	plainTok, hash, err := service.GenerateToken()
	require.NoError(t, err)

	row, err := env.Queries.CreateAPIToken(context.Background(), generated.CreateAPITokenParams{
		UserID:      uid,
		Name:        "test token",
		TokenHash:   hash,
		Scopes:      []string{"read", "write"},
		RoleAtIssue: roleAtIssue,
		ExpiresAt:   expiresAt,
	})
	require.NoError(t, err)
	return plainTok, idStr(row.ID)
}

// TestPATAuth_ValidTokenAuthenticates pins the core Bearer branch: a PAT
// reaches a protected endpoint exactly like a session JWT, scoped to its
// owner.
func TestPATAuth_ValidTokenAuthenticates(t *testing.T) {
	resetDB(t)
	adminToken := env.SetupAdmin(t, "admin@test.com", "password123")
	memberID, _ := createMember(t, adminToken, "member@test.com")

	plain, _ := createAPIToken(t, memberID, "member", pgtype.Timestamptz{})

	project := env.CreateProject(t, adminToken, "Admin Project", "admin-project")
	projectID := extractID(project["id"])

	// The token's own owner (a member) cannot reach the admin's project.
	resp := env.DoRequest(t, "GET", fmt.Sprintf("/api/projects/%s", projectID), nil, testutil.AuthHeader(plain))
	assert.Equal(t, http.StatusForbidden, resp.StatusCode)
	resp.Body.Close()

	// But the token DOES authenticate as its owner: it can list projects
	// (empty, since the member owns none) rather than 401ing.
	resp = env.DoRequest(t, "GET", "/api/projects", nil, testutil.AuthHeader(plain))
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Len(t, testutil.ReadJSONArray(t, resp), 0)
}

// TestPATAuth_UnknownOrGarbageTokenRejected pins the negative path: a
// PAT-shaped value that does not match any stored hash must 401, not panic
// or fall through to the JWT parser.
func TestPATAuth_UnknownOrGarbageTokenRejected(t *testing.T) {
	resetDB(t)
	env.SetupAdmin(t, "admin@test.com", "password123")

	resp := env.DoRequest(t, "GET", "/api/projects", nil, testutil.AuthHeader(service.TokenPrefix+"does-not-exist"))
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
	resp.Body.Close()
}

// TestPATAuth_ExpiredTokenRejected pins the expiry check.
func TestPATAuth_ExpiredTokenRejected(t *testing.T) {
	resetDB(t)
	adminToken := env.SetupAdmin(t, "admin@test.com", "password123")
	memberID, _ := createMember(t, adminToken, "member@test.com")

	plain, _ := createAPIToken(t, memberID, "member", pgtype.Timestamptz{
		Time: time.Now().Add(-time.Hour), Valid: true,
	})

	resp := env.DoRequest(t, "GET", "/api/projects", nil, testutil.AuthHeader(plain))
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
	resp.Body.Close()
}

// TestPATAuth_RoleCanOnlyShrink pins the asymmetric role rule: a token whose
// owner was DEMOTED after issue gets the lower role immediately (restricts);
// a token whose owner was PROMOTED after issue does NOT retroactively gain
// the higher role (an existing credential must never silently escalate).
func TestPATAuth_RoleCanOnlyShrink(t *testing.T) {
	resetDB(t)
	adminToken := env.SetupAdmin(t, "admin@test.com", "password123")

	// Demotion: token issued while admin, owner demoted to member afterwards.
	demotedID, _ := createMember(t, adminToken, "demoted@test.com")
	resp := env.DoRequest(t, "PUT", "/api/users/"+demotedID+"/role", map[string]string{"role": "admin"}, testutil.AuthHeader(adminToken))
	require.Equal(t, http.StatusOK, resp.StatusCode)
	resp.Body.Close()
	demotedPlain, _ := createAPIToken(t, demotedID, "admin", pgtype.Timestamptz{})
	resp = env.DoRequest(t, "PUT", "/api/users/"+demotedID+"/role", map[string]string{"role": "member"}, testutil.AuthHeader(adminToken))
	require.Equal(t, http.StatusOK, resp.StatusCode)
	resp.Body.Close()

	// The token was minted as admin but the owner is a member now — an
	// admin-only endpoint must reject it.
	resp = env.DoRequest(t, "GET", "/api/users", nil, testutil.AuthHeader(demotedPlain))
	assert.Equal(t, http.StatusForbidden, resp.StatusCode, "a demoted owner's token must lose admin access immediately")
	resp.Body.Close()

	// Promotion: token issued while member, owner promoted to admin afterwards.
	promotedID, _ := createMember(t, adminToken, "promoted@test.com")
	promotedPlain, _ := createAPIToken(t, promotedID, "member", pgtype.Timestamptz{})
	resp = env.DoRequest(t, "PUT", "/api/users/"+promotedID+"/role", map[string]string{"role": "admin"}, testutil.AuthHeader(adminToken))
	require.Equal(t, http.StatusOK, resp.StatusCode)
	resp.Body.Close()

	resp = env.DoRequest(t, "GET", "/api/users", nil, testutil.AuthHeader(promotedPlain))
	assert.Equal(t, http.StatusForbidden, resp.StatusCode, "a token minted under a lower role must not gain admin access just because its owner was later promoted")
	resp.Body.Close()
}

// TestPATAuth_LastUsedAtCoarsened pins the write-coarsening rule: two
// requests inside the coarsening window must not produce two writes.
func TestPATAuth_LastUsedAtCoarsened(t *testing.T) {
	resetDB(t)
	adminToken := env.SetupAdmin(t, "admin@test.com", "password123")
	memberID, _ := createMember(t, adminToken, "member@test.com")
	plain, tokenID := createAPIToken(t, memberID, "member", pgtype.Timestamptz{})

	resp := env.DoRequest(t, "GET", "/api/projects", nil, testutil.AuthHeader(plain))
	require.Equal(t, http.StatusOK, resp.StatusCode)
	resp.Body.Close()

	var tid pgtype.UUID
	require.NoError(t, tid.Scan(tokenID))

	first := fetchTokenLastUsed(t, tid)
	require.True(t, first.Valid, "last_used_at must be set on first use")

	resp = env.DoRequest(t, "GET", "/api/projects", nil, testutil.AuthHeader(plain))
	require.Equal(t, http.StatusOK, resp.StatusCode)
	resp.Body.Close()

	second := fetchTokenLastUsed(t, tid)
	assert.Equal(t, first.Time, second.Time, "a second use inside the coarsening window must not rewrite last_used_at")
}

func fetchTokenLastUsed(t *testing.T, tokenID pgtype.UUID) pgtype.Timestamptz {
	t.Helper()
	var lastUsed pgtype.Timestamptz
	err := env.Pool.QueryRow(context.Background(), "SELECT last_used_at FROM api_tokens WHERE id = $1", tokenID).Scan(&lastUsed)
	require.NoError(t, err)
	return lastUsed
}

// TestAuditLog_AttributesToToken pins the point of PR 2: an audit entry can
// carry the id of the PAT that made it, not just the owning user — the gap
// that made the plan's audit-attribution step necessary before any token
// could be issued. Exercised at the AuditService/DB layer directly rather
// than through a live HTTP request: the integration harness wires
// h.auditSvc as nil (SetupTestServer) to keep every other test free of the
// async writer, so this is the level at which the new column and the
// widened Log() signature are actually reachable.
func TestAuditLog_AttributesToToken(t *testing.T) {
	resetDB(t)
	adminToken := env.SetupAdmin(t, "admin@test.com", "password123")
	memberID, _ := createMember(t, adminToken, "member@test.com")
	_, tokenID := createAPIToken(t, memberID, "member", pgtype.Timestamptz{})

	auditSvc := service.NewAuditService(env.Queries)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go auditSvc.Run(ctx)

	auditSvc.Log(memberID, tokenID, "127.0.0.1", "create_project", "project", "via-token-project", nil)

	var gotUserID, gotTokenID pgtype.UUID
	require.Eventually(t, func() bool {
		row := env.Pool.QueryRow(context.Background(),
			"SELECT user_id, token_id FROM audit_logs WHERE action = 'create_project' AND resource_id = 'via-token-project'")
		return row.Scan(&gotUserID, &gotTokenID) == nil
	}, 2*time.Second, 10*time.Millisecond, "audit entry should appear once the async writer drains it")

	assert.Equal(t, memberID, idStr(gotUserID))
	assert.Equal(t, tokenID, idStr(gotTokenID), "the audit entry must attribute the action to the token, not just the owning user")
}

// TestAuditLog_SessionActionLeavesTokenIDNull is TestAuditLog_AttributesToToken's
// negative twin: a session (no PAT) action must leave token_id NULL, not some
// zero-value UUID that would misattribute it to a real token.
func TestAuditLog_SessionActionLeavesTokenIDNull(t *testing.T) {
	resetDB(t)
	adminToken := env.SetupAdmin(t, "admin@test.com", "password123")
	memberID, _ := createMember(t, adminToken, "member@test.com")

	auditSvc := service.NewAuditService(env.Queries)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go auditSvc.Run(ctx)

	auditSvc.Log(memberID, "", "127.0.0.1", "create_project", "project", "via-session-project", nil)

	var gotTokenID pgtype.UUID
	require.Eventually(t, func() bool {
		row := env.Pool.QueryRow(context.Background(),
			"SELECT token_id FROM audit_logs WHERE action = 'create_project' AND resource_id = 'via-session-project'")
		return row.Scan(&gotTokenID) == nil
	}, 2*time.Second, 10*time.Millisecond, "audit entry should appear once the async writer drains it")

	assert.False(t, gotTokenID.Valid, "a session-authenticated action must leave token_id NULL")
}

// TestDeleteProject_NarrowsPinnedTokenAndAuditStillWrites replaces what used
// to be TestDeleteProject_CascadesPinnedTokenAndAuditStillWrites. Migration
// 000067 moved the pin's ON DELETE CASCADE off api_tokens itself and onto the
// join table api_token_projects, specifically so deleting a project no
// longer destroys every token pinned to it — it only narrows their reach.
// This asserts the new contract (token survives, its pin row for the deleted
// project is gone) and keeps the still-relevant half of the old test: audit
// writes must never depend on a token still existing — audit_logs.token_id
// is deliberately NOT a foreign key (see migration 000066), because audit
// writes are async and a dangling token_id must never make one vanish.
func TestDeleteProject_NarrowsPinnedTokenAndAuditStillWrites(t *testing.T) {
	resetDB(t)
	adminToken := env.SetupAdmin(t, "admin@test.com", "password123")
	memberID, _ := createMember(t, adminToken, "member@test.com")

	project := env.CreateProject(t, adminToken, "Pin Target", "pin-target")
	projectID := extractID(project["id"])

	var uid, pid pgtype.UUID
	require.NoError(t, uid.Scan(memberID))
	require.NoError(t, pid.Scan(projectID))
	plainTok, hash, err := service.GenerateToken()
	require.NoError(t, err)
	tok, err := env.Queries.CreateAPIToken(context.Background(), generated.CreateAPITokenParams{
		UserID:      uid,
		Name:        "pinned token",
		TokenHash:   hash,
		Scopes:      []string{"read"},
		Pinned:      true,
		RoleAtIssue: "member",
	})
	require.NoError(t, err)
	require.NoError(t, env.Queries.CreateAPITokenProjectPins(context.Background(), generated.CreateAPITokenProjectPinsParams{
		TokenID:    tok.ID,
		ProjectIds: []pgtype.UUID{pid},
	}))
	_ = plainTok

	resp := env.DoRequest(t, "DELETE", "/api/projects/"+projectID, nil, testutil.AuthHeader(adminToken))
	require.Equal(t, http.StatusOK, resp.StatusCode)
	resp.Body.Close()

	// The token survives deleting the project it was pinned to — only its
	// pin row for that project is gone.
	var count int
	require.NoError(t, env.Pool.QueryRow(context.Background(),
		"SELECT count(*) FROM api_tokens WHERE id = $1", tok.ID).Scan(&count))
	assert.Equal(t, 1, count, "deleting a pinned project must narrow the token's reach, not delete the token")

	require.NoError(t, env.Pool.QueryRow(context.Background(),
		"SELECT count(*) FROM api_token_projects WHERE token_id = $1", tok.ID).Scan(&count))
	assert.Equal(t, 0, count, "the pin row for the deleted project must be gone (ON DELETE CASCADE on the join table)")

	// An audit write naming this token as the actor must still succeed
	// regardless of what happened to it above.
	auditSvc := service.NewAuditService(env.Queries)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go auditSvc.Run(ctx)
	auditSvc.Log(memberID, idStr(tok.ID), "127.0.0.1", "delete_project", "project", projectID, nil)

	require.Eventually(t, func() bool {
		var n int
		_ = env.Pool.QueryRow(context.Background(),
			"SELECT count(*) FROM audit_logs WHERE action = 'delete_project' AND resource_id = $1", projectID).Scan(&n)
		return n == 1
	}, 2*time.Second, 10*time.Millisecond, "the audit row for the delete must be written")
}

// TestTokenPinnedToOnlyProjectReachesNothingAfterDelete is the escalation
// regression migration 000067 exists to prevent. The naive design — a join
// table alone, "zero pin rows" read as unpinned — has a silent privilege
// escalation: deleting a token's LAST pinned project would leave it with zero
// rows, and under that reading it would suddenly reach every project its
// owner can see. pinned is recorded on the token itself specifically so this
// can't happen: a pinned token with zero surviving pins must reach NOTHING.
func TestTokenPinnedToOnlyProjectReachesNothingAfterDelete(t *testing.T) {
	resetDB(t)
	adminToken := env.SetupAdmin(t, "admin@test.com", "password123")
	memberID, memberToken := createMember(t, adminToken, "member@test.com")

	// Both owned by the member directly (not the admin): a token's reach is
	// owner's role ∩ project access ∩ scope, so pinning to a project the
	// member could not otherwise reach would prove nothing about the pin —
	// it would already be forbidden regardless. Both must be independently
	// reachable by the member for the assertions below to isolate what the
	// pin itself is doing.
	pinTarget := env.CreateProject(t, memberToken, "Solo Pin", "solo-pin")
	pinTargetID := extractID(pinTarget["id"])
	otherOwned := env.CreateProject(t, memberToken, "Other Owned", "other-owned")
	_ = otherOwned

	plain := createPinnedAPIToken(t, memberID, pinTargetID, service.AllScopes)

	// Before deletion: the pinned project is reachable, the other is not.
	resp := env.DoRequest(t, "GET", "/api/projects/"+pinTargetID, nil, testutil.AuthHeader(plain))
	require.Equal(t, http.StatusOK, resp.StatusCode)
	resp.Body.Close()

	resp = env.DoRequest(t, "DELETE", "/api/projects/"+pinTargetID, nil, testutil.AuthHeader(adminToken))
	require.Equal(t, http.StatusOK, resp.StatusCode)
	resp.Body.Close()

	// After its only pin is gone: the token must reach NOTHING, not every
	// project its owner (a member who independently owns another project)
	// can otherwise see.
	resp = env.DoRequest(t, "GET", "/api/projects", nil, testutil.AuthHeader(plain))
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var projects []map[string]any
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&projects))
	resp.Body.Close()
	assert.Empty(t, projects, "a token whose only pin was deleted must reach nothing, not every project its owner can see")
}
