package handler_test

import (
	"context"
	"crypto/sha256"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/weiliang79/belune/internal/service"
	"github.com/weiliang79/belune/internal/testutil"
)

// TestCreateAPIToken_ReturnsPlaintextOnceAndAuthenticates pins the create
// endpoint's core contract: the plaintext comes back exactly on this
// response, and it works as a Bearer credential immediately.
func TestCreateAPIToken_ReturnsPlaintextOnceAndAuthenticates(t *testing.T) {
	resetDB(t)
	adminToken := env.SetupAdmin(t, "admin@test.com", "password123")

	resp := env.DoRequest(t, "POST", "/api/tokens", map[string]any{
		"name":   "ci token",
		"scopes": service.AllScopes,
	}, testutil.AuthHeader(adminToken))
	require.Equal(t, http.StatusCreated, resp.StatusCode)
	body := testutil.ReadJSON(t, resp)

	plain, ok := body["token"].(string)
	require.True(t, ok, "create response must carry the plaintext token")
	assert.True(t, service.HasTokenPrefix(plain))
	assert.Equal(t, "ci token", body["name"])

	// GET /api/auth/me, not /api/tokens — the latter now requires a live
	// session (account-management routes were gated behind
	// middleware.RequireSession after this test was written; see routes.go).
	// /api/auth/me stays PAT-callable specifically so a script can confirm
	// what it authenticated as, which is exactly the "works as a Bearer
	// credential immediately" property this test pins.
	meResp := env.DoRequest(t, "GET", "/api/auth/me", nil, testutil.AuthHeader(plain))
	require.Equal(t, http.StatusOK, meResp.StatusCode, "the newly minted token must authenticate on its own")
}

// TestCreateAPIToken_ListNeverCarriesSecret pins that no endpoint past
// creation ever exposes the plaintext or the hash — the list DTO is a
// deliberately narrower shape than the stored row.
func TestCreateAPIToken_ListNeverCarriesSecret(t *testing.T) {
	resetDB(t)
	adminToken := env.SetupAdmin(t, "admin@test.com", "password123")

	createResp := env.DoRequest(t, "POST", "/api/tokens", map[string]any{
		"name":   "list-secret-check",
		"scopes": service.AllScopes,
	}, testutil.AuthHeader(adminToken))
	require.Equal(t, http.StatusCreated, createResp.StatusCode)
	created := testutil.ReadJSON(t, createResp)
	plain := created["token"].(string)

	listResp := env.DoRequest(t, "GET", "/api/tokens", nil, testutil.AuthHeader(adminToken))
	require.Equal(t, http.StatusOK, listResp.StatusCode)
	items := testutil.ReadJSONArray(t, listResp)
	require.Len(t, items, 1)
	row := items[0].(map[string]any)

	_, hasToken := row["token"]
	_, hasHash := row["token_hash"]
	assert.False(t, hasToken, "the list must never carry the plaintext")
	assert.False(t, hasHash, "the list must never carry the hash")
	assert.NotEqual(t, plain, row["id"], "sanity: the id is not somehow the plaintext")
}

// TestCreateAPIToken_StoresSHA256HashOfPlaintext pins the storage format
// itself: what lands in token_hash is SHA-256 of the plaintext, not the
// plaintext, and not something reversible.
func TestCreateAPIToken_StoresSHA256HashOfPlaintext(t *testing.T) {
	resetDB(t)
	adminToken := env.SetupAdmin(t, "admin@test.com", "password123")

	resp := env.DoRequest(t, "POST", "/api/tokens", map[string]any{
		"name":   "hash-check",
		"scopes": service.AllScopes,
	}, testutil.AuthHeader(adminToken))
	require.Equal(t, http.StatusCreated, resp.StatusCode)
	body := testutil.ReadJSON(t, resp)
	plain := body["token"].(string)
	id := body["id"].(string)

	var idUUID pgtype.UUID
	require.NoError(t, idUUID.Scan(id))

	var storedHash []byte
	require.NoError(t, env.Pool.QueryRow(context.Background(),
		"SELECT token_hash FROM api_tokens WHERE id = $1", idUUID).Scan(&storedHash))

	wantHash := sha256.Sum256([]byte(plain))
	assert.Equal(t, wantHash[:], storedHash)
}

