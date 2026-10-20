package handler_test

import (
	"context"
	"fmt"
	"net/http"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/belune-hq/belune/internal/store/generated"
	"github.com/belune-hq/belune/internal/testutil"
)

// createMember creates a member user and returns (id, token).
func createMember(t *testing.T, adminToken, email string) (string, string) {
	t.Helper()
	resp := env.DoRequest(t, "POST", "/api/users", map[string]string{
		"email":    email,
		"password": "password123",
		"role":     "member",
	}, testutil.AuthHeader(adminToken))
	body := testutil.ReadJSON(t, resp)
	id := extractID(body["id"])
	return id, env.LoginAs(t, email, "password123")
}

// mergeMap returns a new map with base's entries overridden by overrides'.
func mergeMap(base, overrides map[string]any) map[string]any {
	out := make(map[string]any, len(base)+len(overrides))
	for k, v := range base {
		out[k] = v
	}
	for k, v := range overrides {
		out[k] = v
	}
	return out
}

// TestUpdateProjectSharing_WidensAccessToEveryMember pins the core behaviour:
// a shared project becomes reachable — read, list, stats — by a Member who is
// neither its owner nor an admin, and reverts when unshared.
func TestUpdateProjectSharing_WidensAccessToEveryMember(t *testing.T) {
	resetDB(t)
	adminToken := env.SetupAdmin(t, "admin@test.com", "password123")
	_, ownerToken := createMember(t, adminToken, "owner@test.com")
	_, otherToken := createMember(t, adminToken, "other@test.com")

	project := env.CreateProject(t, ownerToken, "Team Project", "team-project")
	projectID := extractID(project["id"])

	// Not shared yet: other member is blocked, and does not see it listed.
	resp := env.DoRequest(t, "GET", fmt.Sprintf("/api/projects/%s", projectID), nil, testutil.AuthHeader(otherToken))
	assert.Equal(t, http.StatusForbidden, resp.StatusCode)
	resp.Body.Close()

	resp = env.DoRequest(t, "GET", "/api/projects", nil, testutil.AuthHeader(otherToken))
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Len(t, testutil.ReadJSONArray(t, resp), 0)

	// A Member who owns the project can share it — not admin-only.
	resp = env.DoRequest(t, "PUT", fmt.Sprintf("/api/projects/%s/sharing", projectID), map[string]bool{
		"shared": true,
	}, testutil.AuthHeader(ownerToken))
	require.Equal(t, http.StatusOK, resp.StatusCode)
	shared := testutil.ReadJSON(t, resp)
	assert.Equal(t, true, shared["shared"])

	// Now the other member can read it, and sees it in their list.
	resp = env.DoRequest(t, "GET", fmt.Sprintf("/api/projects/%s", projectID), nil, testutil.AuthHeader(otherToken))
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	resp.Body.Close()

	resp = env.DoRequest(t, "GET", "/api/projects", nil, testutil.AuthHeader(otherToken))
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Len(t, testutil.ReadJSONArray(t, resp), 1)

	// A stats aggregate scoped by user must also include the shared project.
	env.CreateApplication(t, ownerToken, projectID, map[string]any{
		"name":        "Shared App",
		"type":        "git",
		"build_type":  "dockerfile",
		"source_repo": "https://github.com/test/repo",
	})
	resp = env.DoRequest(t, "GET", "/api/stats", nil, testutil.AuthHeader(otherToken))
	require.Equal(t, http.StatusOK, resp.StatusCode)
	stats := testutil.ReadJSON(t, resp)
	appHealth := stats["app_health"].(map[string]any)
	assert.EqualValues(t, 1, appHealth["total"], "the shared project's application must count toward a member's stats")

	// Unsharing revokes access again.
	resp = env.DoRequest(t, "PUT", fmt.Sprintf("/api/projects/%s/sharing", projectID), map[string]bool{
		"shared": false,
	}, testutil.AuthHeader(ownerToken))
	require.Equal(t, http.StatusOK, resp.StatusCode)
	resp.Body.Close()

	resp = env.DoRequest(t, "GET", fmt.Sprintf("/api/projects/%s", projectID), nil, testutil.AuthHeader(otherToken))
	assert.Equal(t, http.StatusForbidden, resp.StatusCode)
	resp.Body.Close()
}

