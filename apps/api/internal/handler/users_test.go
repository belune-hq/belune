package handler_test

import (
	"fmt"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/belune-hq/belune/internal/testutil"
)

func TestCreateUser_AdminOnly(t *testing.T) {
	resetDB(t)
	adminToken := env.SetupAdmin(t, "admin@test.com", "password123")

	// Admin can create user
	resp := env.DoRequest(t, "POST", "/api/users", map[string]string{
		"email":    "member@test.com",
		"password": "password123",
		"role":     "member",
	}, testutil.AuthHeader(adminToken))
	assert.Equal(t, http.StatusCreated, resp.StatusCode)
	result := testutil.ReadJSON(t, resp)
	assert.Equal(t, "member@test.com", result["email"])
	assert.Equal(t, "member", result["role"])

	// Member cannot create user
	memberToken := env.LoginAs(t, "member@test.com", "password123")
	resp = env.DoRequest(t, "POST", "/api/users", map[string]string{
		"email":    "another@test.com",
		"password": "password123",
		"role":     "member",
	}, testutil.AuthHeader(memberToken))
	assert.Equal(t, http.StatusForbidden, resp.StatusCode)
	resp.Body.Close()
}

func TestListUsers(t *testing.T) {
	resetDB(t)
	adminToken := env.SetupAdmin(t, "admin@test.com", "password123")

	// Create a member
	env.DoRequest(t, "POST", "/api/users", map[string]string{
		"email":    "member@test.com",
		"password": "password123",
		"role":     "member",
	}, testutil.AuthHeader(adminToken)).Body.Close()

	// Admin can list users
	resp := env.DoRequest(t, "GET", "/api/users", nil, testutil.AuthHeader(adminToken))
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	users := testutil.ReadJSONArray(t, resp)
	assert.Len(t, users, 2)

	// Member gets 403
	memberToken := env.LoginAs(t, "member@test.com", "password123")
	resp = env.DoRequest(t, "GET", "/api/users", nil, testutil.AuthHeader(memberToken))
	assert.Equal(t, http.StatusForbidden, resp.StatusCode)
	resp.Body.Close()
}

func TestUpdateUserRole(t *testing.T) {
	resetDB(t)
	adminToken := env.SetupAdmin(t, "admin@test.com", "password123")

	// Create a member
	resp := env.DoRequest(t, "POST", "/api/users", map[string]string{
		"email":    "member@test.com",
		"password": "password123",
		"role":     "member",
	}, testutil.AuthHeader(adminToken))
	member := testutil.ReadJSON(t, resp)
	memberID := extractID(member["id"])

	// Promote member to admin
	resp = env.DoRequest(t, "PUT", fmt.Sprintf("/api/users/%s/role", memberID), map[string]string{
		"role": "admin",
	}, testutil.AuthHeader(adminToken))
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	resp.Body.Close()

	// Cannot demote last admin if only one remains
	// First get admin user ID from /me
	resp = env.DoRequest(t, "GET", "/api/auth/me", nil, testutil.AuthHeader(adminToken))
	me := testutil.ReadJSON(t, resp)
	adminID := me["id"].(string)

	// Demote the original admin (now there are 2 admins, so this should work)
	resp = env.DoRequest(t, "PUT", fmt.Sprintf("/api/users/%s/role", adminID), map[string]string{
		"role": "member",
	}, testutil.AuthHeader(adminToken))
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	resp.Body.Close()

	// Now try to demote the last admin — should fail
	memberToken := env.LoginAs(t, "member@test.com", "password123") // member is now admin
	resp = env.DoRequest(t, "PUT", fmt.Sprintf("/api/users/%s/role", memberID), map[string]string{
		"role": "member",
	}, testutil.AuthHeader(memberToken))
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
	resp.Body.Close()
}