// TestCreateAPIToken_RequiresName pins basic validation: an empty (or
// all-whitespace) name is rejected before anything is minted.
func TestCreateAPIToken_RequiresName(t *testing.T) {
	resetDB(t)
	adminToken := env.SetupAdmin(t, "admin@test.com", "password123")

	resp := env.DoRequest(t, "POST", "/api/tokens", map[string]any{
		"name":   "   ",
		"scopes": service.AllScopes,
	}, testutil.AuthHeader(adminToken))
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
	resp.Body.Close()
}

// TestCreateAPIToken_RejectsExpiryOutsideThePicker pins that the API enforces
// the same 1/7/14/30/60/90 list the UI offers — a client cannot request an
// expiry the UI never presents.
func TestCreateAPIToken_RejectsExpiryOutsideThePicker(t *testing.T) {
	resetDB(t)
	adminToken := env.SetupAdmin(t, "admin@test.com", "password123")

	resp := env.DoRequest(t, "POST", "/api/tokens", map[string]any{
		"name":            "bad-expiry",
		"expires_in_days": 5,
		"scopes":          service.AllScopes,
	}, testutil.AuthHeader(adminToken))
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
	resp.Body.Close()
}

// TestCreateAPIToken_OmittedExpiryNeverExpires pins the "no expiry" choice:
// leaving expires_in_days out entirely (not zero) must store NULL.
func TestCreateAPIToken_OmittedExpiryNeverExpires(t *testing.T) {
	resetDB(t)
	adminToken := env.SetupAdmin(t, "admin@test.com", "password123")

	resp := env.DoRequest(t, "POST", "/api/tokens", map[string]any{
		"name":   "forever",
		"scopes": service.AllScopes,
	}, testutil.AuthHeader(adminToken))
	require.Equal(t, http.StatusCreated, resp.StatusCode)
	body := testutil.ReadJSON(t, resp)
	assert.Nil(t, body["expires_at"])
}

// TestCreateAPIToken_HonorsRequestedScopes pins PR4's picker: the scopes on
// the created token are exactly what the caller asked for, not automatically
// AllScopes (that was PR3, before the picker existed).
func TestCreateAPIToken_HonorsRequestedScopes(t *testing.T) {
	resetDB(t)
	adminToken := env.SetupAdmin(t, "admin@test.com", "password123")

	resp := env.DoRequest(t, "POST", "/api/tokens", map[string]any{
		"name":   "narrow",
		"scopes": []string{"read"},
	}, testutil.AuthHeader(adminToken))
	require.Equal(t, http.StatusCreated, resp.StatusCode)
	body := testutil.ReadJSON(t, resp)

	gotScopes, ok := body["scopes"].([]any)
	require.True(t, ok)
	got := make([]string, len(gotScopes))
	for i, s := range gotScopes {
		got[i] = s.(string)
	}
	assert.ElementsMatch(t, []string{"read"}, got)
}

// TestCreateAPIToken_RejectsUnknownScope pins that a scope name outside
// service.AllScopes is rejected, not silently dropped or stored verbatim —
// PR4's enforcement only knows about the four canonical values.
func TestCreateAPIToken_RejectsUnknownScope(t *testing.T) {
	resetDB(t)
	adminToken := env.SetupAdmin(t, "admin@test.com", "password123")

	resp := env.DoRequest(t, "POST", "/api/tokens", map[string]any{
		"name":   "scope-smuggle",
		"scopes": []string{"admin"},
	}, testutil.AuthHeader(adminToken))
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
	resp.Body.Close()
}

// TestCreateAPIToken_RejectsEmptyScopes pins that a token cannot be minted
// with zero scopes — it would authenticate but be able to do nothing, which
// is a footgun rather than a legitimately narrow choice.
func TestCreateAPIToken_RejectsEmptyScopes(t *testing.T) {
	resetDB(t)
	adminToken := env.SetupAdmin(t, "admin@test.com", "password123")

	resp := env.DoRequest(t, "POST", "/api/tokens", map[string]any{
		"name":   "no-scopes",
		"scopes": []string{},
	}, testutil.AuthHeader(adminToken))
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
	resp.Body.Close()
}

