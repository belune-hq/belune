package crowdsec

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/belune-hq/belune/internal/runtime"
	"github.com/stretchr/testify/require"
)

type localCAPIProbe struct {
	runtime.ContainerRuntime
	base, local, credentials string
	files                    map[string]string
	readPaths                []string
	fileError                string
	pollTest                 *testing.T
	upstream                 int
	code                     int
	stderr                   string
}

func (r *localCAPIProbe) ListSystemContainers(context.Context) ([]runtime.ContainerInfo, error) {
	return []runtime.ContainerInfo{{Name: "crowdsec", Labels: map[string]string{"belune-system": SystemLabel}}}, nil
}
func (r *localCAPIProbe) ContainerHealth(context.Context, string) (string, error) {
	return "healthy", nil
}
func (r *localCAPIProbe) ContainerExec(_ context.Context, _ string, cmd []string, _ io.Reader, out, stderr io.Writer) (int, error) {
	if r.pollTest != nil {
		require.Len(r.pollTest, cmd, 5, "polled execs must be fixed local-file reads")
		require.Equal(r.pollTest, "sh", cmd[0])
		require.NotContains(r.pollTest, strings.ToLower(cmd[2]), "cscli")
	}

	if len(cmd) > 2 && cmd[0] == "cscli" && cmd[1] == "capi" {
		r.upstream++
		_, err := io.WriteString(stderr, r.stderr)
		return r.code, err
	}
	text := ""
	path := cmd[len(cmd)-1]
	r.readPaths = append(r.readPaths, path)
	if path == r.fileError {
		return 0, errors.New("local file unavailable")
	}
	switch path {
	case "/etc/crowdsec/config.yaml.local":
		text = r.local
	case "/etc/crowdsec/config.yaml":
		text = r.base
	case "/etc/crowdsec/online_api_credentials.yaml":
		text = r.credentials
	default:
		text = r.files[path]
	}
	_, err := io.WriteString(out, text)
	return 0, err
}
func newLocalCAPIProbe() *localCAPIProbe {
	return &localCAPIProbe{base: "api:\n  server:\n    listen_uri: 0.0.0.0:8080\n", local: "api:\n  server:\n    online_client:\n      credentials_path: /etc/crowdsec/online_api_credentials.yaml\n", credentials: "login: machine\npassword: never-return-this\nurl: https://api.crowdsec.net/\n"}
}
func TestCAPIStatusUsesOnlyLocalSources(t *testing.T) {
	rt := newLocalCAPIProbe()
	m := NewCAPIManager(rt, &capiMemoryStore{})
	m.runner = localOnlyRunner{t}
	rt.pollTest = t
	for range 100 {
		state, err := m.Status(context.Background())
		require.NoError(t, err)
		require.Equal(t, "enabled", state.State)
		require.True(t, state.SharingAvailable)
		require.Nil(t, state.Connection)
	}
	require.Zero(t, rt.upstream)
	rt.local = ""
	state, err := m.Status(context.Background())
	require.NoError(t, err)
	require.Equal(t, "not_registered", state.State, "retained credentials do not mean enabled")
	rt.local = newLocalCAPIProbe().local
	rt.credentials = "password: incomplete"
	state, err = m.Status(context.Background())
	require.NoError(t, err)
	require.Equal(t, "failed", state.State)
	require.Zero(t, rt.upstream)
}
func TestCAPIExplicitConnectionCheckAndRateLimitCooldown(t *testing.T) {
	rt := newLocalCAPIProbe()
	m := NewCAPIManager(rt, &capiMemoryStore{})
	checked, err := m.CheckConnection(context.Background())
	require.NoError(t, err)
	require.Equal(t, "connected", checked.State)
	require.Equal(t, 1, rt.upstream)
	rt.code = 1
	rt.stderr = "API error: Forbidden (HTTP 403), private diagnostic never-return-this"
	checked, err = m.CheckConnection(context.Background())
	require.NoError(t, err)
	require.Equal(t, "rate_limited", checked.State)
	require.Contains(t, checked.Reason, "about an hour")
	require.NotContains(t, checked.Reason, "never-return-this")
	require.Equal(t, 2, rt.upstream)
	for range 100 {
		state, err := m.Status(context.Background())
		require.NoError(t, err)
		require.Equal(t, "enabled", state.State)
		require.Equal(t, "rate_limited", state.Connection.State)
	}
	again, err := m.CheckConnection(context.Background())
	require.NoError(t, err)
	require.Equal(t, checked, again)
	require.Equal(t, 2, rt.upstream, "neither polling nor explicit clicks may reset a blocked IP's cooldown")
	// Checks become eligible again after the hour, rather than permanently stuck.
	old := checked
	old.CheckedAt = time.Now().Add(-time.Hour - time.Minute)
	require.NoError(t, m.saveConnectionRecord(context.Background(), old))
	rt.code = 0
	rt.stderr = ""
	checked, err = m.CheckConnection(context.Background())
	require.NoError(t, err)
	require.Equal(t, "connected", checked.State)
	require.Equal(t, 3, rt.upstream)
}

