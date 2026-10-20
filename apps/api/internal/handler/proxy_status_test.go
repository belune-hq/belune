package handler_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/belune-hq/belune/internal/config"
	"github.com/belune-hq/belune/internal/logtailer"
	"github.com/belune-hq/belune/internal/proxy"
	"github.com/belune-hq/belune/internal/proxy/capability"
	"github.com/belune-hq/belune/internal/runtime"
	"github.com/belune-hq/belune/internal/testutil"
)

// caddyPanelFixture resets everything the panel reads and restores it after,
// because env is shared across the package's tests.
func caddyPanelFixture(t *testing.T) (token string) {
	t.Helper()
	resetDB(t)
	token = env.SetupAdmin(t, "admin@test.com", "password123")

	prev := *env.Config
	env.Config.CaddyContainerName = "belune-caddy-1"
	env.Config.CaddyContainerSource = config.CaddySourceLabel
	t.Cleanup(func() {
		env.Config.CaddyContainerName = prev.CaddyContainerName
		env.Config.CaddyContainerSource = prev.CaddyContainerSource
		env.Runtime.ListSystemContainers_ = nil
		env.Runtime.ListAllContainers_ = nil
		env.Runtime.ListContainersErr = nil
		env.Runtime.ExecFunc = nil
		env.Capabilities.Set(capability.Set{})
		env.Reconciler.Status_ = proxy.ReconcilerStatus{}
		env.AccessLog.Set(logtailer.Status{})
	})
	return token
}

func getCaddyPanel(t *testing.T, token string) map[string]any {
	t.Helper()
	resp := env.DoRequest(t, "GET", "/api/maintenance/proxy", nil, testutil.AuthHeader(token))
	require.Equal(t, http.StatusOK, resp.StatusCode)
	body := testutil.ReadJSON(t, resp)
	caddy, ok := body["caddy"].(map[string]any)
	require.True(t, ok, "response has no caddy object: %v", body)
	return caddy
}

func beluneCaddyContainer(id string) runtime.ContainerInfo {
	return runtime.ContainerInfo{
		ID: id, Name: "belune-caddy-1", Status: "running",
		Image: "ghcr.io/belune-hq/belune-caddy:2",
		Labels: map[string]string{
			"belune-system":                    "caddy",
			"org.opencontainers.image.version": "2.11.7-3",
			"dev.belune.caddy.upstream":        "2.11.7",
		},
	}
}

func TestProxyPanel_ReportsEachDiscoverySource(t *testing.T) {
	token := caddyPanelFixture(t)

	for _, src := range []string{config.CaddySourceEnv, config.CaddySourceLabel, config.CaddySourceAssumed} {
		env.Config.CaddyContainerSource = src
		d := getCaddyPanel(t, token)["discovery"].(map[string]any)
		assert.Equal(t, src, d["source"])
		assert.Equal(t, "belune-caddy-1", d["container_name"])
	}
}

func TestProxyPanel_IdentifiesTheRunningCaddyFromLabels(t *testing.T) {
	token := caddyPanelFixture(t)
	env.Runtime.ListSystemContainers_ = []runtime.ContainerInfo{beluneCaddyContainer("id-labels")}
	env.Capabilities.Set(beluneCaddy)
	env.Reconciler.Status_ = proxy.ReconcilerStatus{AdminState: "healthy"}

	execs := 0
	env.Runtime.ExecFunc = func(context.Context, string, []string, io.Reader, io.Writer, io.Writer) (int, error) {
		execs++
		return 0, nil
	}

	c := getCaddyPanel(t, token)
	assert.Zero(t, execs, "labels carry the version; the panel must not exec for it")
	assert.Equal(t, "found", c["container"].(map[string]any)["state"])
	assert.Equal(t, "ghcr.io/belune-hq/belune-caddy:2", c["image"])
	v := c["version"].(map[string]any)
	assert.Equal(t, "2.11.7-3", v["value"])
	assert.Equal(t, "2.11.7", v["upstream"])
	assert.Equal(t, "label", v["source"])
	assert.Equal(t, "healthy", c["admin"].(map[string]any)["state"])
	assert.Equal(t, "http://localhost:2019", c["admin"].(map[string]any)["url"])

	m := c["modules"].(map[string]any)
	assert.Equal(t, true, m["known"])
	assert.Equal(t, "available", c["features"].(map[string]any)["rate_limit"].(map[string]any)["state"])
}