// TestCreateAPIToken_DeduplicatesScopes pins that repeating a scope in the
// request does not produce a duplicate entry in the stored array.
func TestCreateAPIToken_DeduplicatesScopes(t *testing.T) {
	resetDB(t)
	adminToken := env.SetupAdmin(t, "admin@test.com", "password123")

	resp := env.DoRequest(t, "POST", "/api/tokens", map[string]any{
		"name":   "dupe-scopes",
		"scopes": []string{"read", "read", "write"},
	}, testutil.AuthHeader(adminToken))
	require.Equal(t, http.StatusCreated, resp.StatusCode)
	body := testutil.ReadJSON(t, resp)

	gotScopes := body["scopes"].([]any)
	assert.Len(t, gotScopes, 2)
}

// TestCreateAPIToken_UnpinnedByDefault pins that a token minted without any
// project_ids is unpinned — every project the owner can reach, evaluated at
// use time — with no pin rows recorded for it.
func TestCreateAPIToken_UnpinnedByDefault(t *testing.T) {
	resetDB(t)
	adminToken := env.SetupAdmin(t, "admin@test.com", "password123")

	resp := env.DoRequest(t, "POST", "/api/tokens", map[string]any{
		"name":   "unpinned",
		"scopes": service.AllScopes,
	}, testutil.AuthHeader(adminToken))
	require.Equal(t, http.StatusCreated, resp.StatusCode)
	body := testutil.ReadJSON(t, resp)
	assert.Equal(t, false, body["pinned"])
	assert.Empty(t, body["project_ids"])
	id := body["id"].(string)

	var idUUID pgtype.UUID
	require.NoError(t, idUUID.Scan(id))
	var pinned bool
	require.NoError(t, env.Pool.QueryRow(context.Background(),
		"SELECT pinned FROM api_tokens WHERE id = $1", idUUID).Scan(&pinned))
	assert.False(t, pinned)
	var pinRows int
	require.NoError(t, env.Pool.QueryRow(context.Background(),
		"SELECT count(*) FROM api_token_projects WHERE token_id = $1", idUUID).Scan(&pinRows))
	assert.Zero(t, pinRows)
}

// TestCreateAPIToken_PinsToReachableProject pins the create endpoint's other
// half: a project_ids entry the caller can reach is stored and echoed back,
// not silently dropped the way it was before this test existed (every token
// ever minted through the product was unpinned regardless of what, if
// anything, the caller might have wanted to pin — the service and handler
// simply never carried the field through).
func TestCreateAPIToken_PinsToReachableProject(t *testing.T) {
	resetDB(t)
	adminToken := env.SetupAdmin(t, "admin@test.com", "password123")
	project := env.CreateProject(t, adminToken, "Pin Target", "pin-target")
	projectID := extractID(project["id"])

	resp := env.DoRequest(t, "POST", "/api/tokens", map[string]any{
		"name":        "pinned",
		"scopes":      service.AllScopes,
		"project_ids": []string{projectID},
	}, testutil.AuthHeader(adminToken))
	require.Equal(t, http.StatusCreated, resp.StatusCode)
	body := testutil.ReadJSON(t, resp)
	assert.Equal(t, true, body["pinned"])
	assert.ElementsMatch(t, []string{projectID}, body["project_ids"])

	var idUUID pgtype.UUID
	require.NoError(t, idUUID.Scan(body["id"].(string)))
	var storedProjectID string
	require.NoError(t, env.Pool.QueryRow(context.Background(),
		"SELECT project_id::text FROM api_token_projects WHERE token_id = $1", idUUID).Scan(&storedProjectID))
	assert.Equal(t, projectID, storedProjectID)

	// The list endpoint must carry the pin too — apiTokenDTO used to drop
	// project_id even though the query underneath it already selected it.
	listResp := env.DoRequest(t, "GET", "/api/tokens", nil, testutil.AuthHeader(adminToken))
	require.Equal(t, http.StatusOK, listResp.StatusCode)
	items := testutil.ReadJSONArray(t, listResp)
	require.Len(t, items, 1)
	assert.ElementsMatch(t, []string{projectID}, items[0].(map[string]any)["project_ids"])
}

