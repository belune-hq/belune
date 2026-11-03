package worker

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/belune-hq/belune/internal/runtime"
)

// recordingRuntime embeds the interface so only the two calls a compensator makes
// need implementing; anything else would nil-panic, which is the right outcome.
type recordingRuntime struct {
	runtime.ContainerRuntime
	stopped, removed []string
}

func (r *recordingRuntime) StopContainer(_ context.Context, id string) error {
	r.stopped = append(r.stopped, id)
	return nil
}

func (r *recordingRuntime) RemoveContainer(_ context.Context, id string) error {
	r.removed = append(r.removed, id)
	return nil
}

// A deploy that creates two containers shares ONE deployContext, and each
// createAndStart overwrites dc.containerID. The rollback must still remove BOTH
// containers, each by its own id: a closure that read dc.containerID when it ran
// would remove the second twice and orphan the first. Asserting WHICH ids, not how
// many calls, is the point -- "removes one twice" and "removes two once" are the same
// count.
func TestContainerCompensators_EachRemovesItsOwnContainer(t *testing.T) {
	rt := &recordingRuntime{}
	dc := &deployContext{runtime: rt}
	h := &TaskHandler{}

	for _, id := range []string{"worker-container", "web-container"} {
		dc.containerID = id // what createAndStart does after each create
		dc.addContainerCompensator(id)
	}
	h.runCompensators(dc)

	assert.ElementsMatch(t, []string{"worker-container", "web-container"}, rt.removed, "neither container may be orphaned")
	assert.ElementsMatch(t, []string{"worker-container", "web-container"}, rt.stopped)
	// Newest first, like every compensator: web unwinds before worker.
	assert.Equal(t, []string{"web-container", "worker-container"}, rt.removed)
}