// TestUpdateProjectSharing_OwnerOnly pins that sharing access does NOT grant
// the right to change sharing, delete, or transfer the project — those stay
// owner (or admin) only, even once a member can otherwise use the project.
func TestUpdateProjectSharing_OwnerOnly(t *testing.T) {
	resetDB(t)
	adminToken := env.SetupAdmin(t, "admin@test.com", "password123")
	_, ownerToken := createMember(t, adminToken, "owner@test.com")
	_, otherToken := createMember(t, adminToken, "other@test.com")

	project := env.CreateProject(t, ownerToken, "Owner Only", "owner-only")
	projectID := extractID(project["id"])

	// Non-owner cannot share a project it does not own.
	resp := env.DoRequest(t, "PUT", fmt.Sprintf("/api/projects/%s/sharing", projectID), map[string]bool{
		"shared": true,
	}, testutil.AuthHeader(otherToken))
	assert.Equal(t, http.StatusForbidden, resp.StatusCode)
	resp.Body.Close()

	// Admin shares it on the owner's behalf.
	resp = env.DoRequest(t, "PUT", fmt.Sprintf("/api/projects/%s/sharing", projectID), map[string]bool{
		"shared": true,
	}, testutil.AuthHeader(adminToken))
	require.Equal(t, http.StatusOK, resp.StatusCode)
	resp.Body.Close()

	// The other member now has access, but still cannot unshare or delete it.
	resp = env.DoRequest(t, "GET", fmt.Sprintf("/api/projects/%s", projectID), nil, testutil.AuthHeader(otherToken))
	require.Equal(t, http.StatusOK, resp.StatusCode)
	resp.Body.Close()

	resp = env.DoRequest(t, "PUT", fmt.Sprintf("/api/projects/%s/sharing", projectID), map[string]bool{
		"shared": false,
	}, testutil.AuthHeader(otherToken))
	assert.Equal(t, http.StatusForbidden, resp.StatusCode, "shared access must not grant the right to unshare")
	resp.Body.Close()

	resp = env.DoRequest(t, "DELETE", fmt.Sprintf("/api/projects/%s", projectID), nil, testutil.AuthHeader(otherToken))
	assert.Equal(t, http.StatusForbidden, resp.StatusCode, "shared access must not grant delete rights")
	resp.Body.Close()
}

// TestUpdateProjectSharing_QuotaStaysOwnerScoped is the trap the plan called
// out explicitly: quotas follow ownership, not access. A shared project's
// applications must count only against its owner's quota usage, never against
// a member who merely has shared access to it.
func TestUpdateProjectSharing_QuotaStaysOwnerScoped(t *testing.T) {
	resetDB(t)
	adminToken := env.SetupAdmin(t, "admin@test.com", "password123")
	ownerID, ownerToken := createMember(t, adminToken, "owner@test.com")
	otherID, _ := createMember(t, adminToken, "other@test.com")

	project := env.CreateProject(t, ownerToken, "Quota Project", "quota-project")
	projectID := extractID(project["id"])
	env.CreateApplication(t, ownerToken, projectID, map[string]any{
		"name":        "App",
		"type":        "git",
		"build_type":  "dockerfile",
		"source_repo": "https://github.com/test/repo",
	})

	resp := env.DoRequest(t, "PUT", fmt.Sprintf("/api/projects/%s/sharing", projectID), map[string]bool{
		"shared": true,
	}, testutil.AuthHeader(ownerToken))
	require.Equal(t, http.StatusOK, resp.StatusCode)
	resp.Body.Close()

	resp = env.DoRequest(t, "GET", fmt.Sprintf("/api/quotas/user/%s", ownerID), nil, testutil.AuthHeader(adminToken))
	require.Equal(t, http.StatusOK, resp.StatusCode)
	ownerUsage := testutil.ReadJSON(t, resp)["usage"].(map[string]any)
	assert.EqualValues(t, 1, ownerUsage["applications"], "the owner's quota usage must count the application")

	resp = env.DoRequest(t, "GET", fmt.Sprintf("/api/quotas/user/%s", otherID), nil, testutil.AuthHeader(adminToken))
	require.Equal(t, http.StatusOK, resp.StatusCode)
	otherUsage := testutil.ReadJSON(t, resp)["usage"].(map[string]any)
	assert.EqualValues(t, 0, otherUsage["applications"],
		"a shared project's applications must NOT count against a non-owner's quota")
}

