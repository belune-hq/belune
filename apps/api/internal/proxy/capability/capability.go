// Package capability asks the running Caddy which modules it was built with.
//
// Belune's Caddy image carries extra modules, but stock caddy:2-alpine is a
// supported configuration too, so every module-backed feature is an optional
// capability: absent means "not configured", never an error. Callers must go
// through a Set rather than assume anything from the image tag, which does not
// describe its contents (and bring-your-own Caddy is exactly the case that
// would break such an inference).
package capability

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/weiliang79/belune/internal/runtime"
)

// Module IDs as `caddy list-modules` prints them. Matched exactly: crowdsec
// also registers `crowdsec` and `admin.api.crowdsec`, and layer4 a family of
// `layer4.handlers.*`, none of which prove the handler we would emit exists.
const (
	ModuleRateLimit = "http.handlers.rate_limit"
	ModuleWAF       = "http.handlers.waf"
	ModuleCrowdSec  = "http.handlers.crowdsec"
	ModuleLayer4    = "layer4"
)

var dnsProviderModules = []string{
	"dns.providers.cloudflare",
	"dns.providers.route53",
	"dns.providers.digitalocean",
}

// probeTimeout bounds one exec. list-modules is local and fast; a hang means
// the container is unwell, and the answer is "absent" either way.
const probeTimeout = 10 * time.Second

// Set is the modules a Caddy has. The zero value (and nil) has none, which is
// what every failed probe returns.
type Set struct {
	modules map[string]struct{}
}

// NewSet builds a Set from module IDs; tests and fakes use it.
func NewSet(ids ...string) Set {
	s := Set{modules: make(map[string]struct{}, len(ids))}
	for _, id := range ids {
		s.modules[id] = struct{}{}
	}
	return s
}

// Has reports whether the exact module ID is present.
func (s Set) Has(moduleID string) bool {
	_, ok := s.modules[moduleID]
	return ok
}

func (s Set) RateLimit() bool { return s.Has(ModuleRateLimit) }
func (s Set) WAF() bool       { return s.Has(ModuleWAF) }
func (s Set) CrowdSec() bool  { return s.Has(ModuleCrowdSec) }
func (s Set) Layer4() bool    { return s.Has(ModuleLayer4) }

// DNS01 is true when any supported DNS provider is present; there is no single
// module that means "DNS-01 works".
func (s Set) DNS01() bool {
	for _, id := range dnsProviderModules {
		if s.Has(id) {
			return true
		}
	}
	return false
}

// Provider reports a Caddy's capabilities. Implementations fail closed: when
// they cannot tell, the Set is empty.
type Provider interface {
	Capabilities(ctx context.Context) Set
}

// Runtime is the slice of the container runtime the probe needs.
type Runtime interface {
	ListSystemContainers(ctx context.Context) ([]runtime.ContainerInfo, error)
	ListAllContainers(ctx context.Context) ([]runtime.ContainerInfo, error)
	ContainerExec(ctx context.Context, containerName string, cmd []string, stdin io.Reader, stdout, stderr io.Writer) (int, error)
}

// The full runtime must keep satisfying the probe's narrow view of it.
var _ Runtime = runtime.ContainerRuntime(nil)

// Prober runs `caddy list-modules` in the Caddy container.
//
// The result is cached against the container's ID, not a timer: the reconciler
// asks every 30s, and a recreated container (the image changed) gets a new ID,
// which is exactly when the module set can have changed. Only successes are
// cached, so a transient failure heals on the next call instead of pinning
// every feature off until the next recreate.
type Prober struct {
	rt            Runtime
	containerName string

	mu       sync.Mutex
	cachedID string
	cached   Set
}

func NewProber(rt Runtime, containerName string) *Prober {
	return &Prober{rt: rt, containerName: containerName}
}

// Capabilities returns the running Caddy's module set, or an empty Set if it
// cannot be determined. An empty Set is recoverable; assuming presence would
// write a config Caddy rejects.
func (p *Prober) Capabilities(ctx context.Context) Set {
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()

	p.mu.Lock()
	defer p.mu.Unlock()

	set, err := p.probe(ctx)
	if err != nil {
		slog.Warn("caddy capability probe failed; treating optional modules as absent",
			"container", p.containerName, "error", err)
		return Set{}
	}
	return set
}

// probe must be called with p.mu held.
func (p *Prober) probe(ctx context.Context) (Set, error) {
	id, err := p.containerID(ctx)
	if err != nil {
		return Set{}, err
	}
	if id == p.cachedID {
		return p.cached, nil
	}

	var stdout, stderr bytes.Buffer
	exit, err := p.rt.ContainerExec(ctx, p.containerName, []string{"caddy", "list-modules"}, nil, &stdout, &stderr)
	if err != nil {
		return Set{}, fmt.Errorf("running caddy list-modules: %w", err)
	}
	if exit != 0 {
		return Set{}, fmt.Errorf("caddy list-modules exited %d: %s", exit, strings.TrimSpace(stderr.String()))
	}

	set := parseModules(stdout.String())
	// Even stock Caddy has ~100 modules, so none means the output was not what
	// we think it is, and an empty answer must not be remembered as truth.
	if len(set.modules) == 0 {
		return Set{}, fmt.Errorf("caddy list-modules printed no modules")
	}

	p.cachedID, p.cached = id, set
	return set, nil
}

func (p *Prober) containerID(ctx context.Context) (string, error) {
	// The labelled list first: it is the normal case and the cheap query.
	containers, err := p.rt.ListSystemContainers(ctx)
	if err != nil {
		return "", fmt.Errorf("listing system containers: %w", err)
	}
	if id := findID(containers, p.containerName); id != "" {
		return id, nil
	}

	// ⚠️ Then every container, because ListSystemContainers filters on the
	// belune-system label and CADDY_CONTAINER_NAME is the documented escape
	// hatch for a Caddy that does not carry it (app.go:374 returns before the
	// label is ever consulted). Without this fallback a bring-your-own Caddy
	// named explicitly would route fine and report every optional module
	// absent — the silent half of a failure is the half that costs time.
	all, err := p.rt.ListAllContainers(ctx)
	if err != nil {
		return "", fmt.Errorf("listing all containers: %w", err)
	}
	if id := findID(all, p.containerName); id != "" {
		return id, nil
	}
	return "", fmt.Errorf("no container named %q", p.containerName)
}

func findID(containers []runtime.ContainerInfo, name string) string {
	for _, c := range containers {
		if c.Name == name && c.ID != "" {
			return c.ID
		}
	}
	return ""
}

var moduleIDPattern = regexp.MustCompile(`^[A-Za-z0-9_]+(\.[A-Za-z0-9_]+)*$`)

// parseModules keeps the lines that are a bare module ID. The section footers
// ("  Standard modules: 107") contain spaces and fall out of the pattern, so a
// changed footer cannot leak in as a module.
func parseModules(out string) Set {
	set := NewSet()
	for line := range strings.Lines(out) {
		line = strings.TrimSpace(line)
		if moduleIDPattern.MatchString(line) {
			set.modules[line] = struct{}{}
		}
	}
	return set
}