// TestCreateAPIToken_PinsToMultipleReachableProjects pins the actual point of
// this rework: a token can narrow to MORE than one project, each checked
// independently, and every one of them is enforced afterward (a third,
// unlisted project is rejected by RequirePinnedProjectMatch).
func TestCreateAPIToken_PinsToMultipleReachableProjects(t *testing.T) {
	resetDB(t)
	adminToken := env.SetupAdmin(t, "admin@test.com", "password123")
	projectA := env.CreateProject(t, adminToken, "Project A", "project-a")
	projectAID := extractID(projectA["id"])
	projectB := env.CreateProject(t, adminToken, "Project B", "project-b")
	projectBID := extractID(projectB["id"])
	projectC := env.CreateProject(t, adminToken, "Project C", "project-c")
	projectCID := extractID(projectC["id"])

	resp := env.DoRequest(t, "POST", "/api/tokens", map[string]any{
		"name":        "multi-pinned",
		"scopes":      []string{"read"},
		"project_ids": []string{projectAID, projectBID},
	}, testutil.AuthHeader(adminToken))
	require.Equal(t, http.StatusCreated, resp.StatusCode)
	body := testutil.ReadJSON(t, resp)
	assert.Equal(t, true, body["pinned"])
	assert.ElementsMatch(t, []string{projectAID, projectBID}, body["project_ids"])
	plain := body["token"].(string)

	aResp := env.DoRequest(t, "GET", "/api/projects/"+projectAID, nil, testutil.AuthHeader(plain))
	assert.Equal(t, http.StatusOK, aResp.StatusCode, "the first pinned project must be reachable")
	aResp.Body.Close()

	bResp := env.DoRequest(t, "GET", "/api/projects/"+projectBID, nil, testutil.AuthHeader(plain))
	assert.Equal(t, http.StatusOK, bResp.StatusCode, "the second pinned project must be reachable")
	bResp.Body.Close()

	cResp := env.DoRequest(t, "GET", "/api/projects/"+projectCID, nil, testutil.AuthHeader(plain))
	assert.Equal(t, http.StatusForbidden, cResp.StatusCode, "a project outside the pin set must stay forbidden")
	cResp.Body.Close()
}

// TestCreateAPIToken_DeduplicatesCaseInsensitiveProjectID is a regression
// test for a review finding: deduplicating project_ids on the raw request
// string (before UUID parsing) let two differently-cased strings for the
// SAME project both survive into the pin insert, which failed on its own
// primary key and turned a harmless repeat into a 500 instead of a single
// pin. Dedup now happens on the parsed id.
func TestCreateAPIToken_DeduplicatesCaseInsensitiveProjectID(t *testing.T) {
	resetDB(t)
	adminToken := env.SetupAdmin(t, "admin@test.com", "password123")
	project := env.CreateProject(t, adminToken, "Dup Case", "dup-case")
	projectID := extractID(project["id"])

	resp := env.DoRequest(t, "POST", "/api/tokens", map[string]any{
		"name":        "dup-case-ids",
		"scopes":      service.AllScopes,
		"project_ids": []string{projectID, strings.ToUpper(projectID)},
	}, testutil.AuthHeader(adminToken))
	require.Equal(t, http.StatusCreated, resp.StatusCode)
	body := testutil.ReadJSON(t, resp)
	assert.ElementsMatch(t, []string{projectID}, body["project_ids"],
		"the two case variants of the same id must collapse to one pin, not two")
}

// TestCreateAPIToken_RejectsTooManyPinnedProjects pins the bound that stops
// an authenticated caller from turning one create request into hundreds of
// serial canAccessProject/GetProject round trips.
func TestCreateAPIToken_RejectsTooManyPinnedProjects(t *testing.T) {
	resetDB(t)
	adminToken := env.SetupAdmin(t, "admin@test.com", "password123")

	tooMany := make([]string, 101)
	for i := range tooMany {
		tooMany[i] = "00000000-0000-0000-0000-" + fmt.Sprintf("%012d", i)
	}

	resp := env.DoRequest(t, "POST", "/api/tokens", map[string]any{
		"name":        "too-many-pins",
		"scopes":      service.AllScopes,
		"project_ids": tooMany,
	}, testutil.AuthHeader(adminToken))
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
	resp.Body.Close()
}

