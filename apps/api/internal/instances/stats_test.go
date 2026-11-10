package instances_test

import (
	"context"
	"encoding/json"
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
	assert.Nil(t, p.MemoryLimit, "no configured limit on these rows: the ceiling is unknown, not the sampled 3 x 1000")
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

// mkL is mk with configured limits: the application's, and the process type's (NULL
// inherits). The stats stub reports a cgroup limit of 1000 for every container, a number
// no configured value below equals -- so an assertion on the ceiling can only pass if it
// came from configuration, never from the sample.
func mkL(typeName string, routable bool, idx int32, appMem int64, ptMem pgtype.Int8) row {
	r := mk(typeName, routable, idx, "running")
	r.ApplicationMemoryLimit = appMem
	r.ProcessTypeMemoryLimit = ptMem
	return r
}

func build(t *testing.T, live []row, failing ...string) (instances.Point, bool) {
	t.Helper()
	fail := map[string]bool{}
	for _, f := range failing {
		fail[f] = true
	}
	stats := instances.FetchStats(context.Background(), &slowStats{fail: fail}, names(live))
	return instances.BuildPoint(live, stats, time.Now())
}

// Three routable replicas, two answer: the total is the sum of TWO and the counts say so.
// Without them it reads as a confident figure for the whole application.
func TestBuildPoint_PartialCoverageIsReportedNotHidden(t *testing.T) {
	live := []row{mkL("web", true, 1, 4096, pgtype.Int8{}), mkL("web", true, 2, 4096, pgtype.Int8{}), mkL("web", true, 3, 4096, pgtype.Int8{}), mkL("worker", false, 1, 4096, pgtype.Int8{})}
	p, ok := build(t, live, "web-2")
	require.True(t, ok)

	assert.InDelta(t, 20, *p.CPUPercent, 0.0001, "two of three replicas answered")
	assert.Equal(t, 2, p.RoutableSampled)
	assert.Equal(t, 3, p.RoutableExpected)
	assert.Equal(t, 3, p.Sampled, "web-1, web-3 and the worker")
	assert.Equal(t, 4, p.Expected)
	assert.Len(t, p.Instances, p.Sampled)
	// The ceiling is configuration over the whole expected set: a replica that missed a
	// sample still has one.
	require.NotNil(t, p.MemoryLimit)
	assert.EqualValues(t, 3*4096, *p.MemoryLimit)
}

func TestBuildPoint_FullCoverageCountsAreEqual(t *testing.T) {
	live := []row{mkL("web", true, 1, 4096, pgtype.Int8{}), mkL("web", true, 2, 4096, pgtype.Int8{}), mkL("worker", false, 1, 0, pgtype.Int8{})}
	p, ok := build(t, live)
	require.True(t, ok)
	assert.InDelta(t, 20, *p.CPUPercent, 0.0001)
	assert.Equal(t, 2, p.RoutableSampled)
	assert.Equal(t, 2, p.RoutableExpected)
	assert.Equal(t, 3, p.Sampled)
	assert.Equal(t, 3, p.Expected)
	assert.EqualValues(t, 2*4096, *p.MemoryLimit, "an unlimited WORKER does not make the web's ceiling unknown")
}

// Every contributing instance is limited: the ceiling is the sum of the CONFIGURED limits.
// The stub's cgroup limit (1000 each) differs on purpose.
func TestBuildPoint_CeilingIsTheSumOfConfiguredLimitsNotTheSample(t *testing.T) {
	live := []row{mkL("web", true, 1, 4096, pgtype.Int8{}), mkL("web", true, 2, 8192, pgtype.Int8{})}
	p, ok := build(t, live)
	require.True(t, ok)
	require.NotNil(t, p.MemoryLimit)
	assert.EqualValues(t, 4096+8192, *p.MemoryLimit)
	assert.NotEqualValues(t, 2000, *p.MemoryLimit, "2 x the sampled 1000 would be the old, wrong answer")
}

// One unlimited replica makes the ceiling unknown. A partial sum of the limited ones
// would read as a real ceiling that the unlimited one can blow straight through.
func TestBuildPoint_OneUnlimitedInstanceMakesTheCeilingUnknown(t *testing.T) {
	live := []row{mkL("web", true, 1, 4096, pgtype.Int8{}), mkL("web", true, 2, 0, pgtype.Int8{})}
	p, ok := build(t, live)
	require.True(t, ok)
	assert.Nil(t, p.MemoryLimit)
	require.NotNil(t, p.MemoryUsage, "usage is still reported; only the ceiling is unknown")
}

// The process type's limit overrides the application's; 0 on the type is an explicit
// "unlimited", which also overrides.
func TestBuildPoint_ProcessTypeLimitOverridesTheApplications(t *testing.T) {
	live := []row{
		mkL("web", true, 1, 4096, pgtype.Int8{Int64: 1024, Valid: true}),
		mkL("web", true, 2, 4096, pgtype.Int8{Int64: 1024, Valid: true}),
	}
	p, ok := build(t, live)
	require.True(t, ok)
	require.NotNil(t, p.MemoryLimit)
	assert.EqualValues(t, 2048, *p.MemoryLimit, "the type's 1024 each, not the application's 4096")

	unlimited := []row{mkL("web", true, 1, 4096, pgtype.Int8{Int64: 0, Valid: true})}
	p, ok = build(t, unlimited)
	require.True(t, ok)
	assert.Nil(t, p.MemoryLimit, "an explicit 0 on the type means unlimited, whatever the application says")
}

// Nothing routable answered: the usage totals stay null (the ceiling does not -- see below) and the routable count
// is a real 0, not absent. Nothing at all answered: not published, but the counts are
// still filled in for a caller that wants to log them.
func TestBuildPoint_NothingSampledIsZeroCoverageNotMissing(t *testing.T) {
	live := []row{mkL("web", true, 1, 4096, pgtype.Int8{}), mkL("worker", false, 1, 4096, pgtype.Int8{})}

	p, ok := build(t, live, "web-1")
	require.True(t, ok, "the worker answered")
	assert.Nil(t, p.CPUPercent)
	assert.Nil(t, p.MemoryUsage, "no routable sample: usage is unknown")
	require.NotNil(t, p.MemoryLimit, "the ceiling is configuration: known even though nothing routable answered")
	assert.EqualValues(t, 4096, *p.MemoryLimit)
	assert.Equal(t, 0, p.RoutableSampled)
	assert.Equal(t, 1, p.RoutableExpected)

	raw, err := json.Marshal(p)
	require.NoError(t, err)
	assert.Contains(t, string(raw), `"routable_sampled":0`, "present and zero on the wire, not omitted")

	p, ok = build(t, live, "web-1", "worker-1")
	assert.False(t, ok)
	assert.Equal(t, 0, p.Sampled)
	assert.Equal(t, 2, p.Expected)
}

// The rule the deploy path creates a container with is the rule the ceiling is built from.
func TestEffectiveLimits(t *testing.T) {
	cpu, mem := instances.EffectiveLimits(1.5, 4096, pgtype.Float8{}, pgtype.Int8{})
	assert.Equal(t, 1.5, cpu)
	assert.EqualValues(t, 4096, mem)
	cpu, mem = instances.EffectiveLimits(1.5, 4096, pgtype.Float8{Float64: 0.25, Valid: true}, pgtype.Int8{Int64: 512, Valid: true})
	assert.Equal(t, 0.25, cpu)
	assert.EqualValues(t, 512, mem)
}

// No routable instance at all: the ceiling must be null, not &0 -- 0 means "unlimited".
func TestBuildPoint_NoRoutableInstanceHasNoCeilingRatherThanZero(t *testing.T) {
	p, ok := build(t, []row{mkL("worker", false, 1, 4096, pgtype.Int8{})})
	require.True(t, ok)
	assert.Equal(t, 0, p.RoutableExpected)
	assert.Nil(t, p.MemoryLimit)
}
