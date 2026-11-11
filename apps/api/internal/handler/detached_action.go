package handler

import (
	"context"
	"net/http"
	"time"
)

// appActionWait is how long a Stop, Start or Restart request (of an application or a database)
// waits for the containers before answering 202 and letting the work finish on its own. Under the
// 15s handler timeout, with room to answer; a var only so a test can shorten it.
var appActionWait = 10 * time.Second

// actionState says how awaitDetached ended.
type actionState int

const (
	// actionDone: the work finished within the wait; its result and error are valid.
	actionDone actionState = iota
	// actionSlow: still running after the wait. The caller answers 202 and the work carries on.
	actionSlow
	// actionAbandoned: the client or the timeout handler is gone. Nothing to write to.
	actionAbandoned
)

// awaitDetached runs do on a context DETACHED from the request and waits up to appActionWait for it.
//
// ⛔ The request only decides how long to WAIT, never whether the work completes. Stopping a
// container that ignores SIGTERM takes Docker's whole 30s grace, so a route under the 15s handler
// timeout cannot be allowed to cancel the work, or the stop is cut off and the start that follows
// silently does nothing. See server.handlerTimeout. Each caller keeps its own error mapping and
// response shape; only the hand-over lives here, so applications and databases cannot drift apart.
func awaitDetached[T any](r *http.Request, do func(ctx context.Context) (T, error)) (res T, state actionState, err error) {
	type result struct {
		v   T
		err error
	}
	done := make(chan result, 1)
	go func() {
		v, err := do(context.WithoutCancel(r.Context()))
		done <- result{v, err}
	}()

	select {
	case out := <-done:
		return out.v, actionDone, out.err
	case <-time.After(appActionWait):
		return res, actionSlow, nil
	case <-r.Context().Done():
		return res, actionAbandoned, nil
	}
}