// TestCreateAPIToken_RejectsUnreachableProject pins the validation half: a
// Member cannot pin a token to a project they cannot otherwise reach — the
// creation-time check is UX (fail fast with a clear error), not the security
// boundary, but it must still fail here rather than silently minting a token
// pinned to something its owner can never use.
func TestCreateAPIToken_RejectsUnreachableProject(t *testing.T) {
	resetDB(t)
	adminToken := env.SetupAdmin(t, "admin@test.com", "password123")
	adminProject := env.CreateProject(t, adminToken, "Admin Only", "admin-only")
	adminProjectID := extractID(adminProject["id"])
	_, memberToken := createMember(t, adminToken, "member@test.com")

	resp := env.DoRequest(t, "POST", "/api/tokens", map[string]any{
		"name":        "reaches-too-far",
		"scopes":      service.AllScopes,
		"project_ids": []string{adminProjectID},
	}, testutil.AuthHeader(memberToken))
	assert.Equal(t, http.StatusForbidden, resp.StatusCode)
	resp.Body.Close()
}

// TestCreateAPIToken_MemberCanPinToSharedProject is the case
// canAccessProject (rather than isProjectOwner) exists for: a Member must be
// able to pin a token to a project shared with them, not only one they own,
// or the feature is useless to exactly the people most likely to want a
// narrower token than their own account-wide reach.
func TestCreateAPIToken_MemberCanPinToSharedProject(t *testing.T) {
	resetDB(t)
	adminToken := env.SetupAdmin(t, "admin@test.com", "password123")
	project := env.CreateProject(t, adminToken, "Shared Project", "shared-project")
	projectID := extractID(project["id"])
	_, memberToken := createMember(t, adminToken, "member@test.com")

	shareResp := env.DoRequest(t, "PUT", fmt.Sprintf("/api/projects/%s/sharing", projectID),
		map[string]bool{"shared": true}, testutil.AuthHeader(adminToken))
	require.Equal(t, http.StatusOK, shareResp.StatusCode)
	shareResp.Body.Close()

	resp := env.DoRequest(t, "POST", "/api/tokens", map[string]any{
		"name":        "pinned-to-shared",
		"scopes":      []string{"read"},
		"project_ids": []string{projectID},
	}, testutil.AuthHeader(memberToken))
	require.Equal(t, http.StatusCreated, resp.StatusCode)
	body := testutil.ReadJSON(t, resp)
	assert.ElementsMatch(t, []string{projectID}, body["project_ids"])
}

// TestCreateAPIToken_RejectsMalformedProjectID pins that an unparseable
// project_id is a 400, distinct from the 403 an unreachable-but-valid id
// gets — a client sending garbage should not be told "access denied" for a
// project id that was never going to resolve to anything.
func TestCreateAPIToken_RejectsMalformedProjectID(t *testing.T) {
	resetDB(t)
	adminToken := env.SetupAdmin(t, "admin@test.com", "password123")

	resp := env.DoRequest(t, "POST", "/api/tokens", map[string]any{
		"name":        "bad-project-id",
		"scopes":      service.AllScopes,
		"project_ids": []string{"not-a-uuid"},
	}, testutil.AuthHeader(adminToken))
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
	resp.Body.Close()
}

// TestCreateAPIToken_AdminRejectsNonexistentProject pins the admin-specific
// path a Member never reaches: canAccessOwned (authz.go) short-circuits to
// true for an admin before ever looking the project up, so a well-formed
// but nonexistent project_id would otherwise sail past canAccessProject and
// surface as a 500 from the project_id foreign key at INSERT time instead
// of a 4xx. A Member gets 403 for the identical request (canAccessOwned's
// own lookup fails first), which TestCreateAPIToken_RejectsUnreachableProject
// already covers with an existing project a Member merely cannot reach —
// this one is the "doesn't exist at all" case, admin-only because that's
// the only role for which the distinction is even reachable.
func TestCreateAPIToken_AdminRejectsNonexistentProject(t *testing.T) {
	resetDB(t)
	adminToken := env.SetupAdmin(t, "admin@test.com", "password123")

	resp := env.DoRequest(t, "POST", "/api/tokens", map[string]any{
		"name":        "nonexistent-project",
		"scopes":      service.AllScopes,
		"project_ids": []string{"00000000-0000-0000-0000-000000000000"},
	}, testutil.AuthHeader(adminToken))
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
	resp.Body.Close()
}

