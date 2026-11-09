package instances_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/belune-hq/belune/internal/instances"
	"github.com/belune-hq/belune/internal/runtime"
	"github.com/belune-hq/belune/internal/store/generated"
)

type slowStats struct {
	delay time.Duration
	fail  map[string]bool
	mu    sync.Mutex
	calls int
}

func (s *slowStats) ContainerStats(ctx context.Context, name string) (*runtime.ContainerResourceStats, error) {
	s.mu.Lock()
	s.calls++
	s.mu.Unlock()
	time.Sleep(s.delay)
	if s.fail[name] {
		return nil, errors.New("unreachable")
	}
	return &runtime.ContainerResourceStats{CPUPercent: 10, MemoryUsage: 100, MemoryLimit: 1000, NetworkRxBytes: 1, NetworkTxBytes: 2}, nil
}

type row = generated.ListInstancesWithProcessTypeByApplicationRow

func mk(typeName string, routable bool, idx int32, status string) row {
	return row{
		ID: pgtype.UUID{Bytes: [16]byte{byte(idx), byte(len(typeName))}, Valid: true}, Idx: idx, ContainerName: typeName + "-" + string(rune('0'+idx)),
		Status: status, ProcessTypeName: typeName, ProcessTypeRoutable: routable,
	}
}

func names(rows []row) []string {
	var out []string
	for _, r := range rows {
		out = append(out, r.ContainerName)
	}
	return out
}

// The fan-out is the assertion the errgroup exists for: a pass over N instances costs
// about ONE call, not N. A presence test would pass with a serial loop.
func TestFetchStats_WallTimeIsOneCallNotN(t *testing.T) {
	const n = 8
	const delay = 200 * time.Millisecond
	var all []string
	for i := 0; i < n; i++ {
		all = append(all, "c"+string(rune('a'+i)))
	}
	rt := &slowStats{delay: delay}

	start := time.Now()
	got := instances.FetchStats(context.Background(), rt, all)
	parallel := time.Since(start)

	assert.Len(t, got, n)
	serial := time.Duration(n) * delay
	t.Logf("serial would be %v; parallel took %v for %d instances", serial, parallel, n)
	assert.Less(t, parallel, 3*delay, "about one call's time, not %d of them", n)
}

// One failing stats call degrades only its own instance (no shared context).
func TestFetchStats_OneFailureDegradesOnlyItself(t *testing.T) {
	rt := &slowStats{fail: map[string]bool{"b": true}}
	got := instances.FetchStats(context.Background(), rt, []string{"a", "b", "c"})
	assert.Contains(t, got, "a")
	assert.NotContains(t, got, "b")
	assert.Contains(t, got, "c")
}

// A replica that is not running publishes nothing, while its running siblings do.
func TestLiveInstances_AStoppedReplicaIsDroppedAndItsSiblingsAreNot(t *testing.T) {
	rows := []row{mk("web", true, 1, "running"), mk("web", true, 2, "stopped"), mk("web", true, 3, "running"), mk("w", false, 1, "pending"), mk("w", false, 2, "unhealthy")}
	assert.Equal(t, []string{"web-1", "web-3", "w-2"}, names(instances.LiveInstances(rows)))
}

// The totals are the server-side sum over the routable type's instances; workers are in
// the per-instance list, flagged, but not folded into the totals.
func TestBuildPoint_TotalsAreTheSumOfTheRoutableInstances(t *testing.T) {
	live := []row{mk("web", true, 1, "running"), mk("web", true, 2, "running"), mk("web", true, 3, "running"), mk("worker", false, 1, "running")}
	stats := instances.FetchStats(context.Background(), &slowStats{}, names(live))

	p, ok := instances.BuildPoint(live, stats, time.Now())
	require.True(t, ok)
	require.Len(t, p.Instances, 4)

	var cpu float64
	var mem, rx int64
	for _, in := range p.Instances {
		if in.Routable {
			cpu += in.CPUPercent
			mem += in.MemoryUsage
			rx += in.NetworkRxBytes
		}
	}
	assert.InDelta(t, 30, *p.CPUPercent, 0.0001, "three web replicas at 10 each; the worker is not in the total")
	assert.Equal(t, cpu, *p.CPUPercent)
	assert.Equal(t, mem, *p.MemoryUsage)
	assert.EqualValues(t, 3000, *p.MemoryLimit)
	assert.Equal(t, rx, *p.NetworkRxBytes)
	assert.Equal(t, "web", p.Instances[0].ProcessType)
	assert.EqualValues(t, 1, p.Instances[0].Idx)
	assert.True(t, p.Instances[3].Routable == false && p.Instances[3].ProcessType == "worker")
}

func TestBuildPoint_NullTotalsWhenNoRoutableInstanceAnsweredAndNothingWhenNoneDid(t *testing.T) {
	live := []row{mk("web", true, 1, "running"), mk("worker", false, 1, "running")}

	stats := instances.FetchStats(context.Background(), &slowStats{fail: map[string]bool{"web-1": true}}, names(live))
	p, ok := instances.BuildPoint(live, stats, time.Now())
	require.True(t, ok, "the worker answered")
	assert.Nil(t, p.CPUPercent, "no routable sample: unknown, never zero")
	assert.Len(t, p.Instances, 1)

	_, ok = instances.BuildPoint(live, map[string]*runtime.ContainerResourceStats{}, time.Now())
	assert.False(t, ok, "nothing sampled, nothing published")
}