// TestUpdateProjectSharing_MemberCanUseButNotDestroy pins the boundary a code
// review caught: sharing must grant a Member full operational use of the
// project's applications, databases, and domains, but NEVER the right to
// destroy them. Only the owner (or an admin) may delete an application,
// delete a database, or remove a domain — even once shared access already
// lets that Member reach, deploy, and manage those resources.
func TestUpdateProjectSharing_MemberCanUseButNotDestroy(t *testing.T) {
	resetDB(t)
	adminToken := env.SetupAdmin(t, "admin@test.com", "password123")
	_, ownerToken := createMember(t, adminToken, "owner@test.com")
	_, otherToken := createMember(t, adminToken, "other@test.com")

	project := env.CreateProject(t, ownerToken, "Use Not Destroy", "use-not-destroy")
	projectID := extractID(project["id"])

	app := env.CreateApplication(t, ownerToken, projectID, map[string]any{
		"name":        "App",
		"type":        "git",
		"build_type":  "dockerfile",
		"source_repo": "https://github.com/test/repo",
	})
	appID := extractID(app["id"])

	dbResp := env.DoRequest(t, "POST", fmt.Sprintf("/api/projects/%s/databases", projectID), map[string]any{
		"name": "db", "type": "postgres",
	}, testutil.AuthHeader(ownerToken))
	require.Equal(t, http.StatusAccepted, dbResp.StatusCode)
	dbID := extractID(testutil.ReadJSON(t, dbResp)["id"])

	domainResp := env.DoRequest(t, "POST", fmt.Sprintf("/api/projects/%s/applications/%s/domains", projectID, appID), map[string]any{
		"hostname": "shared.example.com",
	}, testutil.AuthHeader(ownerToken))
	require.Equal(t, http.StatusCreated, domainResp.StatusCode)
	domainID := extractID(testutil.ReadJSON(t, domainResp)["id"])

	shareResp := env.DoRequest(t, "PUT", fmt.Sprintf("/api/projects/%s/sharing", projectID), map[string]bool{
		"shared": true,
	}, testutil.AuthHeader(ownerToken))
	require.Equal(t, http.StatusOK, shareResp.StatusCode)
	shareResp.Body.Close()

	// Positive: shared access reaches the application, database, and domain.
	resp := env.DoRequest(t, "GET", fmt.Sprintf("/api/projects/%s/applications/%s", projectID, appID), nil, testutil.AuthHeader(otherToken))
	assert.Equal(t, http.StatusOK, resp.StatusCode, "a shared member must be able to read the application")
	resp.Body.Close()

	resp = env.DoRequest(t, "GET", fmt.Sprintf("/api/projects/%s/databases/%s", projectID, dbID), nil, testutil.AuthHeader(otherToken))
	assert.Equal(t, http.StatusOK, resp.StatusCode, "a shared member must be able to read the database")
	resp.Body.Close()

	resp = env.DoRequest(t, "GET", fmt.Sprintf("/api/projects/%s/applications/%s/domains", projectID, appID), nil, testutil.AuthHeader(otherToken))
	assert.Equal(t, http.StatusOK, resp.StatusCode, "a shared member must be able to list domains")
	resp.Body.Close()

	// Negative: shared access does NOT reach delete.
	resp = env.DoRequest(t, "DELETE", fmt.Sprintf("/api/projects/%s/applications/%s", projectID, appID), nil, testutil.AuthHeader(otherToken))
	assert.Equal(t, http.StatusForbidden, resp.StatusCode, "a shared member must not be able to delete the application")
	resp.Body.Close()

	resp = env.DoRequest(t, "DELETE", fmt.Sprintf("/api/projects/%s/databases/%s", projectID, dbID), nil, testutil.AuthHeader(otherToken))
	assert.Equal(t, http.StatusForbidden, resp.StatusCode, "a shared member must not be able to delete the database")
	resp.Body.Close()

	resp = env.DoRequest(t, "DELETE", fmt.Sprintf("/api/projects/%s/applications/%s/domains/%s", projectID, appID, domainID), nil, testutil.AuthHeader(otherToken))
	assert.Equal(t, http.StatusForbidden, resp.StatusCode, "a shared member must not be able to remove the domain")
	resp.Body.Close()

	// The owner retains full destroy rights throughout.
	resp = env.DoRequest(t, "DELETE", fmt.Sprintf("/api/projects/%s/applications/%s/domains/%s", projectID, appID, domainID), nil, testutil.AuthHeader(ownerToken))
	assert.Equal(t, http.StatusOK, resp.StatusCode, "the owner must still be able to remove the domain")
	resp.Body.Close()

	resp = env.DoRequest(t, "DELETE", fmt.Sprintf("/api/projects/%s/databases/%s", projectID, dbID), nil, testutil.AuthHeader(ownerToken))
	assert.Equal(t, http.StatusOK, resp.StatusCode, "the owner must still be able to delete the database")
	resp.Body.Close()

	resp = env.DoRequest(t, "DELETE", fmt.Sprintf("/api/projects/%s/applications/%s", projectID, appID), nil, testutil.AuthHeader(ownerToken))
	assert.Equal(t, http.StatusOK, resp.StatusCode, "the owner must still be able to delete the application")
	resp.Body.Close()
}

