package crowdsec

import (
	"bytes"
	"context"
	"crypto/rand"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/belune-hq/belune/internal/pkg/redact"
	"github.com/belune-hq/belune/internal/runtime"
	dockerruntime "github.com/belune-hq/belune/internal/runtime/docker"
	"github.com/stretchr/testify/require"
)

// Only a uniquely named disposable container is visible to the manager. The
// fault replaces registration output, while file writes, fatal startup, health
// checks and recovery all go through the real Docker daemon.
type capiDrillRuntime struct {
	runtime.ContainerRuntime
	name              string
	badRegistration   bool
	sawUnhealthy      bool
	registrationError string
	badIssued         bool
	waitedForFatal    bool
	sawFatal          bool
}

func (r *capiDrillRuntime) ListSystemContainers(ctx context.Context) ([]runtime.ContainerInfo, error) {
	all, err := r.ContainerRuntime.ListSystemContainers(ctx)
	var own []runtime.ContainerInfo
	for _, c := range all {
		if c.Name == r.name {
			own = append(own, c)
		}
	}
	return own, err
}
func (r *capiDrillRuntime) ContainerExec(ctx context.Context, name string, cmd []string, in io.Reader, out, stderr io.Writer) (int, error) {
	if r.badRegistration && name == r.name && len(cmd) == 3 && strings.Contains(cmd[2], "capi register --file") {
		r.badIssued = true
		_, err := io.WriteString(out, "login: belune-deliberately-invalid\npassword: invalid-drill-password\nurl: https://api.crowdsec.net/\n")
		return 0, err
	}
	var diagnostic bytes.Buffer
	code, err := r.ContainerRuntime.ContainerExec(ctx, name, cmd, in, out, io.MultiWriter(stderr, &diagnostic))
	if code != 0 && (len(cmd) >= 3 && (strings.Contains(cmd[2], "capi register --file") || strings.Contains(cmd[2], "cscli -c")) || len(cmd) > 1 && cmd[0] == "cscli" && cmd[1] == "console") {
		r.registrationError = redact.Error(diagnostic.String())
	}
	return code, err
}

// Delay the injected restart until the real daemon hits its fatal path. Without
// this, the product can reject CAPI authentication and recover before the
// daemon exits, which verifies rollback but not recovery from a crash loop.
func (r *capiDrillRuntime) RestartContainer(ctx context.Context, name string, timeout int) error {
	if err := r.ContainerRuntime.RestartContainer(ctx, name, timeout); err != nil {
		return err
	}
	if r.badIssued && !r.waitedForFatal && name == r.name {
		r.waitedForFatal = true
		deadline := time.Now().Add(20 * time.Second)
		for time.Now().Before(deadline) {
			logs, err := r.ContainerLogsTail(ctx, name, 100)
			if err == nil {
				raw, _ := io.ReadAll(logs)
				_ = logs.Close()
				if strings.Contains(string(raw), "level=fatal") {
					r.sawFatal = true
					break
				}
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Second):
			}
		}
	}
	return nil
}

func (r *capiDrillRuntime) ContainerHealth(ctx context.Context, name string) (string, error) {
	health, err := r.ContainerRuntime.ContainerHealth(ctx, name)
	if r.badRegistration && health != "healthy" {
		r.sawUnhealthy = true
	}
	return health, err
}

func newCAPIDrill(t *testing.T) (*CAPIManager, *capiDrillRuntime, *dockerruntime.Client, string, CAPIStatus) {
	if os.Getenv("BELUNE_CAPI_INTEGRATION") != "1" {
		t.Skip("set BELUNE_CAPI_INTEGRATION=1 for a disposable real CAPI registration and rollback drill")
	}
	ctx := context.Background()
	docker, err := dockerruntime.New()
	require.NoError(t, err)
	t.Cleanup(func() { _ = docker.Close() })
	name := "belune-capi-drill-" + strings.ToLower(rand.Text())
	configVolume, dataVolume := name+"-config", name+"-data"
	for _, volume := range []string{configVolume, dataVolume} {
		require.NoError(t, docker.CreateDataVolume(ctx, volume))
		t.Cleanup(func() { require.NoError(t, docker.RemoveVolume(ctx, volume)) })
	}
	id, err := docker.CreateContainer(ctx, runtime.ContainerConfig{
		Name: name, Image: "crowdsecurity/crowdsec:v1.8.1",
		Env:                 map[string]string{"DISABLE_AGENT": "true", "DISABLE_ONLINE_API": "true"},
		Volumes:             map[string]string{configVolume: "/etc/crowdsec", dataVolume: "/var/lib/crowdsec/data"},
		Labels:              map[string]string{"belune-system": SystemLabel},
		HealthCheckCommand:  "wget -q -O /dev/null http://127.0.0.1:8080/health",
		HealthCheckInterval: 10 * time.Second, HealthCheckTimeout: 5 * time.Second, HealthCheckRetries: 5, HealthCheckStartPeriod: 20 * time.Second,
	})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, docker.RemoveContainer(ctx, id)) })
	require.NoError(t, docker.StartContainer(ctx, id))
	rt := &capiDrillRuntime{ContainerRuntime: docker, name: name}
	m := NewCAPIManager(rt, &capiMemoryStore{})
	require.NoError(t, m.waitHealthy(ctx, name))
	// An existing unrelated override must survive enable, disable and exact rollback.
	original := "# operator comment must survive rollback\nprometheus:\n  enabled: false\n"
	_, err = m.exec(ctx, name, []string{"sh", "-ec", "cat > /etc/crowdsec/config.yaml.local"}, original)
	require.NoError(t, err)
	initial, err := m.Status(ctx)
	require.NoError(t, err)
	require.Equal(t, "not_registered", initial.State)
	require.True(t, initial.SharingAvailable)
	require.Equal(t, CAPISharing{Custom: true, Tainted: true}, initial.Sharing)
	t.Log("initial local-only state healthy, actual sharing defaults confirmed")
	return m, rt, docker, name, initial
}

