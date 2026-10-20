package handler_test

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/belune-hq/belune/internal/testutil"
)

func TestGetMetrics_AdminOnly(t *testing.T) {
	resetDB(t)
	adminToken := env.SetupAdmin(t, "admin@test.com", "password123")

	// Create member
	env.DoRequest(t, "POST", "/api/users", map[string]string{
		"email":    "member@test.com",
		"password": "password123",
		"role":     "member",
	}, testutil.AuthHeader(adminToken)).Body.Close()
	memberToken := env.LoginAs(t, "member@test.com", "password123")

	// Create some data
	env.CreateProject(t, adminToken, "Project 1", "project-1")
	env.CreateProject(t, adminToken, "Project 2", "project-2")

	// Admin can get metrics
	resp := env.DoRequest(t, "GET", "/api/summary", nil, testutil.AuthHeader(adminToken))
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	result := testutil.ReadJSON(t, resp)
	assert.Equal(t, float64(2), result["projects"])

	// Member gets 403
	resp = env.DoRequest(t, "GET", "/api/summary", nil, testutil.AuthHeader(memberToken))
	assert.Equal(t, http.StatusForbidden, resp.StatusCode)
	resp.Body.Close()
}

func TestTriggerCleanup(t *testing.T) {
	resetDB(t)
	adminToken := env.SetupAdmin(t, "admin@test.com", "password123")

	resp := env.DoRequest(t, "POST", "/api/maintenance/cleanup", nil, testutil.AuthHeader(adminToken))
	assert.Equal(t, http.StatusAccepted, resp.StatusCode)
	resp.Body.Close()

	// Verify cleanup task was enqueued
	assert.Len(t, env.Asynq.Tasks, 1)
	assert.Equal(t, "cleanup", env.Asynq.Tasks[0].TypeName)
}
