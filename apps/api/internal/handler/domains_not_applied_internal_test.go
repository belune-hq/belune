package handler

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// A handler with no reconciler (tests, a build that does not wire one) reports
// nothing rather than panicking.
func TestNotAppliedByRoute_NilReconciler(t *testing.T) {
	assert.Nil(t, (&Handler{}).notAppliedByRoute())
}
