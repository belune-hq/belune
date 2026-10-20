package logger_test

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/belune-hq/belune/internal/pkg/logger"
)

func TestIsTransientConnError(t *testing.T) {
	// The transient cases are verbatim from a real full-stack restart log: asynq
	// and go-redis emit these for the few seconds before redis is reachable.
	transient := []string{
		`Dequeue error: UNKNOWN: redis eval error: dial tcp 172.18.0.3:6379: connect: connection refused`,
		`Failed to delete expired completed tasks from queue "low": INTERNAL_ERROR: redis eval error: dial tcp: lookup redis on 127.0.0.11:53: no such host`,
		`redis: discarding bad PubSub connection: EOF`,
		`read tcp 172.18.0.4:6379: i/o timeout`,
		`write tcp 172.18.0.4:6379: connection reset by peer`,
	}
	for _, m := range transient {
		assert.True(t, logger.IsTransientConnError(m), "should be transient: %q", m)
	}

	// Genuine faults must stay at Error — they are not connectivity churn.
	real := []string{
		`could not decode task payload: invalid character`,
		`handler returned a non-nil error for task deploy`,
		`panic recovered while processing task`,
		`NOAUTH Authentication required`,
	}
	for _, m := range real {
		assert.False(t, logger.IsTransientConnError(m), "should not be transient: %q", m)
	}
}