// TestCreateAPIToken_MemberNonexistentProjectStaysForbidden is the other half
// of the test above, and it guards a property that rests entirely on the ORDER
// of two checks inside CreateAPIToken: canAccessProject runs first, the
// existence check second. Because of that order a non-admin never reaches the
// 404 — a nonexistent id is already collapsed into "access denied" by the
// access check, the same collapse that stops a token using a resource's mere
// existence as a cross-tenant oracle.
//
// Nothing about that is enforced by a type or a signature; hoist the existence
// check above the access check during some future refactor and Members start
// learning which project ids are real, while every other test in this file
// still passes. Hence this one: it fails the moment the order flips.
func TestCreateAPIToken_MemberNonexistentProjectStaysForbidden(t *testing.T) {
	resetDB(t)
	adminToken := env.SetupAdmin(t, "admin@test.com", "password123")
	_, memberToken := createMember(t, adminToken, "member@test.com")

	resp := env.DoRequest(t, "POST", "/api/tokens", map[string]any{
		"name":        "member-nonexistent-project",
		"scopes":      service.AllScopes,
		"project_ids": []string{"00000000-0000-0000-0000-000000000000"},
	}, testutil.AuthHeader(memberToken))
	defer resp.Body.Close()
	// Deliberately asserting the exact status, not merely "an error": a 404
	// here would be the leak, and it is the plausible regression.
	assert.Equal(t, http.StatusForbidden, resp.StatusCode,
		"a Member must not be able to tell a nonexistent project from one they cannot reach")
}

// TestCreateAPIToken_PinnedTokenEnforcesItsPin creates a pinned token
// through the real endpoint (not a direct DB insert) and proves
// middleware.RequirePinnedProjectMatch actually enforces the pin it stored — the
// end-to-end path that a direct-insert test helper would never exercise,
// which is exactly why this gap survived undetected.
func TestCreateAPIToken_PinnedTokenEnforcesItsPin(t *testing.T) {
	resetDB(t)
	adminToken := env.SetupAdmin(t, "admin@test.com", "password123")
	ownProject := env.CreateProject(t, adminToken, "Own Project", "own-project")
	ownProjectID := extractID(ownProject["id"])
	otherProject := env.CreateProject(t, adminToken, "Other Project", "other-project")
	otherProjectID := extractID(otherProject["id"])

	createResp := env.DoRequest(t, "POST", "/api/tokens", map[string]any{
		"name":        "pin-enforced",
		"scopes":      []string{"read"},
		"project_ids": []string{ownProjectID},
	}, testutil.AuthHeader(adminToken))
	require.Equal(t, http.StatusCreated, createResp.StatusCode)
	plain := testutil.ReadJSON(t, createResp)["token"].(string)

	ownResp := env.DoRequest(t, "GET", "/api/projects/"+ownProjectID, nil, testutil.AuthHeader(plain))
	assert.Equal(t, http.StatusOK, ownResp.StatusCode, "the pinned project itself must still work")
	ownResp.Body.Close()

	otherResp := env.DoRequest(t, "GET", "/api/projects/"+otherProjectID, nil, testutil.AuthHeader(plain))
	assert.Equal(t, http.StatusForbidden, otherResp.StatusCode, "a pin created through the real endpoint must be enforced, not just stored")
	otherBody := testutil.ReadJSON(t, otherResp)
	assert.Equal(t, "token is pinned to a different project", otherBody["error"])
}

// TestListAPITokens_ScopedToCallingUser pins per-user isolation: there is no
// cross-user token oversight view in v1, admin or not.
func TestListAPITokens_ScopedToCallingUser(t *testing.T) {
	resetDB(t)
	adminToken := env.SetupAdmin(t, "admin@test.com", "password123")
	_, memberToken := createMember(t, adminToken, "member@test.com")

	resp := env.DoRequest(t, "POST", "/api/tokens", map[string]any{
		"name":   "admin token",
		"scopes": service.AllScopes,
	}, testutil.AuthHeader(adminToken))
	require.Equal(t, http.StatusCreated, resp.StatusCode)
	resp.Body.Close()

	memberList := env.DoRequest(t, "GET", "/api/tokens", nil, testutil.AuthHeader(memberToken))
	require.Equal(t, http.StatusOK, memberList.StatusCode)
	assert.Len(t, testutil.ReadJSONArray(t, memberList), 0, "a member must not see the admin's tokens")

	adminList := env.DoRequest(t, "GET", "/api/tokens", nil, testutil.AuthHeader(adminToken))
	require.Equal(t, http.StatusOK, adminList.StatusCode)
	assert.Len(t, testutil.ReadJSONArray(t, adminList), 1)
}

