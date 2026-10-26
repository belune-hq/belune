package handler

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/belune-hq/belune/internal/proxy"
)

// A handler with no reconciler (tests, a build that does not wire one) reports
// nothing rather than panicking.
func TestNotAppliedByRoute_NilReconciler(t *testing.T) {
	assert.Nil(t, (&Handler{}).notAppliedByRoute())
}

// fixedStatus is a reconciler whose status is whatever the test says.
type fixedStatus struct{ st proxy.ReconcilerStatus }

func (f fixedStatus) Status() proxy.ReconcilerStatus     { return f.st }
func (f fixedStatus) ReconcileNow(context.Context) error { return nil }
func (f fixedStatus) WAF() proxy.WAFState                { return proxy.WAFState{} }
func (f fixedStatus) CrowdSec() proxy.CrowdSecState      { return proxy.CrowdSecState{} }

// The grace period must not collapse when IntervalSeconds is zero. Zero is what
// an unconfigured or stubbed reconciler reports, and an unfloored
// 2*0 == no grace at all reports a degradation the instant it appears — which
// during an update, when Caddy is briefly unprobeable, is exactly the false
// alarm the grace period exists to prevent.
func TestNotAppliedByRoute_GraceIsFlooredAtAZeroInterval(t *testing.T) {
	fresh := proxy.Degradation{
		Hostname: "a.test", Path: "/", Feature: "rate_limit",
		Module: "http.handlers.rate_limit", Reason: proxy.ReasonModulesUnknown,
		Since: time.Now().Add(-5 * time.Second),
	}
	standing := fresh
	standing.Since = time.Now().Add(-2 * minNotAppliedGrace)

	h := &Handler{reconciler: fixedStatus{proxy.ReconcilerStatus{
		IntervalSeconds: 0,
		Degraded:        []proxy.Degradation{fresh},
	}}}
	assert.Empty(t, h.notAppliedByRoute(), "5s old against a zero interval: still inside the floor")

	h = &Handler{reconciler: fixedStatus{proxy.ReconcilerStatus{
		IntervalSeconds: 0,
		Degraded:        []proxy.Degradation{standing},
	}}}
	// The control: the floor delays the report, it must not suppress it for ever.
	assert.Len(t, h.notAppliedByRoute()[routeKey{"a.test", "/"}], 1,
		"past the floor it is reported even with no interval configured")
}
