package caddyinfo

import (
	"context"
	"errors"
	"io"
	"testing"

	"github.com/belune-hq/belune/internal/config"
	"github.com/belune-hq/belune/internal/proxy/capability"
	"github.com/belune-hq/belune/internal/runtime"
)

type fakeRT struct {
	system, all []runtime.ContainerInfo
	listErr     error
	execOut     string
	execExit    int
	execErr     error
	execCalls   int
}

func (f *fakeRT) ListSystemContainers(context.Context) ([]runtime.ContainerInfo, error) {
	return f.system, f.listErr
}
func (f *fakeRT) ListAllContainers(context.Context) ([]runtime.ContainerInfo, error) {
	return f.all, f.listErr
}
func (f *fakeRT) ContainerExec(_ context.Context, _ string, _ []string, _ io.Reader, stdout, _ io.Writer) (int, error) {
	f.execCalls++
	_, _ = io.WriteString(stdout, f.execOut)
	return f.execExit, f.execErr
}

type fakeCaps struct{ set capability.Set }

func (f fakeCaps) Capabilities(context.Context) capability.Set { return f.set }

func cfg() *config.Config {
	return &config.Config{
		CaddyContainerName:   "belune-caddy-1",
		CaddyContainerSource: config.CaddySourceLabel,
		CaddyAdminURL:        "http://caddy:2019",
	}
}

func belune() runtime.ContainerInfo {
	return runtime.ContainerInfo{
		ID: "abc", Name: "belune-caddy-1", Status: "running",
		Image: "ghcr.io/belune-hq/belune-caddy:2",
		Labels: map[string]string{
			labelImageVersion: "2.11.7-3",
			labelUpstream:     "2.11.7",
		},
	}
}

func TestVersionFromLabelsNeedsNoExec(t *testing.T) {
	rt := &fakeRT{system: []runtime.ContainerInfo{belune()}}
	r := New(cfg(), rt, fakeCaps{}).Inspect(context.Background(), "healthy")

	if r.Version.Value == nil || *r.Version.Value != "2.11.7-3" || *r.Version.Upstream != "2.11.7" || *r.Version.Source != VersionFromLabel {
		t.Fatalf("version = %+v", r.Version)
	}
	if r.Image == nil || *r.Image != "ghcr.io/belune-hq/belune-caddy:2" {
		t.Errorf("image = %v", r.Image)
	}
	if rt.execCalls != 0 {
		t.Errorf("labels were enough, yet exec ran %d times", rt.execCalls)
	}
}

// A bring-your-own image may use the OCI version label for ITS version. Without
// the Belune marker label it must not be shown as Caddy's.
func TestForeignVersionLabelIsNotTrusted(t *testing.T) {
	c := belune()
	c.Labels = map[string]string{labelImageVersion: "1.0.0"}
	rt := &fakeRT{system: []runtime.ContainerInfo{c}, execOut: "v2.10.0 h1:abc\n"}

	r := New(cfg(), rt, fakeCaps{}).Inspect(context.Background(), "healthy")
	if r.Version.Value == nil || *r.Version.Value != "2.10.0" || *r.Version.Source != VersionFromExec {
		t.Fatalf("version = %+v, want 2.10.0 via exec", r.Version)
	}
}

func TestExecVersionCachedPerContainerID(t *testing.T) {
	c := belune()
	c.Labels = nil
	rt := &fakeRT{system: []runtime.ContainerInfo{c}, execOut: "v2.10.0 h1:abc\n"}
	in := New(cfg(), rt, fakeCaps{})

	in.Inspect(context.Background(), "healthy")
	in.Inspect(context.Background(), "healthy")
	if rt.execCalls != 1 {
		t.Fatalf("exec ran %d times for one container, want 1", rt.execCalls)
	}

	c.ID = "recreated"
	rt.system = []runtime.ContainerInfo{c}
	in.Inspect(context.Background(), "healthy")
	if rt.execCalls != 2 {
		t.Fatalf("a recreated container must be asked again; exec calls = %d", rt.execCalls)
	}
}

// The failure case the whole package is for: not being able to ask must read as
// unknown, never as a version or an absence.
func TestUndeterminableIsNilNotEmpty(t *testing.T) {
	c := belune()
	c.Labels = nil
	for name, rt := range map[string]*fakeRT{
		"exec errors":    {system: []runtime.ContainerInfo{c}, execErr: errors.New("boom")},
		"exec exits 1":   {system: []runtime.ContainerInfo{c}, execExit: 1},
		"garbage output": {system: []runtime.ContainerInfo{c}, execOut: "<html>"},
		"not running":    {system: []runtime.ContainerInfo{func() runtime.ContainerInfo { x := c; x.Status = "exited"; return x }()}},
	} {
		t.Run(name, func(t *testing.T) {
			r := New(cfg(), rt, fakeCaps{}).Inspect(context.Background(), "")
			if r.Version.Value != nil || r.Version.Upstream != nil || r.Version.Source != nil {
				t.Errorf("version = %+v, want all nil", r.Version)
			}
		})
	}
}

func TestContainerStates(t *testing.T) {
	ctx := context.Background()

	r := New(cfg(), &fakeRT{}, fakeCaps{}).Inspect(ctx, "")
	if r.Container.State != ContainerNotFound {
		t.Errorf("empty runtime = %q, want not_found", r.Container.State)
	}

	r = New(cfg(), &fakeRT{listErr: errors.New("docker down")}, fakeCaps{}).Inspect(ctx, "")
	if r.Container.State != ContainerUnknown {
		t.Errorf("failing runtime = %q, want unknown — it must not read as not_found", r.Container.State)
	}

	r = New(cfg(), nil, nil).Inspect(ctx, "")
	if r.Container.State != ContainerUnknown || r.Modules.Known || r.Version.Value != nil || r.Image != nil {
		t.Errorf("no runtime wired must report everything unknown, got %+v", r)
	}
	if r.Admin.State != "unknown" {
		t.Errorf("empty admin state must default to unknown, got %q", r.Admin.State)
	}
}

func TestModulesKnownVersusEmpty(t *testing.T) {
	ctx := context.Background()
	rt := &fakeRT{system: []runtime.ContainerInfo{belune()}}

	r := New(cfg(), rt, fakeCaps{}).Inspect(ctx, "")
	if r.Modules.Known || len(r.Modules.IDs) != 0 {
		t.Errorf("a probe that could not ask must be Known=false, got %+v", r.Modules)
	}

	r = New(cfg(), rt, fakeCaps{set: capability.NewSet("http", "layer4")}).Inspect(ctx, "")
	if !r.Modules.Known || len(r.Modules.IDs) != 2 || r.Modules.IDs[0] != "http" {
		t.Errorf("modules = %+v", r.Modules)
	}
}

func TestMovedTagImageIDIsNotAReference(t *testing.T) {
	c := belune()
	c.Image = "sha256:c232cfa0dbee"
	r := New(cfg(), &fakeRT{system: []runtime.ContainerInfo{c}}, fakeCaps{}).Inspect(context.Background(), "")
	if r.Image != nil {
		t.Errorf("image = %q, want nil", *r.Image)
	}
}

func TestDiscoveryIsPassedThrough(t *testing.T) {
	c := cfg()
	c.CaddyContainerSource = config.CaddySourceAssumed
	r := New(c, nil, nil).Inspect(context.Background(), "healthy")
	if r.Discovery.Source != "assumed" || r.Discovery.ContainerName != "belune-caddy-1" || r.Admin.URL != "http://caddy:2019" {
		t.Errorf("discovery/admin = %+v %+v", r.Discovery, r.Admin)
	}
}