// TestGitIntegration_CannotBeAttachedByNonOwner pins the fix for the review's
// second finding: a git_integration_id is user-level, not project-level, so
// sharing must never let it be attached to an application by anyone but the
// integration's own owner (or an admin) — whether via CreateApplication,
// UpdateApplication, or ChangeApplicationSource. This predates project
// sharing, but sharing is what makes a real git_integration_id discoverable
// (readable off a shared application's JSON) instead of requiring a guess.
func TestGitIntegration_CannotBeAttachedByNonOwner(t *testing.T) {
	resetDB(t)
	ctx := context.Background()
	adminToken := env.SetupAdmin(t, "admin@test.com", "password123")
	ownerID, ownerToken := createMember(t, adminToken, "owner@test.com")
	_, otherToken := createMember(t, adminToken, "other@test.com")

	var ownerUUID pgtype.UUID
	require.NoError(t, ownerUUID.Scan(ownerID))
	integration, err := env.Queries.CreateGitIntegration(ctx, generated.CreateGitIntegrationParams{
		Provider:        "github",
		BaseUrl:         "https://github.com",
		AccountLogin:    "owner-account",
		ConfigEncrypted: []byte("dummy"),
		CreatedBy:       ownerUUID,
	})
	require.NoError(t, err)
	integrationID := idStr(integration.ID)

	// A second integration, owned by the same user, so the update test below
	// attempts a real attachment CHANGE — not a same-value resubmit, which the
	// fix deliberately allows regardless of who sends it.
	integration2, err := env.Queries.CreateGitIntegration(ctx, generated.CreateGitIntegrationParams{
		Provider:        "github",
		BaseUrl:         "https://github.com",
		AccountLogin:    "owner-account-2",
		ConfigEncrypted: []byte("dummy"),
		CreatedBy:       ownerUUID,
	})
	require.NoError(t, err)
	integration2ID := idStr(integration2.ID)

	otherProject := env.CreateProject(t, otherToken, "Other's Project", "others-project")
	otherProjectID := extractID(otherProject["id"])

	// Cannot attach on create.
	resp := env.DoRequest(t, "POST", fmt.Sprintf("/api/projects/%s/applications", otherProjectID), map[string]any{
		"name": "App", "type": "git", "build_type": "dockerfile",
		"source_repo":        "https://github.com/test/repo",
		"git_integration_id": integrationID,
	}, testutil.AuthHeader(otherToken))
	assert.Equal(t, http.StatusForbidden, resp.StatusCode, "attaching another user's git integration on create must be rejected")
	resp.Body.Close()

	// The owner CAN use their own integration.
	ownerProject := env.CreateProject(t, ownerToken, "Owner's Project", "owners-project")
	ownerProjectID := extractID(ownerProject["id"])
	ownerApp := env.CreateApplication(t, ownerToken, ownerProjectID, map[string]any{
		"name":               "App",
		"type":               "git",
		"build_type":         "dockerfile",
		"source_repo":        "https://github.com/test/repo",
		"git_integration_id": integrationID,
	})
	appID := extractID(ownerApp["id"])
	assert.Equal(t, integrationID, ownerApp["git_integration_id"], "the owner must be able to attach their own integration")

	// Cannot CHANGE the attachment on update, even by a Member who has shared
	// access to the application's project — sharing is project-level,
	// integrations are not.
	shareResp := env.DoRequest(t, "PUT", fmt.Sprintf("/api/projects/%s/sharing", ownerProjectID), map[string]bool{
		"shared": true,
	}, testutil.AuthHeader(ownerToken))
	require.Equal(t, http.StatusOK, shareResp.StatusCode)
	shareResp.Body.Close()

	// UpdateApplication is a full-row update (see CLAUDE.md): name and
	// source_repo must ride along on every PUT, or validateSource itself
	// 400s before the ownership check is even reached. Sending them
	// unchanged isolates what these assertions are actually testing.
	baseBody := map[string]any{
		"name":        "App",
		"source_repo": "https://github.com/test/repo",
	}

	resp = env.DoRequest(t, "PUT", fmt.Sprintf("/api/projects/%s/applications/%s", ownerProjectID, appID),
		mergeMap(baseBody, map[string]any{"git_integration_id": integration2ID}),
		testutil.AuthHeader(otherToken))
	assert.Equal(t, http.StatusForbidden, resp.StatusCode, "attaching a different git integration the caller does not own must be rejected, even with shared access to the application")
	resp.Body.Close()

	// Resubmitting the SAME already-attached integration must NOT be blocked
	// — that's not an attachment change, just a client PUTing the resource
	// back unmodified, and a shared member must not be punished for it.
	resp = env.DoRequest(t, "PUT", fmt.Sprintf("/api/projects/%s/applications/%s", ownerProjectID, appID),
		mergeMap(baseBody, map[string]any{"git_integration_id": integrationID}),
		testutil.AuthHeader(otherToken))
	assert.Equal(t, http.StatusOK, resp.StatusCode, "resubmitting the already-attached integration must not be treated as a new attachment")
	resp.Body.Close()

	// Omitting the field (preserve current) must NOT be blocked by the shared
	// member's lack of ownership of the already-attached integration.
	resp = env.DoRequest(t, "PUT", fmt.Sprintf("/api/projects/%s/applications/%s", ownerProjectID, appID),
		mergeMap(baseBody, map[string]any{"name": "Renamed"}),
		testutil.AuthHeader(otherToken))
	assert.Equal(t, http.StatusOK, resp.StatusCode, "a shared member editing unrelated fields must not be blocked by an integration they don't own")
	resp.Body.Close()
}

