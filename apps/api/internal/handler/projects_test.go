package handler_test

import (
	"context"
	"fmt"
	"net/http"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/belune-hq/belune/internal/testutil"
)

func TestCreateProject(t *testing.T) {
	resetDB(t)
	token := env.SetupAdmin(t, "admin@test.com", "password123")

	// Valid creation
	project := env.CreateProject(t, token, "My Project", "my-project")
	assert.Equal(t, "My Project", project["name"])
	assert.Equal(t, "my-project", project["slug"])

	// Placed on the local server: the caller never picks, and server_id is
	// NOT NULL, so a project created without one would not have been stored.
	var placedOn pgtype.UUID
	require.NoError(t, placedOn.Scan(project["server_id"].(string)))
	assert.Equal(t, testutil.LocalServerID(t, context.Background(), env.Queries), placedOn)

	// Missing fields
	resp := env.DoRequest(t, "POST", "/api/projects", map[string]string{
		"name": "No Slug",
	}, testutil.AuthHeader(token))
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
	resp.Body.Close()
}

// TestServerPlacement_RestrictBlocksServersNotOwners pins both halves of the
// ON DELETE RESTRICT on projects.server_id. v0.1.3 shipped a RESTRICT that
// looked right and fired during an unrelated cascade, so this one was reasoned
// about rather than copied: it must stop a server being deleted out from under
// the projects placed on it, and must never stand in the way of deleting the
// user who owns them.
func TestServerPlacement_RestrictBlocksServersNotOwners(t *testing.T) {
	resetDB(t)
	ctx := context.Background()
	adminToken := env.SetupAdmin(t, "admin@test.com", "password123")

	resp := env.DoRequest(t, "POST", "/api/users", map[string]string{
		"email":    "owner@test.com",
		"password": "password123",
		"role":     "member",
	}, testutil.AuthHeader(adminToken))
	require.Equal(t, http.StatusCreated, resp.StatusCode)
	ownerID := extractID(testutil.ReadJSON(t, resp)["id"])

	ownerToken := env.LoginAs(t, "owner@test.com", "password123")
	env.CreateProject(t, ownerToken, "Owned", "owned")

	// The SQLSTATE, not just any error: ON DELETE SET NULL would also fail here,
	// with a not-null violation instead of a foreign-key one, and this test would
	// stay green while the guarantee it exists for was gone.
	_, err := env.Pool.Exec(ctx, "DELETE FROM servers WHERE is_local")
	var pgErr *pgconn.PgError
	require.ErrorAs(t, err, &pgErr, "a server still holding projects must not be deletable")
	require.Equal(t, "23503", pgErr.Code, "expected a foreign-key violation, got %s", pgErr.Message)

	resp = env.DoRequest(t, "DELETE", "/api/users/"+ownerID, nil, testutil.AuthHeader(adminToken))
	require.Equal(t, http.StatusOK, resp.StatusCode,
		"deleting a user who owns projects must not trip the server FK")
	resp.Body.Close()

	server, err := env.Queries.GetLocalServer(ctx)
	require.NoError(t, err, "the local server row outlives the projects placed on it")
	assert.True(t, server.IsLocal)
}

func TestGetProject(t *testing.T) {
	resetDB(t)
	adminToken := env.SetupAdmin(t, "admin@test.com", "password123")

	// Create a member
	env.DoRequest(t, "POST", "/api/users", map[string]string{
		"email":    "member@test.com",
		"password": "password123",
		"role":     "member",
	}, testutil.AuthHeader(adminToken)).Body.Close()
	memberToken := env.LoginAs(t, "member@test.com", "password123")

	// Admin creates project (owned by admin)
	project := env.CreateProject(t, adminToken, "Admin Project", "admin-project")
	projectID := extractID(project["id"])

	// Admin can get it
	resp := env.DoRequest(t, "GET", fmt.Sprintf("/api/projects/%s", projectID), nil, testutil.AuthHeader(adminToken))
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	resp.Body.Close()

	// Member cannot access admin's project
	resp = env.DoRequest(t, "GET", fmt.Sprintf("/api/projects/%s", projectID), nil, testutil.AuthHeader(memberToken))
	assert.Equal(t, http.StatusForbidden, resp.StatusCode)
	resp.Body.Close()

	// Member creates own project
	memberProject := env.CreateProject(t, memberToken, "Member Project", "member-project")
	memberProjectID := extractID(memberProject["id"])

	// Member can access own project
	resp = env.DoRequest(t, "GET", fmt.Sprintf("/api/projects/%s", memberProjectID), nil, testutil.AuthHeader(memberToken))
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	resp.Body.Close()

	// Admin can also access member's project
	resp = env.DoRequest(t, "GET", fmt.Sprintf("/api/projects/%s", memberProjectID), nil, testutil.AuthHeader(adminToken))
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	resp.Body.Close()
}