// TestDeleteAPIToken_OwnerCanDeleteAndItStopsAuthenticating pins the full
// revoke path end to end.
func TestDeleteAPIToken_OwnerCanDeleteAndItStopsAuthenticating(t *testing.T) {
	resetDB(t)
	adminToken := env.SetupAdmin(t, "admin@test.com", "password123")

	createResp := env.DoRequest(t, "POST", "/api/tokens", map[string]any{
		"name":   "revoke-me",
		"scopes": service.AllScopes,
	}, testutil.AuthHeader(adminToken))
	require.Equal(t, http.StatusCreated, createResp.StatusCode)
	created := testutil.ReadJSON(t, createResp)
	id := created["id"].(string)
	plain := created["token"].(string)

	delResp := env.DoRequest(t, "DELETE", "/api/tokens/"+id, nil, testutil.AuthHeader(adminToken))
	assert.Equal(t, http.StatusOK, delResp.StatusCode)
	delResp.Body.Close()

	resp := env.DoRequest(t, "GET", "/api/projects", nil, testutil.AuthHeader(plain))
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode, "a deleted token must stop authenticating immediately")
	resp.Body.Close()
}

// TestDeleteAPIToken_CannotDeleteAnotherUsersToken pins that ownership is
// enforced by the query shape itself (WHERE id = $1 AND user_id = $2), not a
// separate check that could be forgotten on a future endpoint.
func TestDeleteAPIToken_CannotDeleteAnotherUsersToken(t *testing.T) {
	resetDB(t)
	adminToken := env.SetupAdmin(t, "admin@test.com", "password123")
	_, memberToken := createMember(t, adminToken, "member@test.com")

	createResp := env.DoRequest(t, "POST", "/api/tokens", map[string]any{
		"name":   "admin-owned",
		"scopes": service.AllScopes,
	}, testutil.AuthHeader(adminToken))
	require.Equal(t, http.StatusCreated, createResp.StatusCode)
	created := testutil.ReadJSON(t, createResp)
	id := created["id"].(string)
	plain := created["token"].(string)

	delResp := env.DoRequest(t, "DELETE", "/api/tokens/"+id, nil, testutil.AuthHeader(memberToken))
	assert.Equal(t, http.StatusNotFound, delResp.StatusCode, "a token belonging to someone else must read as not-found, not forbidden")
	delResp.Body.Close()

	// Still very much alive.
	resp := env.DoRequest(t, "GET", "/api/projects", nil, testutil.AuthHeader(plain))
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	resp.Body.Close()
}

// TestDeleteAPIToken_UnknownIDNotFound pins the not-found path for an id that
// simply does not exist.
func TestDeleteAPIToken_UnknownIDNotFound(t *testing.T) {
	resetDB(t)
	adminToken := env.SetupAdmin(t, "admin@test.com", "password123")

	resp := env.DoRequest(t, "DELETE", "/api/tokens/00000000-0000-0000-0000-000000000000", nil, testutil.AuthHeader(adminToken))
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
	resp.Body.Close()
}

// TestDeleteAPIToken_SelfRevocationSucceeds is a regression test for a window
// project_v016_plan flagged explicitly: a token deleting itself (the same
// credential authenticating the DELETE request that removes its own row).
// This is now rejected: minting and revoking tokens requires a live session
// (middleware.RequireSession), specifically because a PAT could otherwise
// mint itself a longer-lived replacement and then revoke the original to
// cover its tracks — a self-propagation path scope enforcement alone cannot
// close, since token creation is a legitimate "write" action. The token used
// to authenticate this request must survive, unaffected by the rejection.
func TestDeleteAPIToken_PATCannotRevokeItself(t *testing.T) {
	resetDB(t)
	adminToken := env.SetupAdmin(t, "admin@test.com", "password123")

	createResp := env.DoRequest(t, "POST", "/api/tokens", map[string]any{
		"name":   "self-revoke",
		"scopes": service.AllScopes,
	}, testutil.AuthHeader(adminToken))
	require.Equal(t, http.StatusCreated, createResp.StatusCode)
	created := testutil.ReadJSON(t, createResp)
	id := created["id"].(string)
	plain := created["token"].(string)

	// Authenticate the DELETE with the very token being deleted.
	delResp := env.DoRequest(t, "DELETE", "/api/tokens/"+id, nil, testutil.AuthHeader(plain))
	assert.Equal(t, http.StatusForbidden, delResp.StatusCode)
	delResp.Body.Close()

	// Still alive — the rejection must not have deleted it anyway.
	resp := env.DoRequest(t, "GET", "/api/projects", nil, testutil.AuthHeader(plain))
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	resp.Body.Close()
}

