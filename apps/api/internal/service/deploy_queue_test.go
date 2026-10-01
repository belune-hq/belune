package service_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/hibiken/asynq"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/weiliang79/belune/internal/service"
)

type captureEnqueuer struct {
	opts [][]asynq.Option
	errs []error // returned in order, then nil
}

func (c *captureEnqueuer) Enqueue(_ *asynq.Task, opts ...asynq.Option) (*asynq.TaskInfo, error) {
	c.opts = append(c.opts, opts)
	if len(c.errs) > 0 {
		err := c.errs[0]
		c.errs = c.errs[1:]
		return nil, err
	}
	return &asynq.TaskInfo{}, nil
}

type fakeDeleter struct {
	err   error
	calls []string
}

func (f *fakeDeleter) DeleteTask(queue, id string) error {
	f.calls = append(f.calls, queue+"/"+id)
	return f.err
}

func optionValue(t *testing.T, opts []asynq.Option, typ asynq.OptionType) any {
	t.Helper()
	for _, o := range opts {
		if o.Type() == typ {
			return o.Value()
		}
	}
	t.Fatalf("option type %v not set", typ)
	return nil
}

// The mock enqueuer in the handler tests discards options, so nothing there
// can notice a dropped MaxRetry(0) or Timeout — removing either left the whole
// REST suite green. They are pinned here instead.
func TestDeployQueue_EnqueueLikeOptions(t *testing.T) {
	enq := &captureEnqueuer{}
	q := service.NewDeployQueue(nil, enq, &fakeDeleter{}, 30)

	require.NoError(t, q.EnqueueLike("critical", "app-1", asynq.NewTask("deploy", nil)))

	require.Len(t, enq.opts, 1)
	o := enq.opts[0]
	assert.Equal(t, 0, optionValue(t, o, asynq.MaxRetryOpt), "a deploy runs exactly once")
	assert.Equal(t, 30*time.Minute, optionValue(t, o, asynq.TimeoutOpt))
	assert.Equal(t, "deploy:app-1", optionValue(t, o, asynq.TaskIDOpt), "one TaskID serialises every deploy-like op per app")
	assert.Equal(t, "critical", optionValue(t, o, asynq.QueueOpt))
}

// Two spellings of one application id must take the same TaskID, or the guard
// that serialises an app's deploys would let two run at once. Every caller
// passes the raw URL param, so this is where the spelling is normalised.
func TestDeployQueue_TaskIDIsCanonicalAcrossSpellings(t *testing.T) {
	const lower = "0a1b2c3d-4e5f-6789-abcd-ef0123456789"
	enq := &captureEnqueuer{}
	q := service.NewDeployQueue(nil, enq, &fakeDeleter{}, 30)

	for _, spelling := range []string{lower, strings.ToUpper(lower), "{" + lower + "}"} {
		require.NoError(t, q.EnqueueLike("critical", spelling, asynq.NewTask("deploy", nil)))
	}
	require.Len(t, enq.opts, 3)
	for _, o := range enq.opts {
		assert.Equal(t, "deploy:"+lower, optionValue(t, o, asynq.TaskIDOpt))
	}
}

func TestDeployQueue_ReclaimsStaleTaskButNotActiveOne(t *testing.T) {
	t.Run("stale holder is deleted and the task re-enqueued", func(t *testing.T) {
		enq := &captureEnqueuer{errs: []error{asynq.ErrTaskIDConflict}}
		del := &fakeDeleter{}
		q := service.NewDeployQueue(nil, enq, del, 30)

		require.NoError(t, q.EnqueueLike("critical", "app-1", asynq.NewTask("deploy", nil)))
		assert.Equal(t, []string{"critical/deploy:app-1"}, del.calls)
		assert.Len(t, enq.opts, 2)
	})
	t.Run("active holder keeps the original conflict", func(t *testing.T) {
		enq := &captureEnqueuer{errs: []error{asynq.ErrTaskIDConflict}}
		q := service.NewDeployQueue(nil, enq, &fakeDeleter{err: errors.New("active")}, 30)

		err := q.EnqueueLike("critical", "app-1", asynq.NewTask("deploy", nil))
		assert.ErrorIs(t, err, asynq.ErrTaskIDConflict)
		assert.Len(t, enq.opts, 1)
	})
}
