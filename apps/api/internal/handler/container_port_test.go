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

	"github.com/weiliang79/belune/internal/testutil"
)

func readContainerPort(t *testing.T, appID string) *int32 {
	t.Helper()
	var port *int32
	require.NoError(t, env.Pool.QueryRow(context.Background(),
		`SELECT container_port FROM applications WHERE id = $1`, appID).Scan(&port))
	return port
}

func TestSetContainerPort(t *testing.T) {
	resetDB(t)
	token := env.SetupAdmin(t, "port@test.com", "password123")
	projectID, appID := settingsApp(t, token)
	url := fmt.Sprintf("/api/projects/%s/applications/%s/port", projectID, appID)
	put := func(body any) (int, map[string]any) {
		resp := env.DoRequest(t, "PUT", url, body, testutil.AuthHeader(token))
		return resp.StatusCode, testutil.ReadJSON(t, resp)
	}
	before := readAppRuntime(t, appID)

	status, body := put(map[string]any{"container_port": 3000})
	require.Equal(t, http.StatusOK, status)
	assert.EqualValues(t, 3000, body["container_port"])
	require.NotNil(t, readContainerPort(t, appID))
	assert.EqualValues(t, 3000, *readContainerPort(t, appID))
	// Saved, not applied: the marker the Reload badge reads must be stamped.
	// (pending_change itself is suppressed before a first deploy, so read the
	// column.)
	var marked bool
	require.NoError(t, env.Pool.QueryRow(context.Background(),
		`SELECT config_changed_at IS NOT NULL FROM applications WHERE id = $1`, appID).Scan(&marked))
	assert.True(t, marked, "a port change needs a redeploy and must say so")

	// Nothing else on the row moves.
	assert.Equal(t, before, readAppRuntime(t, appID))

	// 0 clears it back to NULL.
	status, body = put(map[string]any{"container_port": 0})
	require.Equal(t, http.StatusOK, status)
	assert.Nil(t, body["container_port"])
	assert.Nil(t, readContainerPort(t, appID), "0 must clear the column to NULL, not store 0")

	// Refusals write nothing. A body without the field, or with null, must
	// never be read as a clear.
	_, _ = put(map[string]any{"container_port": 4000})
	for name, bad := range map[string]any{
		"missing":      map[string]any{},
		"null":         map[string]any{"container_port": nil},
		"negative":     map[string]any{"container_port": -1},
		"too large":    map[string]any{"container_port": 65536},
		"not a number": map[string]any{"container_port": "3000"},
	} {
		t.Run(name, func(t *testing.T) {
			status, _ := put(bad)
			assert.Equal(t, http.StatusBadRequest, status)
			require.NotNil(t, readContainerPort(t, appID))
			assert.EqualValues(t, 4000, *readContainerPort(t, appID), "a refused request changed the port")
		})
	}

	status, _ = put(map[string]any{"container_port": 65535})
	assert.Equal(t, http.StatusOK, status, "65535 is the top of the range")
}

func TestSetContainerPort_AccessAndAudit(t *testing.T) {
	resetDB(t)
	adminToken := env.SetupAdmin(t, "port-acl@test.com", "password123")
	projectID, appID := settingsApp(t, adminToken)
	url := fmt.Sprintf("/api/projects/%s/applications/%s/port", projectID, appID)

	// A read-only token may not write.
	readTok := mintScoped(t, adminToken, []string{"read"})
	resp := env.DoRequest(t, "PUT", url, map[string]any{"container_port": 3000}, testutil.AuthHeader(readTok))
	resp.Body.Close()
	assert.Equal(t, http.StatusForbidden, resp.StatusCode)
	assert.Nil(t, readContainerPort(t, appID))

	// The shared harness has no audit service on purpose; the audited server
	// serves the same database and the same REST routes.
	body, err := json.Marshal(map[string]any{"container_port": 3000})
	require.NoError(t, err)
	req, err := http.NewRequest("PUT", newAuditedMCP(t)+url, bytes.NewReader(body))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+adminToken)
	resp, err = http.DefaultClient.Do(req)
	require.NoError(t, err)
	resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	// Audit rows are written asynchronously.
	require.Eventually(t, func() bool {
		var n int
		env.Pool.QueryRow(context.Background(),
			`SELECT count(*) FROM audit_logs WHERE action = 'set_container_port' AND resource_id = $1`, appID).Scan(&n)
		return n == 1
	}, 2*time.Second, 10*time.Millisecond)
}

func TestMCP_SetPort_SetsClearsAndValidates(t *testing.T) {
	resetDB(t)
	adminToken := env.SetupAdmin(t, "mcp-port@test.com", "password123")
	_, appID := settingsApp(t, adminToken)
	token := mintScoped(t, adminToken, []string{"write"})
	before := readAppRuntime(t, appID)

	var got struct {
		Application struct {
			ContainerPort int32 `json:"container_port"`
		} `json:"application"`
		Note string `json:"note"`
	}
	decodeToolResult(t, callToolArgs(t, token, "set_application_port", map[string]any{
		"application_id": appID, "container_port": 3000,
	}), &got)
	assert.EqualValues(t, 3000, got.Application.ContainerPort)
	assert.Contains(t, got.Note, "saved, not applied")
	assert.Contains(t, got.Note, "trigger_deployment")
	require.NotNil(t, readContainerPort(t, appID))
	assert.EqualValues(t, 3000, *readContainerPort(t, appID))
	assert.Equal(t, before, readAppRuntime(t, appID))

	for _, bad := range []int{-1, 65536} {
		assert.Contains(t, toolErrorText(t, callToolArgs(t, token, "set_application_port", map[string]any{
			"application_id": appID, "container_port": bad,
		})), "between 1 and 65535")
	}
	assert.EqualValues(t, 3000, *readContainerPort(t, appID), "a refused call must change nothing")

	// A fresh value: the field is omitempty, so decoding over the old one would
	// leave 3000 in place and hide a failed clear.
	var cleared struct {
		Application struct {
			ContainerPort int32 `json:"container_port"`
		} `json:"application"`
	}
	decodeToolResult(t, callToolArgs(t, token, "set_application_port", map[string]any{
		"application_id": appID, "container_port": 0,
	}), &cleared)
	assert.Nil(t, readContainerPort(t, appID), "0 must clear the port")
	assert.Zero(t, cleared.Application.ContainerPort)
}
