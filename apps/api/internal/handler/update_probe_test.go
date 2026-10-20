package handler

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/belune-hq/belune/internal/runtime"
)

// countingRuntime counts the container listings the cache is meant to absorb.
// Embeds the interface rather than testutil's mock: testutil imports this
// package through the server, so an internal test cannot import it back.
type countingRuntime struct {
	runtime.ContainerRuntime
	lists atomic.Int32
}

func (c *countingRuntime) ListAllContainers(_ context.Context) ([]runtime.ContainerInfo, error) {
	c.lists.Add(1)
	return nil, nil
}

func TestUpdateProbe_CachesTheContainerRead(t *testing.T) {
	old := updateProbeTTL
	updateProbeTTL = 5 * time.Second
	t.Cleanup(func() { updateProbeTTL = old })

	rt := &countingRuntime{}
	now := time.Unix(1000, 0)
	p := &updateProbe{now: func() time.Time { return now }}
	ctx := context.Background()

	for range 20 {
		p.helper(ctx, rt)
	}
	assert.EqualValues(t, 1, rt.lists.Load(), "every poller in the TTL shares one Docker call")

	now = now.Add(6 * time.Second)
	p.helper(ctx, rt)
	assert.EqualValues(t, 2, rt.lists.Load(), "and it refreshes once stale")
}

func TestUpdateProbe_MarkRunningSurvivesAStaleNegative(t *testing.T) {
	old := updateProbeTTL
	updateProbeTTL = 5 * time.Second
	t.Cleanup(func() { updateProbeTTL = old })

	rt := &countingRuntime{}
	now := time.Unix(1000, 0)
	p := &updateProbe{now: func() time.Time { return now }}
	ctx := context.Background()

	running, _ := p.helper(ctx, rt)
	assert.False(t, running, "primed with the pre-spawn answer: no helper")

	p.markRunning(now)
	running, _ = p.helper(ctx, rt)
	assert.True(t, running, "a helper this process just started must not read as absent for a TTL")
}
