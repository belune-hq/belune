package crowdsec

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/belune-hq/belune/internal/pkg/redact"
	containerRuntime "github.com/belune-hq/belune/internal/runtime"
	"github.com/belune-hq/belune/internal/store/generated"
	"github.com/stretchr/testify/require"
)

// Only cscli gets the fixture CA; file reads, CLI execution, health and restarts
// remain real Docker operations. The container has no external network at all.
type consoleDrillRuntime struct {
	containerRuntime.ContainerRuntime
	t *testing.T
}

func (r *consoleDrillRuntime) ContainerExec(ctx context.Context, name string, cmd []string, in io.Reader, out, stderr io.Writer) (int, error) {
	if strings.Contains(strings.Join(cmd, " "), "cscli") {
		cmd = append([]string{"env", "SSL_CERT_FILE=/tmp/belune-console-ca.pem"}, cmd...)
	}
	if strings.Contains(strings.Join(cmd, " "), "console status") {
		var diagnostic bytes.Buffer
		code, err := r.ContainerRuntime.ContainerExec(ctx, name, cmd, in, out, io.MultiWriter(stderr, &diagnostic))
		r.t.Logf("offline console diagnostics: %s", redact.Error(diagnostic.String()))
		return code, err
	}
	return r.ContainerRuntime.ContainerExec(ctx, name, cmd, in, out, stderr)
}
func TestConsoleDockerEnrollmentProtocol(t *testing.T) {
	if os.Getenv("BELUNE_CONSOLE_INTEGRATION") != "1" {
		t.Skip("set BELUNE_CONSOLE_INTEGRATION=1 for the offline real-cscli enrollment drill")
	}
	t.Setenv("BELUNE_CAPI_INTEGRATION", "1")
	ctx := context.Background()
	m, rt, docker, name, _ := newCAPIDrill(t, true)
	_, err := m.Enroll(ctx, CAPIEnrollRequest{Mode: "quick", Consent: true, Sharing: &CAPISharing{Custom: true, Tainted: true}})
	require.ErrorContains(t, err, "enable Central API")
	binary := filepath.Join(t.TempDir(), "console-api")
	build := exec.CommandContext(ctx, "go", "build", "-o", binary, "./testdata/console-api")
	build.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux", "GOARCH="+runtime.GOARCH)
	output, err := build.CombinedOutput()
	require.NoError(t, err, string(output))
	file, err := os.Open(binary)
	require.NoError(t, err)
	code, err := docker.ContainerExec(ctx, name, []string{"sh", "-ec", `cat > /tmp/belune-console-api; chmod 700 /tmp/belune-console-api; printf '\n127.0.0.1 api.crowdsec.net\n' >> /etc/hosts; /tmp/belune-console-api > /tmp/belune-console-fixture.log 2>&1 < /dev/null &`}, file, io.Discard, io.Discard)
	_ = file.Close()
	require.NoError(t, err)
	require.Zero(t, code)
	require.Eventually(t, func() bool {
		_, err := m.exec(ctx, name, []string{"wget", "-qO-", "http://127.0.0.1:8088/"}, "")
		return err == nil
	}, 5e9, 1e8)
	// Keep the daemon's live LAPI offline. Only its next CLI config read uses
	// these isolated, fake watcher credentials; no daemon restart is performed.
	_, err = m.exec(ctx, name, []string{"sh", "-ec", `cat > /etc/crowdsec/online_api_credentials.yaml`}, "login: offline-watcher\npassword: offline-password\nurl: https://api.crowdsec.net/\n")
	require.NoError(t, err)
	_, err = m.exec(ctx, name, []string{"sh", "-ec", `cat > /etc/crowdsec/config.yaml.local`}, "api:\n  server:\n    online_client:\n      credentials_path: /etc/crowdsec/online_api_credentials.yaml\n")
	require.NoError(t, err)
	m.rt = &consoleDrillRuntime{rt, t}
	m.runner = NewContainerRunner(m.rt)
	_, err = m.store.UpsertSetting(ctx, generated.UpsertSettingParams{Key: "instance_name", Value: "Offline enrollment drill"})
	require.NoError(t, err)
	sharing := CAPISharing{Custom: true, Tainted: true}
	request := CAPIEnrollRequest{Mode: "quick", Consent: true, Sharing: &sharing}
	result, err := m.Enroll(ctx, request)
	require.NoError(t, err)
	require.Equal(t, "https://app.crowdsec.net/enroll/offline-fixture", result.URL)
	require.Equal(t, "awaiting_approval", result.State)
	result, err = m.Enroll(ctx, request)
	require.NoError(t, err)
	require.Equal(t, "already_enrolled", result.Outcome)
	result, err = m.CheckEnrollment(ctx)
	require.NoError(t, err)
	require.Equal(t, "awaiting_approval", result.State)
	counts := func() map[string]any {
		raw, err := m.exec(ctx, name, []string{"wget", "-qO-", "http://127.0.0.1:8088/"}, "")
		require.NoError(t, err)
		var value map[string]any
		require.NoError(t, json.Unmarshal([]byte(raw), &value))
		return value
	}
	before := counts()
	for range 20 {
		state, err := m.Status(ctx)
		require.NoError(t, err)
		require.Equal(t, "awaiting_approval", state.Enrollment.State)
		require.Equal(t, sharing, state.Sharing)
	}
	require.Equal(t, before, counts(), "polling must not create any upstream protocol traffic")
	require.Equal(t, float64(2), before["enrollments"])
	require.Equal(t, "Offline enrollment drill", before["name"])
	_, err = m.exec(ctx, name, []string{"sh", "-ec", `cat > /etc/crowdsec/console.yaml`}, "share_manual_decisions: false\nshare_custom: false\nshare_tainted: false\nshare_context: false\n")
	require.NoError(t, err)
	request.Sharing = &CAPISharing{}
	request.Mode = "key"
	request.Key = "offline-key;$(touch /tmp/should-not-exist)"
	request.Overwrite = true
	result, err = m.Enroll(ctx, request)
	require.NoError(t, err)
	require.Equal(t, "awaiting_approval", result.State)
	after := counts()
	require.Equal(t, true, after["key_present"])
	require.Equal(t, true, after["overwrite"])
	require.Equal(t, false, after["auto"])
	_, err = m.exec(ctx, name, []string{"sh", "-ec", "test ! -e /tmp/should-not-exist"}, "")
	require.NoError(t, err)
	state, err := m.Status(ctx)
	require.NoError(t, err)
	require.Equal(t, CAPISharing{}, state.Sharing)
	_, err = m.exec(ctx, name, []string{"wget", "-qO-", "http://127.0.0.1:8088/rate-limit"}, "")
	require.NoError(t, err)
	_, err = m.CheckEnrollment(ctx)
	require.ErrorIs(t, err, ErrCAPIRateLimited)
	limited := counts()
	_, err = m.Enroll(ctx, request)
	require.ErrorIs(t, err, ErrCAPIRateLimited)
	_, err = m.CheckConnection(ctx)
	require.NoError(t, err)
	require.Equal(t, limited, counts(), "switching actions must not bypass a real CLI's zero-exit 403 warning")
	t.Log("real CLI: disabled refusal, quick URL, already enrolled, pending check, exact sharing, zero polling traffic and literal overwrite key and cross-action 403 cooldown verified offline")
}
