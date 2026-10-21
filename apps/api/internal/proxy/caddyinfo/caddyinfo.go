// Package caddyinfo describes the running Caddy itself — which container, what
// version, which modules — for the dashboard's proxy panel.
//
// The rule that shapes every type here: a field distinguishes "we could not
// determine this" from "this is absent". Caddy can run outside Docker, restart
// mid-request, or be a bring-your-own image with no labels, and a panel that
// reports "no modules" when it merely could not exec sends someone to rebuild an
// image that was fine. Unknown is therefore a value (nil, or Known=false), never
// a zero.
package caddyinfo

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/belune-hq/belune/internal/config"
	"github.com/belune-hq/belune/internal/proxy/capability"
)

// Container states.
const (
	ContainerFound    = "found"
	ContainerNotFound = "not_found" // the runtime answered and no container has the name
	ContainerUnknown  = "unknown"   // the runtime could not be asked
)

// Where a version came from.
const (
	VersionFromLabel = "label"
	VersionFromExec  = "exec"
)

const (
	// labelImageVersion is the OCI version label; labelUpstream is Belune's own.
	// The first is only trusted together with the second: a bring-your-own image
	// can set org.opencontainers.image.version to ITS version, not Caddy's.
	labelImageVersion = "org.opencontainers.image.version"
	labelUpstream     = "dev.belune.caddy.upstream"

	execTimeout = 5 * time.Second
)

// Report is everything the panel shows about Caddy itself.
type Report struct {
	Discovery Discovery `json:"discovery"`
	Admin     Admin     `json:"admin"`
	Container Container `json:"container"`
	Version   Version   `json:"version"`
	// Image is the reference the container was created from. Nil when unknown,
	// which includes the case where Docker reports an image ID instead because
	// the tag has since moved: an ID is not a reference.
	Image   *string `json:"image"`
	Modules Modules `json:"modules"`
}

// Discovery is how the container's name was arrived at.
type Discovery struct {
	// Source is env, label or assumed (config.CaddySource*); empty only if
	// discovery never ran. "assumed" means nothing verified the name.
	Source        string `json:"source"`
	ContainerName string `json:"container_name"`
}

// Admin is Caddy's admin API as the reconciler last saw it.
type Admin struct {
	URL string `json:"url"`
	// State is the reconciler's own: unknown, unreachable, healthy, lost_config.
	State string `json:"state"`
}

type Container struct {
	State string `json:"state"`
	// Status is Docker's state string (running, exited, …); empty unless found.
	Status string `json:"status,omitempty"`
}

// Version is nil-valued when it could not be determined.
type Version struct {
	// Value is the version as the image reports it (the Belune image appends a
	// build suffix: 2.11.7-3). Upstream is Caddy's own number.
	Value    *string `json:"value"`
	Upstream *string `json:"upstream"`
	// Source is label or exec; nil when Value is nil.
	Source *string `json:"source"`
}

type Modules struct {
	// Known is false when the probe could not read Caddy's modules. IDs is then
	// empty and says nothing about what Caddy has.
	Known bool     `json:"known"`
	IDs   []string `json:"ids"`
}

// Inspector builds Reports. One per process; safe for concurrent use.
type Inspector struct {
	cfg  *config.Config
	rt   capability.Runtime
	caps capability.Provider

	mu         sync.Mutex
	cachedID   string
	cachedExec string // result of `caddy version` for cachedID
}

// New returns an Inspector. rt and caps may be nil (Caddy not managed here, or
// not wired in a test); every dependent field then reports unknown.
func New(cfg *config.Config, rt capability.Runtime, caps capability.Provider) *Inspector {
	return &Inspector{cfg: cfg, rt: rt, caps: caps}
}

// Inspect assembles a Report. adminState is the reconciler's probe state;
// passed in, not re-probed, so the panel and the reconciler cannot disagree.
// Never blocks on an exec in the steady state: modules come from the shared
// prober's cache, the version from labels or a cache keyed on the container ID.
func (i *Inspector) Inspect(ctx context.Context, adminState string) Report {
	if adminState == "" {
		adminState = "unknown"
	}
	r := Report{
		Discovery: Discovery{Source: i.cfg.CaddyContainerSource, ContainerName: i.cfg.CaddyContainerName},
		Admin:     Admin{URL: i.cfg.CaddyAdminURL, State: adminState},
		Container: Container{State: ContainerUnknown},
		Modules:   Modules{IDs: []string{}},
	}

	if i.caps != nil {
		if set := i.caps.Capabilities(ctx); set.Known() {
			r.Modules = Modules{Known: true, IDs: set.IDs()}
		}
	}
	if i.rt == nil {
		return r
	}

	c, err := capability.FindContainer(ctx, i.rt, i.cfg.CaddyContainerName)
	switch {
	case errors.Is(err, capability.ErrContainerNotFound):
		r.Container.State = ContainerNotFound
		return r
	case err != nil:
		slog.Debug("caddy panel: could not look up the caddy container", "error", err)
		return r
	}
	r.Container = Container{State: ContainerFound, Status: c.Status}

	// `sha256:…` is what Docker substitutes once the tag has moved off the image
	// the container runs, so it names the image but is not the reference.
	if c.Image != "" && !strings.HasPrefix(c.Image, "sha256:") {
		img := c.Image
		r.Image = &img
	}

	// The OCI label alone is not enough; see labelImageVersion.
	if up := c.Labels[labelUpstream]; up != "" && c.Labels[labelImageVersion] != "" {
		v, src := c.Labels[labelImageVersion], VersionFromLabel
		r.Version = Version{Value: &v, Upstream: &up, Source: &src}
		return r
	}

	if c.Status == "running" {
		if v := i.execVersion(ctx, c.ID); v != "" {
			src := VersionFromExec
			r.Version = Version{Value: &v, Upstream: &v, Source: &src}
		}
	}
	return r
}

// execVersion runs `caddy version`, cached on the container ID because a
// recreated container is exactly when the answer can change. Only a success is
// cached, so a transient failure retries on the next poll.
func (i *Inspector) execVersion(ctx context.Context, containerID string) string {
	i.mu.Lock()
	defer i.mu.Unlock()
	if containerID == i.cachedID {
		return i.cachedExec
	}

	ctx, cancel := context.WithTimeout(ctx, execTimeout)
	defer cancel()
	var stdout, stderr bytes.Buffer
	exit, err := i.rt.ContainerExec(ctx, i.cfg.CaddyContainerName, []string{"caddy", "version"}, nil, &stdout, &stderr)
	if err != nil || exit != 0 {
		slog.Debug("caddy panel: caddy version failed", "error", err, "exit", exit)
		return ""
	}
	v := parseVersion(stdout.String())
	if v == "" {
		return ""
	}
	i.cachedID, i.cachedExec = containerID, v
	return v
}

// parseVersion takes the first token of `caddy version` ("v2.11.7 h1:…") and
// drops the leading v. Anything not shaped like a version is rejected rather
// than shown, so unexpected output reads as unknown.
func parseVersion(out string) string {
	fields := strings.Fields(out)
	if len(fields) == 0 {
		return ""
	}
	v := strings.TrimPrefix(fields[0], "v")
	if v == "" || v[0] < '0' || v[0] > '9' {
		return ""
	}
	return v
}