// TestCanAccessApplication_NotShared_StillBlocked is the negative-direction
// coverage a review flagged as missing: a Member with no relationship to a
// PRIVATE project's application must stay blocked, so a bug that widened
// canAccessOwned's "OR shared" branch unconditionally would be caught here.
func TestCanAccessApplication_NotShared_StillBlocked(t *testing.T) {
	resetDB(t)
	adminToken := env.SetupAdmin(t, "admin@test.com", "password123")
	_, ownerToken := createMember(t, adminToken, "owner@test.com")
	_, otherToken := createMember(t, adminToken, "other@test.com")

	project := env.CreateProject(t, ownerToken, "Private", "private")
	projectID := extractID(project["id"])
	app := env.CreateApplication(t, ownerToken, projectID, map[string]any{
		"name":        "App",
		"type":        "git",
		"build_type":  "dockerfile",
		"source_repo": "https://github.com/test/repo",
	})
	appID := extractID(app["id"])

	resp := env.DoRequest(t, "GET", fmt.Sprintf("/api/projects/%s/applications/%s", projectID, appID), nil, testutil.AuthHeader(otherToken))
	assert.Equal(t, http.StatusForbidden, resp.StatusCode, "a non-member must stay blocked from a private project's application")
	resp.Body.Close()
}
