package capability

import (
	"context"
	"errors"
	"io"
	"testing"

	"github.com/weiliang79/belune/internal/runtime"
)

const listModulesOutput = `admin.api.load
caddy.adapters.caddyfile
http.handlers.reverse_proxy
http.handlers.static_response

  Standard modules: 4

admin.api.crowdsec
crowdsec
dns.providers.cloudflare
http.handlers.crowdsec
http.handlers.rate_limit
layer4
layer4.handlers.proxy

  Non-standard modules: 7

  Unknown modules: 0
`

type fakeRuntime struct {
	id       string
	listErr  error
	out      string
	exit     int
	execErr  error
	execs    int
	notFound bool
}

func (f *fakeRuntime) ListSystemContainers(context.Context) ([]runtime.ContainerInfo, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	if f.notFound {
		return []runtime.ContainerInfo{{ID: "x", Name: "other"}}, nil
	}
	return []runtime.ContainerInfo{{ID: f.id, Name: "caddy"}}, nil
}

func (f *fakeRuntime) ContainerExec(_ context.Context, _ string, _ []string, _ io.Reader, stdout, _ io.Writer) (int, error) {
	f.execs++
	_, _ = io.WriteString(stdout, f.out)
	return f.exit, f.execErr
}

func TestProbeParsesRealisticOutput(t *testing.T) {
	p := NewProber(&fakeRuntime{id: "a", out: listModulesOutput}, "caddy")
	s := p.Capabilities(context.Background())

	if !s.RateLimit() || !s.CrowdSec() || !s.Layer4() || !s.DNS01() {
		t.Fatalf("expected rate limit, crowdsec, layer4, dns01 present: %+v", s)
	}
	if s.WAF() {
		t.Fatal("waf is not in the output")
	}
	if s.Has("Standard") || s.Has("Standard modules: 4") {
		t.Fatal("footer leaked in as a module")
	}
}

func TestExactMatchOnly(t *testing.T) {
	// crowdsec's sibling IDs and layer4's children must not stand in for the
	// handler we would actually emit.
	s := parseModules("crowdsec\nadmin.api.crowdsec\nlayer4.handlers.proxy\n")
	if s.CrowdSec() || s.Layer4() {
		t.Fatalf("prefix/sibling IDs counted as present: %+v", s)
	}
}

func TestDNS01NeedsAnyProvider(t *testing.T) {
	if NewSet("http.handlers.rate_limit").DNS01() {
		t.Fatal("no provider, DNS01 must be false")
	}
	if !NewSet("dns.providers.route53").DNS01() {
		t.Fatal("any provider should enable DNS01")
	}
}

func TestFailsClosed(t *testing.T) {
	cases := map[string]*fakeRuntime{
		"list error":   {listErr: errors.New("docker down")},
		"no container": {notFound: true},
		"exec error":   {id: "a", out: listModulesOutput, execErr: errors.New("boom")},
		"exit code":    {id: "a", out: listModulesOutput, exit: 1},
		"empty out":    {id: "a", out: "\n"},
	}
	for name, rt := range cases {
		t.Run(name, func(t *testing.T) {
			s := NewProber(rt, "caddy").Capabilities(context.Background())
			if s.RateLimit() || s.CrowdSec() || s.Layer4() || s.DNS01() || s.WAF() {
				t.Fatalf("failed probe reported capabilities: %+v", s)
			}
		})
	}
}

func TestCachesPerContainerID(t *testing.T) {
	rt := &fakeRuntime{id: "a", out: listModulesOutput}
	p := NewProber(rt, "caddy")
	ctx := context.Background()

	p.Capabilities(ctx)
	p.Capabilities(ctx)
	if rt.execs != 1 {
		t.Fatalf("same container ID re-exec'd: %d execs", rt.execs)
	}

	// Recreated container, now without the module.
	rt.id, rt.out = "b", "http.handlers.reverse_proxy\n"
	s := p.Capabilities(ctx)
	if rt.execs != 2 {
		t.Fatalf("new container ID did not re-probe: %d execs", rt.execs)
	}
	if s.RateLimit() {
		t.Fatal("stale capabilities served after recreate")
	}
}

func TestFailureIsNotCached(t *testing.T) {
	rt := &fakeRuntime{id: "a", execErr: errors.New("starting up")}
	p := NewProber(rt, "caddy")
	ctx := context.Background()

	if p.Capabilities(ctx).RateLimit() {
		t.Fatal("failed probe reported present")
	}
	rt.execErr, rt.out = nil, listModulesOutput
	if !p.Capabilities(ctx).RateLimit() {
		t.Fatal("a transient failure pinned capabilities off")
	}
}

func TestNilAndZeroSetAreEmpty(t *testing.T) {
	var s Set
	if s.Has("layer4") || s.DNS01() {
		t.Fatal("zero Set must be empty")
	}
}
