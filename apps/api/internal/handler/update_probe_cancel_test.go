package handler

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/belune-hq/belune/internal/runtime"
)

// ctxRuntime behaves like the real Docker client: it honours the
// caller's context and errors when it is already dead.
type ctxRuntime struct{ runtime.ContainerRuntime }

func (c *ctxRuntime) ListAllContainers(ctx context.Context) ([]runtime.ContainerInfo, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return []runtime.ContainerInfo{{
		ID: "h", Status: "running", CreatedAt: time.Unix(900, 0),
		Labels: map[string]string{runtime.LabelUpdateHelper: "true"},
	}}, nil
}

// FINDING 2 probe: can ONE client's cancelled request hide a running update
// from every other client for a whole TTL?
func TestUpdateProbe_CancelledRequestDoesNotPoisonTheCache(t *testing.T) {
	old := updateProbeTTL
	updateProbeTTL = 5 * time.Second
	t.Cleanup(func() { updateProbeTTL = old })

	rt := &ctxRuntime{}
	now := time.Unix(1000, 0)
	p := &updateProbe{now: func() time.Time { return now }}

	// A healthy read sees the update.
	running, _ := p.helper(context.Background(), rt)
	assert.True(t, running, "sanity: a running helper is visible")

	// Force a refresh, then let ONE poller's request be cancelled mid-flight —
	// a closed tab, a navigation, or the route's own 15s timeout.
	now = now.Add(6 * time.Second)
	dead, cancel := context.WithCancel(context.Background())
	cancel()
	p.helper(dead, rt)

	// Every other client now asks, with a perfectly good context.
	running, _ = p.helper(context.Background(), rt)
	assert.True(t, running,
		"one client's cancellation must not blank the update flag for everyone else for a TTL")
}
