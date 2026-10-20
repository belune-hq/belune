package proxy_test

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/belune-hq/belune/internal/proxy"
)

func TestNextCaddyState(t *testing.T) {
	one := []proxy.RouteConfig{{Hostname: "a.test"}}
	assert.Equal(t, proxy.CaddyUnreachable, proxy.NextCaddyState(one, errors.New("refused"), 1))
	assert.Equal(t, proxy.CaddyLostConfig, proxy.NextCaddyState(nil, nil, 2))
	assert.Equal(t, proxy.CaddyHealthy, proxy.NextCaddyState(nil, nil, 0), "no domains: nothing to lose")
	assert.Equal(t, proxy.CaddyHealthy, proxy.NextCaddyState(one, nil, 1))
}

// The edge-vs-level decision: a still-broken Caddy must not reconcile again.
func TestProbeShouldReconcile(t *testing.T) {
	const (
		unk, down, ok, lost = proxy.CaddyUnknown, proxy.CaddyUnreachable, proxy.CaddyHealthy, proxy.CaddyLostConfig
	)
	cases := []struct {
		name       string
		prev, next proxy.CaddyState
		errored    bool
		want       bool
	}{
		{"healthy to lost", ok, lost, false, true},
		{"lost stays lost (level, not edge)", lost, lost, false, false},
		{"unreachable to lost", down, lost, false, true},
		{"unknown to lost", unk, lost, false, true},
		{"unreachable to healthy", down, ok, false, true},
		{"lost to healthy needs nothing", lost, ok, false, false},
		{"healthy stays healthy", ok, ok, false, false},
		{"unknown to healthy, opening pass fine", unk, ok, false, false},
		{"unknown to healthy, opening pass failed", unk, ok, true, true},
		{"becoming unreachable", ok, down, false, false},
		{"staying unreachable", down, down, false, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			assert.Equal(t, c.want, proxy.ProbeShouldReconcile(c.prev, c.next, c.errored))
		})
	}
}
