package handler_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/belune-hq/belune/internal/testutil"
)

// auditedRequest sends a request to a server built by newAuditedMCP: a second
// router over the same database whose audit service is real, because the shared
// harness wires h.auditSvc as nil on purpose.
func (g gateEnv) auditedRequest(t *testing.T, base, method, path string, body any) *http.Response {
	t.Helper()
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		require.NoError(t, err)
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, base+path, rdr)
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Belune-Client-IP", "203.0.113.9")
	for k, v := range testutil.AuthHeader(g.token) {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	return resp
}

// Turning basic auth on or off on a public route leaves a trace, and the trace
// names the hostname, the type and whether it is on: never the config. The audit
// log records key NAMES and never values, and with `password` accepted the config
// is the second place a plaintext could escape.
func TestRouteFeatures_AreAuditedWithoutTheConfig(t *testing.T) {
	g := newGateEnv(t)
	base := newAuditedMCP(t) // a second router over the same DB, with a real audit writer

	resp := g.auditedRequest(t, base, "PUT", g.featuresURL, map[string]any{
		"feature_type": "basic_auth", "enabled": true,
		"config": map[string]any{"username": "alice", "password": gatePassword},
	})
	putBody := readBody(t, resp) // once: the message argument below is evaluated eagerly
	require.Equal(t, http.StatusOK, resp.StatusCode, putBody)
	var feature struct {
		ID string `json:"id"`
	}
	require.NoError(t, json.Unmarshal([]byte(putBody), &feature))

	var details string
	require.Eventually(t, func() bool {
		return env.Pool.QueryRow(context.Background(),
			`SELECT details::text FROM audit_logs WHERE action = 'upsert_route_feature' AND resource_type = 'route_feature' AND resource_id = $1`,
			feature.ID).Scan(&details) == nil
	}, 3*time.Second, 10*time.Millisecond, "setting a route feature must write an audit row")

	var d map[string]any
	require.NoError(t, json.Unmarshal([]byte(details), &d))
	assert.Equal(t, map[string]any{"hostname": g.hostname, "feature_type": "basic_auth", "enabled": true}, d)

	// Removal is audited too, and says what was removed.
	del := g.auditedRequest(t, base, "DELETE", g.featuresURL+"/"+feature.ID, nil)
	require.Equal(t, http.StatusOK, del.StatusCode, readBody(t, del))
	var deleted string
	require.Eventually(t, func() bool {
		return env.Pool.QueryRow(context.Background(),
			`SELECT details::text FROM audit_logs WHERE action = 'delete_route_feature' AND resource_type = 'route_feature' AND resource_id = $1`,
			feature.ID).Scan(&deleted) == nil
	}, 3*time.Second, 10*time.Millisecond, "removing a route feature must write an audit row")
	var dd map[string]any
	require.NoError(t, json.Unmarshal([]byte(deleted), &dd))
	assert.Equal(t, map[string]any{"hostname": g.hostname, "feature_type": "basic_auth", "enabled": true}, dd)

	// Neither entry, nor any other audit row, holds a secret or the config.
	rows, err := env.Pool.Query(context.Background(), `SELECT details::text FROM audit_logs WHERE details IS NOT NULL`)
	require.NoError(t, err)
	defer rows.Close()
	for rows.Next() {
		var text string
		require.NoError(t, rows.Scan(&text))
		assert.NotContains(t, text, gatePassword)
		assert.NotContains(t, text, "$2a$", "not even the hash")
		assert.NotContains(t, text, "config")
		assert.NotContains(t, text, "alice", "not even the username")
	}
}
