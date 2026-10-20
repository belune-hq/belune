package handler_test

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/belune-hq/belune/internal/testutil"
)

func TestHealthCheck_Healthy(t *testing.T) {
	resetDB(t)

	resp := env.DoRequest(t, "GET", "/healthz", nil, nil)
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	result := testutil.ReadJSON(t, resp)
	assert.Equal(t, true, result["healthy"])

	checks, ok := result["checks"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "ok", checks["database"])
	// Docker mock returns empty list, so it should be healthy
	assert.Contains(t, checks["docker"], "ok")
}