func TestCAPIRegistrationRateLimitIsActionable(t *testing.T) {
	rt := newLocalCAPIProbe()
	rt.code = 1
	rt.stderr = "api register http 403 Forbidden password=private-secret"
	m := NewCAPIManager(rt, &capiMemoryStore{})
	_, err := m.exec(context.Background(), "crowdsec", []string{"cscli", "capi", "register"}, "")
	require.ErrorIs(t, err, ErrCAPIRateLimited)
	require.Contains(t, err.Error(), "about an hour")
	require.NotContains(t, err.Error(), "private-secret")
}

func TestCAPIUnavailableCheckReplacesPreviousSuccessWithoutAuthenticating(t *testing.T) {
	rt := newLocalCAPIProbe()
	m := NewCAPIManager(rt, &capiMemoryStore{})
	_, err := m.CheckConnection(context.Background())
	require.NoError(t, err)
	rt.local = ""
	checked, err := m.CheckConnection(context.Background())
	require.NoError(t, err)
	require.Equal(t, "unavailable", checked.State)
	require.False(t, checked.CheckedAt.IsZero())
	require.Equal(t, 1, rt.upstream)
	state, err := m.Status(context.Background())
	require.NoError(t, err)
	require.Equal(t, "unavailable", state.Connection.State)
	require.Equal(t, checked.CheckedAt, state.Connection.CheckedAt)
}

// Reject every runner invocation, not merely one spelling of capi status.
// Status has no reason to invoke cscli; console status can also log in upstream.
type localOnlyRunner struct{ t *testing.T }

func (r localOnlyRunner) Run(_ context.Context, args ...string) (string, error) {
	r.t.Fatalf("Status invoked an upstream-capable command: cscli %v", args)
	return "", nil
}

func TestCAPIStatusReadsEffectiveConsolePathWithoutCommandFallback(t *testing.T) {
	rt := newLocalCAPIProbe()
	custom := "/tmp/console 'custom' $(must-not-execute).yaml"
	rt.base += "    console_path: /tmp/old-console.yaml\n"
	rt.local += "    console_path: " + custom + "\n"
	rt.files = map[string]string{custom: "share_manual_decisions: true\nshare_custom: false\nshare_context: true\n", "/etc/crowdsec/console.yaml": "share_tainted: false\n"}
	m := NewCAPIManager(rt, &capiMemoryStore{})
	m.runner = localOnlyRunner{t}
	rt.pollTest = t
	state, err := m.Status(context.Background())
	require.NoError(t, err)
	require.True(t, state.SharingAvailable)
	require.Equal(t, CAPISharing{Manual: true, Tainted: true, Context: true}, state.Sharing)
	require.Contains(t, rt.readPaths, custom)
	require.NotContains(t, rt.readPaths, "/etc/crowdsec/console.yaml")
	require.NotContains(t, rt.readPaths, custom+".local")
	rt.fileError = custom
	state, err = m.Status(context.Background())
	require.NoError(t, err)
	require.False(t, state.SharingAvailable)
	rt.fileError = ""
	rt.files[custom] = "share_custom: [invalid]"
	state, err = m.Status(context.Background())
	require.NoError(t, err)
	require.False(t, state.SharingAvailable)
}