func TestCAPIDockerRollback(t *testing.T) {
	ctx := context.Background()
	m, rt, docker, name, _ := newCAPIDrill(t)
	enabled := true
	// Establish exact pre-apply bytes, including credentials and console preferences.
	files, err := newCAPIFiles(ctx, rt, name)
	require.NoError(t, err)
	before := map[string][]byte{}
	for _, file := range []string{"config.yaml.local", "online_api_credentials.yaml", "console.yaml"} {
		before[file], err = files.read(ctx, file)
		require.NoError(t, err)
	}
	files.close(ctx)
	// Cancellation of the originating request must not prevent rollback.
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	rt.badRegistration = true
	t.Log("deliberate invalid-credentials startup begins on the disposable container")
	require.NoError(t, m.Apply(cancelled, CAPIChange{Enabled: &enabled}))
	require.True(t, rt.sawUnhealthy, "real daemon must leave healthy state")
	state, err := m.Status(ctx)
	require.NoError(t, err)
	t.Log("recovery result:", state.Reason)
	require.Equal(t, "failed_reverted", state.State, state.Reason)
	require.Equal(t, "healthy", state.Health)
	require.Contains(t, state.Reason, "reverted")
	require.NotContains(t, state.Reason, "invalid-drill-password")
	files, err = newCAPIFiles(ctx, rt, name)
	require.NoError(t, err)
	defer files.close(ctx)
	for file, expected := range before {
		actual, err := files.read(ctx, file)
		require.NoError(t, err)
		require.Equal(t, string(expected), string(actual), file)
	}
	journal, err := files.hasJournal(ctx)
	require.NoError(t, err)
	require.False(t, journal)
	_, err = m.runner.Run(ctx, "lapi", "status")
	require.NoError(t, err)
	logs, err := docker.ContainerLogsTail(ctx, name, 300)
	require.NoError(t, err)
	defer func() { _ = logs.Close() }()
	raw, err := io.ReadAll(logs)
	require.NoError(t, err)
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.Contains(line, "level=fatal") || strings.Contains(line, "CAPI") || strings.Contains(line, "Central API") {
			t.Log("daemon startup evidence:", redact.Error(line))
		}
	}
	require.True(t, rt.sawFatal, "invalid credentials must reach the real daemon fatal startup path")
	_, err = m.exec(ctx, name, []string{"sh", "-ec", `key=$(cscli bouncers add belune-capi-after-revert -o raw); wget -q -O /dev/null --header "X-Api-Key: $key" 'http://127.0.0.1:8080/v1/decisions/stream?startup=true'`}, "")
	require.NoError(t, err)
	t.Log("fatal startup self-reverted; exact previous files, LAPI and bouncer authentication verified with a cancelled parent context")
}

func TestCAPIDockerLifecycle(t *testing.T) {
	if os.Getenv("BELUNE_CAPI_REGISTER") != "1" {
		t.Skip("set BELUNE_CAPI_REGISTER=1 to opt into real anonymous registration with CrowdSec")
	}
	ctx := context.Background()
	m, rt, _, name, initial := newCAPIDrill(t)
	enabled, disabled := true, false
	require.NoError(t, m.Apply(ctx, CAPIChange{Enabled: &enabled, Sharing: &initial.Sharing}))
	state, err := m.Status(ctx)
	require.NoError(t, err)
	require.Equal(t, "enabled", state.State, state.Reason+" "+rt.registrationError)
	require.Equal(t, "healthy", state.Health)
	t.Log("anonymous registration and CAPI authentication succeeded")
	preferences := CAPISharing{Manual: true, Context: true}
	require.NoError(t, m.Apply(ctx, CAPIChange{Sharing: &preferences}))
	changed, err := m.Status(ctx)
	require.NoError(t, err)
	require.Empty(t, changed.Reason, rt.registrationError)
	require.Equal(t, preferences, changed.Sharing)
	require.Equal(t, "enabled", changed.State)
	require.NoError(t, m.Apply(ctx, CAPIChange{Sharing: &initial.Sharing}))
	t.Log("all four sharing switches apply after registration")

	require.NoError(t, m.restartAndVerify(ctx, name))
	_, err = m.runner.Run(ctx, "capi", "status")
	require.NoError(t, err)
	t.Log("online_client override survives restart with DISABLE_ONLINE_API=true")
	require.Eventually(t, func() bool {
		raw, err := m.runner.Run(ctx, "decisions", "list", "--origin", "CAPI", "-o", "json")
		return err == nil && len(strings.TrimSpace(raw)) > 2 && strings.TrimSpace(raw) != "null"
	}, 4*time.Minute, 5*time.Second, "community decisions should arrive from CAPI")
	t.Log("community decisions downloaded")
	require.NoError(t, m.Apply(ctx, CAPIChange{Enabled: &disabled}))
	state, err = m.Status(ctx)
	require.NoError(t, err)
	require.Equal(t, "not_registered", state.State, state.Reason)
	require.Equal(t, "healthy", state.Health)
	// Use a real bouncer token without sending it to test logs.
	_, err = m.exec(ctx, name, []string{"sh", "-ec", `key=$(cscli bouncers add belune-capi-drill -o raw); wget -q -O /dev/null --header "X-Api-Key: $key" 'http://127.0.0.1:8080/v1/decisions/stream?startup=true'`}, "")
	require.NoError(t, err)
	t.Log("disable verified; local API and real bouncer authenticate")

}