// TestDeleteAPIToken_PATCannotRevokeAnyToken pins that the session
// requirement is not limited to a token deleting itself — a PAT is rejected
// on this route regardless of whose token id it names, including one of its
// own owner's OTHER tokens.
func TestDeleteAPIToken_PATCannotRevokeAnyToken(t *testing.T) {
	resetDB(t)
	adminToken := env.SetupAdmin(t, "admin@test.com", "password123")

	authTokenResp := env.DoRequest(t, "POST", "/api/tokens", map[string]any{
		"name":   "authenticator",
		"scopes": service.AllScopes,
	}, testutil.AuthHeader(adminToken))
	require.Equal(t, http.StatusCreated, authTokenResp.StatusCode)
	authPlain := testutil.ReadJSON(t, authTokenResp)["token"].(string)

	targetResp := env.DoRequest(t, "POST", "/api/tokens", map[string]any{
		"name":   "target",
		"scopes": service.AllScopes,
	}, testutil.AuthHeader(adminToken))
	require.Equal(t, http.StatusCreated, targetResp.StatusCode)
	targetID := testutil.ReadJSON(t, targetResp)["id"].(string)

	delResp := env.DoRequest(t, "DELETE", "/api/tokens/"+targetID, nil, testutil.AuthHeader(authPlain))
	assert.Equal(t, http.StatusForbidden, delResp.StatusCode)
	delResp.Body.Close()
}

// TestCreateAPIToken_RequiresSession pins the other half of the same fix: a
// PAT cannot mint a replacement token for itself either.
func TestCreateAPIToken_RequiresSession(t *testing.T) {
	resetDB(t)
	adminToken := env.SetupAdmin(t, "admin@test.com", "password123")

	createResp := env.DoRequest(t, "POST", "/api/tokens", map[string]any{
		"name":   "authenticator",
		"scopes": service.AllScopes,
	}, testutil.AuthHeader(adminToken))
	require.Equal(t, http.StatusCreated, createResp.StatusCode)
	plain := testutil.ReadJSON(t, createResp)["token"].(string)

	resp := env.DoRequest(t, "POST", "/api/tokens", map[string]any{
		"name":   "smuggled-replacement",
		"scopes": service.AllScopes,
	}, testutil.AuthHeader(plain))
	assert.Equal(t, http.StatusForbidden, resp.StatusCode)
	resp.Body.Close()
}

// TestListAPITokens_RequiresSession pins that listing joined minting and
// revoking behind a live session: it returns every token's id, name,
// scopes, role_at_issue, and last-used/expiry timestamps, which — even with
// values correctly masked — is a credential inventory a leaked low-scope
// token could use to map out which other tokens exist, which one holds
// write, and which id DeleteAPIToken takes. Was PAT-accessible originally
// (as read-only); the user decided a PAT should manage infrastructure, not
// enumerate credentials, so this now matches TestCreateAPIToken_RequiresSession
// and TestDeleteAPIToken_PATCannotRevokeItself's shape rather than standing
// apart from them.
func TestListAPITokens_RequiresSession(t *testing.T) {
	resetDB(t)
	adminToken := env.SetupAdmin(t, "admin@test.com", "password123")

	createResp := env.DoRequest(t, "POST", "/api/tokens", map[string]any{
		"name":   "list-via-pat",
		"scopes": service.AllScopes,
	}, testutil.AuthHeader(adminToken))
	require.Equal(t, http.StatusCreated, createResp.StatusCode)
	plain := testutil.ReadJSON(t, createResp)["token"].(string)

	resp := env.DoRequest(t, "GET", "/api/tokens", nil, testutil.AuthHeader(plain))
	body := testutil.ReadJSON(t, resp)
	assert.Equal(t, http.StatusForbidden, resp.StatusCode)
	assert.Equal(t, "this action requires a session, not a personal access token", body["error"])
}
