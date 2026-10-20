package handler_test

import (
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/belune-hq/belune/internal/runtime"
	"github.com/belune-hq/belune/internal/testutil"
)

// A real one-shot stats call takes ~2s, and the overview used to make them
// serially — 8+ running apps could never finish inside the 15s handler timeout.
// The mock delays each call so a serial loop is unmistakable: 8 x 300ms = 2.4s
// serial versus ~300ms overlapped.
func TestProjectMetrics_StatsRunConcurrently(t *testing.T) {
	resetDB(t)
	adminToken := env.SetupAdmin(t, "admin@test.com", "password123")
	project := env.CreateProject(t, adminToken, "Metrics Project", "metrics-project")
	projectID := extractID(project["id"])

	const apps = 8
	const perCall = 300 * time.Millisecond
	ids := make([]string, apps)
	slugs := make([]string, apps)
	var containers []runtime.ContainerInfo
	for i := range apps {
		app := env.CreateApplication(t, adminToken, projectID, map[string]any{
			"name":         fmt.Sprintf("Metrics App %d", i),
			"type":         "image",
			"build_type":   "image",
			"source_image": "nginx:latest",
		})
		ids[i] = extractID(app["id"])
		slugs[i], _ = app["slug"].(string)
		require.NotEmpty(t, slugs[i])
		containers = append(containers, runtime.ContainerInfo{Name: slugs[i], Status: "running"})
	}

	// slugs[0]'s stats call fails: it must degrade to zeros without affecting
	// or cancelling its siblings.
	failing := slugs[0]
	env.Runtime.ListContainers_ = containers
	env.Runtime.StatsFunc = func(_ context.Context, name string) (*runtime.ContainerResourceStats, error) {
		time.Sleep(perCall)
		if name == failing {
			return nil, fmt.Errorf("stats unavailable")
		}
		return &runtime.ContainerResourceStats{CPUPercent: 12.5, MemoryUsage: 1024, MemoryLimit: 4096}, nil
	}
	t.Cleanup(func() {
		env.Runtime.ListContainers_ = nil
		env.Runtime.StatsFunc = nil
	})

	start := time.Now()
	resp := env.DoRequest(t, "GET", "/api/projects/"+projectID+"/metrics", nil, testutil.AuthHeader(adminToken))
	elapsed := time.Since(start)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	body := testutil.ReadJSON(t, resp)

	assert.Less(t, elapsed, apps*perCall/2, "stats calls must overlap, not run one after another")
	require.Len(t, body, apps)
	for i, id := range ids {
		m, ok := body[id].(map[string]any)
		require.True(t, ok, "app %d missing from the overview", i)
		assert.Equal(t, "running", m["status"])
		if slugs[i] == failing {
			assert.EqualValues(t, 0, m["cpu_percent"])
			assert.EqualValues(t, 0, m["memory_used"])
			continue
		}
		assert.EqualValues(t, 12.5, m["cpu_percent"])
		assert.EqualValues(t, 1024, m["memory_used"])
		assert.EqualValues(t, 4096, m["memory_limit"])
	}
}
