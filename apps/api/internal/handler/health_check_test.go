package handler_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/belune-hq/belune/internal/testutil"
)

func TestSetHealthCheck(t *testing.T) {
	resetDB(t)
	token := env.SetupAdmin(t, "admin@test.com", "password123")
	project := env.CreateProject(t, token, "Test Project", "test-project")
	projectID := extractID(project["id"])
	app := env.CreateApplication(t, token, projectID, map[string]any{
		"name": "HC App", "type": "image", "build_type": "image",
		"source_image": "nginx:1.25",
	})
	appID := extractID(app["id"])
	url := fmt.Sprintf("/api/projects/%s/applications/%s/health-check", projectID, appID)

	// Configure a command check.
	resp := env.DoRequest(t, "PUT", url, map[string]any{
		"type": "command", "command": "curl -f http://localhost/health",
		"interval_seconds": 15, "retries": 2,
	}, testutil.AuthHeader(token))
	require.Equal(t, http.StatusOK, resp.StatusCode)
	result := testutil.ReadJSON(t, resp)
	assert.Equal(t, "command", result["health_check_type"])
	assert.Equal(t, "curl -f http://localhost/health", result["health_check_command"])

	// Stored as expected.
	var typ, command string
	var interval int
	require.NoError(t, env.Pool.QueryRow(context.Background(),
		`SELECT health_check_type, COALESCE(health_check_command,''), COALESCE(health_check_interval_seconds,0)
		   FROM applications WHERE id = $1`, appID).Scan(&typ, &command, &interval))
	assert.Equal(t, "command", typ)
	assert.Equal(t, "curl -f http://localhost/health", command)
	assert.Equal(t, 15, interval)

	// Switching to http clears the command, so no stale check survives.
	resp = env.DoRequest(t, "PUT", url, map[string]any{
		"type": "http", "path": "/healthz",
	}, testutil.AuthHeader(token))
	require.Equal(t, http.StatusOK, resp.StatusCode)
	resp.Body.Close()

	var hasCommand bool
	var path string
	require.NoError(t, env.Pool.QueryRow(context.Background(),
		`SELECT health_check_command IS NOT NULL, COALESCE(health_check_path,'')
		   FROM applications WHERE id = $1`, appID).Scan(&hasCommand, &path))
	assert.False(t, hasCommand, "the command must be cleared when switching to http")
	assert.Equal(t, "/healthz", path)
}

func TestSetHealthCheck_Rejections(t *testing.T) {
	resetDB(t)
	token := env.SetupAdmin(t, "admin@test.com", "password123")
	project := env.CreateProject(t, token, "Test Project", "test-project")
	projectID := extractID(project["id"])
	app := env.CreateApplication(t, token, projectID, map[string]any{
		"name": "HC App", "type": "image", "build_type": "image",
		"source_image": "nginx:1.25",
	})
	appID := extractID(app["id"])
	url := fmt.Sprintf("/api/projects/%s/applications/%s/health-check", projectID, appID)

	for name, body := range map[string]map[string]any{
		"command with no command": {"type": "command"},
		"http with no path":       {"type": "http"},
		"unknown type":            {"type": "tcp"},
	} {
		t.Run(name, func(t *testing.T) {
			resp := env.DoRequest(t, "PUT", url, body, testutil.AuthHeader(token))
			defer resp.Body.Close()
			assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
		})
	}
}

func readContainerPort(t *testing.T, appID string) *int32 {
	t.Helper()
	var port *int32
	require.NoError(t, env.Pool.QueryRow(context.Background(),
		`SELECT container_port FROM applications WHERE id = $1`, appID).Scan(&port))
	return port
}

