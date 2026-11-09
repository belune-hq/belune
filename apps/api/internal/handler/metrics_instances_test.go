package handler_test

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/belune-hq/belune/internal/instances"
	"github.com/belune-hq/belune/internal/runtime"
	"github.com/belune-hq/belune/internal/status"
	"github.com/belune-hq/belune/internal/store/generated"
	"github.com/belune-hq/belune/internal/testutil"
)

// The SSE stream used to resolve one container at connect and keep it for the life of
// the connection. Replicas change under a live connection (someone scales while the
// page is open), so the instances are re-read every tick and a new replica appears in
// the stream without a reconnect.
func TestStreamMetrics_PicksUpAReplicaAddedWhileTheStreamIsOpen(t *testing.T) {
	token, projectID, appID, appUUID, _, inst := seedTypedLogApp(t)
	ctx := context.Background()

	// Everything running, so every instance is sampled.
	for _, in := range inst {
		_, err := env.Queries.UpdateInstanceStatus(ctx, generated.UpdateInstanceStatusParams{ID: in.ID, Status: status.ApplicationRunning})
		require.NoError(t, err)
	}
	_, err := env.Queries.UpdateApplicationStatus(ctx, generated.UpdateApplicationStatusParams{ID: appUUID, Status: status.ApplicationRunning})
	require.NoError(t, err)
	env.Runtime.StatsFunc = func(context.Context, string) (*runtime.ContainerResourceStats, error) {
		return &runtime.ContainerResourceStats{CPUPercent: 5, MemoryUsage: 50}, nil
	}
	t.Cleanup(func() { env.Runtime.StatsFunc = nil })

	old := instances.MetricsInterval
	instances.MetricsInterval = 50 * time.Millisecond
	t.Cleanup(func() { instances.MetricsInterval = old })

	resp := env.DoRequest(t, "GET", fmt.Sprintf("/api/projects/%s/applications/%s/metrics/stream", projectID, appID), nil, testutil.AuthHeader(token))
	require.Equal(t, http.StatusOK, resp.StatusCode)
	defer func() { _ = resp.Body.Close() }()

	points := make(chan instances.Point, 16)
	go func() {
		sc := bufio.NewScanner(resp.Body)
		for sc.Scan() {
			if raw, ok := strings.CutPrefix(sc.Text(), "data: "); ok {
				var p instances.Point
				if json.Unmarshal([]byte(raw), &p) == nil {
					points <- p
				}
			}
		}
		close(points)
	}()
	next := func() instances.Point {
		select {
		case p, ok := <-points:
			require.True(t, ok, "stream ended")
			return p
		case <-time.After(5 * time.Second):
			t.Fatal("no metrics point within 5s")
			return instances.Point{}
		}
	}

	first := next()
	assert.Len(t, first.Instances, 3, "web + queue + scheduler")
	require.NotNil(t, first.CPUPercent)
	assert.InDelta(t, 5, *first.CPUPercent, 0.0001, "the total is the routable type's: one web replica")

	// A second web replica appears while the stream is open.
	web := inst["web"]
	_, err = env.Queries.CreateInstance(ctx, generated.CreateInstanceParams{
		ApplicationID: appUUID, ProcessTypeID: web.ProcessTypeID, Idx: 2, ContainerName: web.ContainerName + "-2", Status: status.ApplicationRunning,
	})
	require.NoError(t, err)

	deadline := time.After(5 * time.Second)
	for {
		select {
		case p := <-points:
			if len(p.Instances) == 4 {
				require.NotNil(t, p.CPUPercent)
				assert.InDelta(t, 10, *p.CPUPercent, 0.0001, "the total now sums both web replicas")
				return
			}
		case <-deadline:
			t.Fatal("the stream never picked up the new replica")
		}
	}
}

// ListInstancesByApplication used to order by idx alone, which interleaves the types
// (web-1, worker-1, web-2, worker-2). ResolveAll returns it as-is.
func TestResolveAll_GroupsByProcessTypeThenIdx(t *testing.T) {
	_, _, _, appUUID, _, inst := seedTypedLogApp(t)
	ctx := context.Background()
	web := inst["web"]
	_, err := env.Queries.CreateInstance(ctx, generated.CreateInstanceParams{
		ApplicationID: appUUID, ProcessTypeID: web.ProcessTypeID, Idx: 2, ContainerName: web.ContainerName + "-2", Status: "pending",
	})
	require.NoError(t, err)
	q := inst["queue"]
	_, err = env.Queries.CreateInstance(ctx, generated.CreateInstanceParams{
		ApplicationID: appUUID, ProcessTypeID: q.ProcessTypeID, Idx: 2, ContainerName: q.ContainerName + "-2", Status: "pending",
	})
	require.NoError(t, err)

	rows, err := instances.ResolveAll(ctx, env.Queries, appUUID)
	require.NoError(t, err)
	var got []string
	for _, r := range rows {
		got = append(got, r.ContainerName)
	}
	assert.Equal(t, []string{
		web.ContainerName, web.ContainerName + "-2", // the routable type first
		q.ContainerName, q.ContainerName + "-2",
		inst["scheduler"].ContainerName,
	}, got)
}