// The test that matters: every way of being unable to ask must come back as
// unknown. If any of them rendered as absent, an operator would be told their
// Caddy lacks a module or a version when we simply could not look.
func TestProxyPanel_CouldNotDetermineIsNotAbsent(t *testing.T) {
	token := caddyPanelFixture(t)

	t.Run("runtime unreachable", func(t *testing.T) {
		env.Runtime.ListContainersErr = errors.New("docker unreachable")
		c := getCaddyPanel(t, token)
		assert.Equal(t, "unknown", c["container"].(map[string]any)["state"], "an unreachable runtime is not a missing container")
		assert.Nil(t, c["version"].(map[string]any)["value"])
		assert.Nil(t, c["image"])
		assert.Equal(t, false, c["modules"].(map[string]any)["known"])
		assert.Equal(t, "unknown", c["features"].(map[string]any)["rate_limit"].(map[string]any)["state"],
			"modules we could not read must not become unavailable")
	})

	t.Run("container not found", func(t *testing.T) {
		env.Runtime.ListContainersErr = nil
		c := getCaddyPanel(t, token)
		assert.Equal(t, "not_found", c["container"].(map[string]any)["state"])
		assert.Nil(t, c["version"].(map[string]any)["value"])
	})

	t.Run("modules probe failed on a found container", func(t *testing.T) {
		env.Runtime.ListSystemContainers_ = []runtime.ContainerInfo{beluneCaddyContainer("id-unknown-mods")}
		env.Capabilities.Set(capability.Set{})
		c := getCaddyPanel(t, token)
		assert.Equal(t, "found", c["container"].(map[string]any)["state"])
		assert.Equal(t, false, c["modules"].(map[string]any)["known"])
		assert.Equal(t, "unknown", c["features"].(map[string]any)["rate_limit"].(map[string]any)["state"])
	})

	t.Run("a Caddy that answered and lacks the module is unavailable, not unknown", func(t *testing.T) {
		env.Capabilities.Set(stockCaddy)
		c := getCaddyPanel(t, token)
		assert.Equal(t, true, c["modules"].(map[string]any)["known"])
		assert.Equal(t, "unavailable", c["features"].(map[string]any)["rate_limit"].(map[string]any)["state"])
	})
}

func TestProxyPanel_FallsBackToExecForUnlabelledImage(t *testing.T) {
	token := caddyPanelFixture(t)
	byo := beluneCaddyContainer("id-byo")
	byo.Labels = map[string]string{}
	byo.Image = "caddy:2-alpine"
	env.Runtime.ListAllContainers_ = []runtime.ContainerInfo{byo}
	env.Config.CaddyContainerSource = config.CaddySourceEnv

	calls := 0
	env.Runtime.ExecFunc = func(_ context.Context, _ string, _ []string, _ io.Reader, stdout, _ io.Writer) (int, error) {
		calls++
		_, _ = stdout.Write([]byte("v2.10.0 h1:abc\n"))
		return 0, nil
	}

	for range 2 {
		v := getCaddyPanel(t, token)["version"].(map[string]any)
		assert.Equal(t, "2.10.0", v["value"])
		assert.Equal(t, "exec", v["source"])
	}
	assert.Equal(t, 1, calls, "the exec result is cached against the container ID")
}

func TestProxyPanel_AccessLogKeepsQuietAndBrokenApart(t *testing.T) {
	token := caddyPanelFixture(t)
	now := time.Now()

	cases := []struct {
		name string
		st   logtailer.Status
	}{
		{"missing file", logtailer.Status{Path: "/x", File: logtailer.FileMissing, State: logtailer.StateMissing}},
		{"present, no traffic yet", logtailer.Status{Path: "/x", File: logtailer.FilePresent, State: logtailer.StateNoTraffic, Since: now}},
		{"ingesting", logtailer.Status{Path: "/x", File: logtailer.FilePresent, State: logtailer.StateIngesting, LinesRead: 3, RowsInserted: 3}},
	}
	seen := map[string]bool{}
	for _, tc := range cases {
		env.AccessLog.Set(tc.st)
		al := getCaddyPanel(t, token)["access_log"].(map[string]any)
		assert.Equal(t, tc.st.State, al["state"], tc.name)
		assert.Equal(t, tc.st.File, al["file"], tc.name)
		seen[al["state"].(string)] = true
	}
	assert.Len(t, seen, 3, "missing file, no traffic and ingesting must be three distinct outcomes")
}