func TestSetHealthCheck_ContainerPort(t *testing.T) {
	resetDB(t)
	token := env.SetupAdmin(t, "port@test.com", "password123")
	projectID, appID := settingsApp(t, token)
	url := fmt.Sprintf("/api/projects/%s/applications/%s/health-check", projectID, appID)
	put := func(body map[string]any) (int, map[string]any) {
		resp := env.DoRequest(t, "PUT", url, body, testutil.AuthHeader(token))
		return resp.StatusCode, testutil.ReadJSON(t, resp)
	}

	status, body := put(map[string]any{"type": "none", "container_port": 3000})
	require.Equal(t, http.StatusOK, status)
	assert.EqualValues(t, 3000, body["container_port"], "the response must carry the port just written")
	require.NotNil(t, readContainerPort(t, appID))
	assert.EqualValues(t, 3000, *readContainerPort(t, appID))
	var marked bool
	require.NoError(t, env.Pool.QueryRow(context.Background(),
		`SELECT config_changed_at IS NOT NULL FROM applications WHERE id = $1`, appID).Scan(&marked))
	assert.True(t, marked, "a port change needs a redeploy and must say so")

	// The regression this field's pointer exists for: a health-check PUT that
	// does not mention the port must leave a routing default alone.
	for name, b := range map[string]map[string]any{
		"omitted": {"type": "http", "path": "/healthz"},
		"null":    {"type": "none", "container_port": nil},
	} {
		t.Run(name+" leaves the port", func(t *testing.T) {
			status, _ := put(b)
			require.Equal(t, http.StatusOK, status)
			require.NotNil(t, readContainerPort(t, appID))
			assert.EqualValues(t, 3000, *readContainerPort(t, appID))
		})
	}

	// Out of range is refused and nothing is written, health check included.
	// (Map order above is random, so pin the stored check first.)
	status, _ = put(map[string]any{"type": "http", "path": "/healthz"})
	require.Equal(t, http.StatusOK, status)
	for name, bad := range map[string]any{"negative": -1, "too large": 65536, "string": "3000"} {
		t.Run(name, func(t *testing.T) {
			status, _ := put(map[string]any{"type": "command", "command": "true", "container_port": bad})
			assert.Equal(t, http.StatusBadRequest, status)
			require.NotNil(t, readContainerPort(t, appID))
			assert.EqualValues(t, 3000, *readContainerPort(t, appID), "a refused request changed the port")
			var typ string
			require.NoError(t, env.Pool.QueryRow(context.Background(),
				`SELECT health_check_type FROM applications WHERE id = $1`, appID).Scan(&typ))
			assert.Equal(t, "http", typ, "a refused request changed the health check")
		})
	}

	status, _ = put(map[string]any{"type": "none", "container_port": 65535})
	assert.Equal(t, http.StatusOK, status, "65535 is the top of the range")

	// 0 clears to NULL, not to a stored 0.
	status, body = put(map[string]any{"type": "none", "container_port": 0})
	require.Equal(t, http.StatusOK, status)
	assert.Nil(t, body["container_port"])
	assert.Nil(t, readContainerPort(t, appID))
}

func TestSetHealthCheck_ContainerPort_AccessAndAudit(t *testing.T) {
	resetDB(t)
	adminToken := env.SetupAdmin(t, "port-acl@test.com", "password123")
	projectID, appID := settingsApp(t, adminToken)
	url := fmt.Sprintf("/api/projects/%s/applications/%s/health-check", projectID, appID)

	readTok := mintScoped(t, adminToken, []string{"read"})
	resp := env.DoRequest(t, "PUT", url, map[string]any{"type": "none", "container_port": 3000}, testutil.AuthHeader(readTok))
	resp.Body.Close()
	assert.Equal(t, http.StatusForbidden, resp.StatusCode)
	assert.Nil(t, readContainerPort(t, appID))

	// The shared harness has no audit service on purpose; the audited server
	// serves the same database and the same REST routes.
	send := func(body map[string]any) {
		raw, err := json.Marshal(body)
		require.NoError(t, err)
		req, err := http.NewRequest("PUT", newAuditedMCP(t)+url, bytes.NewReader(raw))
		require.NoError(t, err)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+adminToken)
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		resp.Body.Close()
		require.Equal(t, http.StatusOK, resp.StatusCode)
	}
	send(map[string]any{"type": "none", "container_port": 3000})
	send(map[string]any{"type": "none"})

	// Audit rows are written asynchronously. One entry per call; the port is
	// in the details only when the key was sent.
	require.Eventually(t, func() bool {
		var n int
		env.Pool.QueryRow(context.Background(),
			`SELECT count(*) FROM audit_logs WHERE action = 'set_health_check' AND resource_id = $1`, appID).Scan(&n)
		return n == 2
	}, 2*time.Second, 10*time.Millisecond)
	var withPort int
	require.NoError(t, env.Pool.QueryRow(context.Background(),
		`SELECT count(*) FROM audit_logs WHERE action = 'set_health_check' AND resource_id = $1 AND details ? 'container_port'`,
		appID).Scan(&withPort))
	assert.Equal(t, 1, withPort)
}