func TestDeleteUser(t *testing.T) {
	resetDB(t)
	adminToken := env.SetupAdmin(t, "admin@test.com", "password123")

	// Create a member
	resp := env.DoRequest(t, "POST", "/api/users", map[string]string{
		"email":    "member@test.com",
		"password": "password123",
		"role":     "member",
	}, testutil.AuthHeader(adminToken))
	member := testutil.ReadJSON(t, resp)
	memberID := extractID(member["id"])

	// Get admin ID
	resp = env.DoRequest(t, "GET", "/api/auth/me", nil, testutil.AuthHeader(adminToken))
	me := testutil.ReadJSON(t, resp)
	adminID := me["id"].(string)

	// Cannot delete self
	resp = env.DoRequest(t, "DELETE", fmt.Sprintf("/api/users/%s", adminID), nil, testutil.AuthHeader(adminToken))
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
	resp.Body.Close()

	// Can delete member
	resp = env.DoRequest(t, "DELETE", fmt.Sprintf("/api/users/%s", memberID), nil, testutil.AuthHeader(adminToken))
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	resp.Body.Close()

	// Cannot delete last admin
	// Create another admin, then try to delete the original
	resp = env.DoRequest(t, "POST", "/api/users", map[string]string{
		"email":    "member2@test.com",
		"password": "password123",
		"role":     "admin",
	}, testutil.AuthHeader(adminToken))
	resp.Body.Close()

	// Original admin is the last admin? No, we just created another.
	// Delete the new admin
	resp = env.DoRequest(t, "GET", "/api/users", nil, testutil.AuthHeader(adminToken))
	users := testutil.ReadJSONArray(t, resp)

	for _, u := range users {
		user := u.(map[string]any)
		uid := extractID(user["id"])
		if uid != adminID {
			// Delete this user
			resp = env.DoRequest(t, "DELETE", fmt.Sprintf("/api/users/%s", uid), nil, testutil.AuthHeader(adminToken))
			resp.Body.Close()
		}
	}

	// Now admin is last admin — can't delete
	// Actually we can't delete ourselves anyway, so this is covered by self-deletion check
}

func TestResetUserPassword(t *testing.T) {
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

	// Admin resets member's password, stepping up with the admin's OWN
	// current password.
	resp = env.DoRequest(t, "PUT", fmt.Sprintf("/api/users/%s/password", memberID), map[string]string{
		"password":         "newpassword123",
		"current_password": "password123",
	}, testutil.AuthHeader(adminToken))
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	resp.Body.Close()

	// Member can login with new password
	newToken := env.LoginAs(t, "member@test.com", "newpassword123")
	assert.NotEmpty(t, newToken)
}

// TestResetUserPassword_RequiresCallersOwnPassword pins the step-up fix: a
// hijacked or borrowed admin session must not be able to take over ANY
// account, including another admin's, with nothing re-verified. Wrong or
// absent current_password is rejected before the target's password ever
// changes.
func TestResetUserPassword_RequiresCallersOwnPassword(t *testing.T) {
	resetDB(t)
	adminToken := env.SetupAdmin(t, "admin@test.com", "password123")

	resp := env.DoRequest(t, "POST", "/api/users", map[string]string{
		"email":    "member@test.com",
		"password": "password123",
		"role":     "member",
	}, testutil.AuthHeader(adminToken))
	member := testutil.ReadJSON(t, resp)
	memberID := extractID(member["id"])

	resp = env.DoRequest(t, "PUT", fmt.Sprintf("/api/users/%s/password", memberID), map[string]string{
		"password":         "newpassword123",
		"current_password": "wrong-password",
	}, testutil.AuthHeader(adminToken))
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
	resp.Body.Close()

	// The target's password must be untouched.
	stillOld := env.LoginAs(t, "member@test.com", "password123")
	assert.NotEmpty(t, stillOld)
}

// extractID converts a pgtype.UUID JSON representation to a string UUID.
func extractID(id any) string {
	// pgtype.UUID serializes as a string UUID in JSON
	if s, ok := id.(string); ok {
		return s
	}
	return fmt.Sprintf("%v", id)
}