func TestListProjects(t *testing.T) {
	resetDB(t)
	adminToken := env.SetupAdmin(t, "admin@test.com", "password123")

	// Create member
	env.DoRequest(t, "POST", "/api/users", map[string]string{
		"email":    "member@test.com",
		"password": "password123",
		"role":     "member",
	}, testutil.AuthHeader(adminToken)).Body.Close()
	memberToken := env.LoginAs(t, "member@test.com", "password123")

	// Admin creates 2 projects
	env.CreateProject(t, adminToken, "Admin Project 1", "admin-project-1")
	env.CreateProject(t, adminToken, "Admin Project 2", "admin-project-2")

	// Member creates 1 project
	env.CreateProject(t, memberToken, "Member Project", "member-project")

	// Admin sees all 3
	resp := env.DoRequest(t, "GET", "/api/projects", nil, testutil.AuthHeader(adminToken))
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	projects := testutil.ReadJSONArray(t, resp)
	assert.Len(t, projects, 3)

	// Member sees only 1
	resp = env.DoRequest(t, "GET", "/api/projects", nil, testutil.AuthHeader(memberToken))
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	projects = testutil.ReadJSONArray(t, resp)
	assert.Len(t, projects, 1)
}

func TestUpdateProject(t *testing.T) {
	resetDB(t)
	adminToken := env.SetupAdmin(t, "admin@test.com", "password123")

	// Create member
	env.DoRequest(t, "POST", "/api/users", map[string]string{
		"email":    "member@test.com",
		"password": "password123",
		"role":     "member",
	}, testutil.AuthHeader(adminToken)).Body.Close()
	memberToken := env.LoginAs(t, "member@test.com", "password123")

	// Admin creates project
	project := env.CreateProject(t, adminToken, "Original", "original")
	projectID := extractID(project["id"])

	// Admin can update
	resp := env.DoRequest(t, "PUT", fmt.Sprintf("/api/projects/%s", projectID), map[string]string{
		"name": "Updated",
	}, testutil.AuthHeader(adminToken))
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	result := testutil.ReadJSON(t, resp)
	assert.Equal(t, "Updated", result["name"])

	// Member cannot update admin's project
	resp = env.DoRequest(t, "PUT", fmt.Sprintf("/api/projects/%s", projectID), map[string]string{
		"name": "Hacked",
	}, testutil.AuthHeader(memberToken))
	assert.Equal(t, http.StatusForbidden, resp.StatusCode)
	resp.Body.Close()
}

func TestDeleteProject(t *testing.T) {
	resetDB(t)
	token := env.SetupAdmin(t, "admin@test.com", "password123")

	project := env.CreateProject(t, token, "To Delete", "to-delete")
	projectID := extractID(project["id"])

	// Create an application in the project to test cascade
	env.CreateApplication(t, token, projectID, map[string]any{
		"name":        "Test App",
		"type":        "git",
		"build_type":  "dockerfile",
		"source_repo": "https://github.com/test/repo",
	})

	resp := env.DoRequest(t, "DELETE", fmt.Sprintf("/api/projects/%s", projectID), nil, testutil.AuthHeader(token))
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	resp.Body.Close()

	// Verify runtime stop/remove was called
	require.Greater(t, len(env.Runtime.StopCalls), 0)
	require.Greater(t, len(env.Runtime.RemoveCalls), 0)

	// Verify project is gone
	resp = env.DoRequest(t, "GET", fmt.Sprintf("/api/projects/%s", projectID), nil, testutil.AuthHeader(token))
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
	resp.Body.Close()
}

func TestTransferProject(t *testing.T) {
	resetDB(t)
	adminToken := env.SetupAdmin(t, "admin@test.com", "password123")

	// Create member
	resp := env.DoRequest(t, "POST", "/api/users", map[string]string{
		"email":    "member@test.com",
		"password": "password123",
		"role":     "member",
	}, testutil.AuthHeader(adminToken))
	member := testutil.ReadJSON(t, resp)
	memberID := extractID(member["id"])
	memberToken := env.LoginAs(t, "member@test.com", "password123")

	// Admin creates project
	project := env.CreateProject(t, adminToken, "Transfer Me", "transfer-me")
	projectID := extractID(project["id"])

	// Member cannot transfer
	resp = env.DoRequest(t, "PUT", fmt.Sprintf("/api/projects/%s/transfer", projectID), map[string]string{
		"user_id": memberID,
	}, testutil.AuthHeader(memberToken))
	assert.Equal(t, http.StatusForbidden, resp.StatusCode)
	resp.Body.Close()

	// Admin can transfer
	resp = env.DoRequest(t, "PUT", fmt.Sprintf("/api/projects/%s/transfer", projectID), map[string]string{
		"user_id": memberID,
	}, testutil.AuthHeader(adminToken))
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	resp.Body.Close()

	// Member can now access the project
	resp = env.DoRequest(t, "GET", fmt.Sprintf("/api/projects/%s", projectID), nil, testutil.AuthHeader(memberToken))
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	resp.Body.Close()
}
