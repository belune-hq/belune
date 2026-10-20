package handler_test

import (
	"context"
	"net/http"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/belune-hq/belune/internal/store/generated"
	"github.com/belune-hq/belune/internal/testutil"
)

func TestListAuditLogs(t *testing.T) {
	resetDB(t)
	token := env.SetupAdmin(t, "admin@test.com", "password123")

	// Insert some audit log entries directly
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		env.Queries.CreateAuditLog(ctx, generated.CreateAuditLogParams{
			Action:       "test_action",
			ResourceType: "test",
			ResourceID:   pgtype.Text{String: "res-1", Valid: true},
		})
	}

	resp := env.DoRequest(t, "GET", "/api/audit-logs?limit=10&offset=0", nil, testutil.AuthHeader(token))
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	result := testutil.ReadJSON(t, resp)
	items, ok := result["items"].([]any)
	require.True(t, ok)
	assert.Len(t, items, 3)
	total, ok := result["total"].(float64)
	require.True(t, ok)
	assert.Equal(t, float64(3), total)
}

func TestListAuditLogs_Pagination(t *testing.T) {
	resetDB(t)
	token := env.SetupAdmin(t, "admin@test.com", "password123")

	ctx := context.Background()
	for i := 0; i < 5; i++ {
		env.Queries.CreateAuditLog(ctx, generated.CreateAuditLogParams{
			Action:       "paginated_action",
			ResourceType: "test",
		})
	}

	resp := env.DoRequest(t, "GET", "/api/audit-logs?limit=2&offset=0", nil, testutil.AuthHeader(token))
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	result := testutil.ReadJSON(t, resp)
	items := result["items"].([]any)
	assert.Len(t, items, 2)
	assert.Equal(t, float64(5), result["total"].(float64))
}

func TestListAuditLogs_NonAdmin(t *testing.T) {
	resetDB(t)
	adminToken := env.SetupAdmin(t, "admin@test.com", "password123")

	// Create member user
	resp := env.DoRequest(t, "POST", "/api/users", map[string]any{
		"email":    "member@test.com",
		"password": "password123",
		"role":     "member",
	}, testutil.AuthHeader(adminToken))
	resp.Body.Close()

	memberToken := env.LoginAs(t, "member@test.com", "password123")

	resp = env.DoRequest(t, "GET", "/api/audit-logs?limit=10&offset=0", nil, testutil.AuthHeader(memberToken))
	assert.Equal(t, http.StatusForbidden, resp.StatusCode)
	resp.Body.Close()
}
