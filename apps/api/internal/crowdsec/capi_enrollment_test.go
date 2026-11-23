package crowdsec

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/belune-hq/belune/internal/store/generated"
	"github.com/stretchr/testify/require"
)

type consoleProbe struct {
	*localCAPIProbe
	commands           [][]string
	output, diagnostic string
	exit               int
	health             string
	execError          error
	restarts           int
}

func (r *consoleProbe) ContainerExec(ctx context.Context, name string, cmd []string, in io.Reader, out, stderr io.Writer) (int, error) {
	if (cmd[0] == "sh" && strings.Contains(cmd[2], "console enroll")) || (cmd[0] == "timeout" && cmd[4] == "cscli") {
		r.commands = append(r.commands, append([]string(nil), cmd...))
		_, _ = io.WriteString(out, r.output)
		_, _ = io.WriteString(stderr, r.diagnostic)
		return r.exit, r.execError
	}
	return r.localCAPIProbe.ContainerExec(ctx, name, cmd, in, out, stderr)
}
func (r *consoleProbe) ContainerHealth(context.Context, string) (string, error) {
	if r.health != "" {
		return r.health, nil
	}
	return "healthy", nil
}
func (r *consoleProbe) RestartContainer(context.Context, string, int) error { r.restarts++; return nil }
func consoleFixture() (*CAPIManager, *consoleProbe, *capiMemoryStore, CAPIEnrollRequest) {
	rt := &consoleProbe{localCAPIProbe: newLocalCAPIProbe(), diagnostic: `time="now" level=info msg="Please visit the following URL to enroll your instance: https://app.crowdsec.net/enroll/test-link"`}
	store := &capiMemoryStore{}
	m := NewCAPIManager(rt, store)
	m.pollInterval = time.Millisecond
	m.runner = newFakeRunner().on("lapi status", "ok", nil)
	sharing := CAPISharing{Custom: true, Tainted: true}
	return m, rt, store, CAPIEnrollRequest{Mode: "quick", Consent: true, Sharing: &sharing}
}
func TestConsoleEnrollmentRequiresEnabledHealthyTierOneAndConsent(t *testing.T) {
	for _, test := range []string{"disabled", "invalid_credentials", "unhealthy", "unconsented", "stale_sharing", "quick_overwrite", "quick_key"} {
		t.Run(test, func(t *testing.T) {
			m, rt, _, request := consoleFixture()
			switch test {
			case "disabled":
				rt.local = ""
			case "invalid_credentials":
				rt.credentials = "login: incomplete"
			case "unhealthy":
				rt.health = "unhealthy"
			case "unconsented":
				request.Consent = false
			case "stale_sharing":
				request.Sharing = &CAPISharing{}
			case "quick_overwrite":
				request.Overwrite = true
			case "quick_key":
				request.Key = "must-not-send"
			}
			_, err := m.Enroll(context.Background(), request)
			require.Error(t, err)
			require.Empty(t, rt.commands)
		})
	}
}
func TestConsoleEnrollmentUsesExplicitSharingAndKeepsSecretsOutOfStorage(t *testing.T) {
	m, rt, store, request := consoleFixture()
	_, err := store.UpsertSetting(context.Background(), generated.UpsertSettingParams{Key: "instance_name", Value: "My server; $(must-not-execute)"})
	require.NoError(t, err)
	result, err := m.Enroll(context.Background(), request)
	require.NoError(t, err)
	require.Equal(t, "awaiting_approval", result.State)
	require.Equal(t, "https://app.crowdsec.net/enroll/test-link", result.URL)
	require.True(t, result.RestartRequired)
	require.Zero(t, rt.restarts)
	cmd := rt.commands[0]
	require.Contains(t, strings.Join(cmd, "|"), "--name|My server; $(must-not-execute)")
	require.Contains(t, strings.Join(cmd, "|"), "--disable|manual|--enable|custom|--enable|tainted|--disable|context|--quick")
	require.NotContains(t, fmt.Sprint(store.rows), "test-link")
	for range 20 {
		status, err := m.Status(context.Background())
		require.NoError(t, err)
		require.Equal(t, "awaiting_approval", status.Enrollment.State)
		require.Empty(t, status.Enrollment.URL)
	}
	require.Len(t, rt.commands, 1, "polling never checks enrollment")
	request.Mode = "key"
	request.Key = `-key;$(must-not-execute)`
	request.Overwrite = true
	rt.diagnostic = "Watcher successfully enrolled. Visit https://app.crowdsec.net to accept it."
	_, err = m.Enroll(context.Background(), request)
	require.NoError(t, err)
	cmd = rt.commands[1]
	require.Equal(t, request.Key, cmd[len(cmd)-1])
	require.Equal(t, "--", cmd[len(cmd)-2])
	require.Contains(t, cmd, "--overwrite")
	require.NotContains(t, cmd[2], request.Key)
	require.NotContains(t, fmt.Sprint(store.rows), request.Key)
	rt.exit = 1
	rt.execError = errors.New("runtime echoed " + request.Key)
	rt.diagnostic = "key=" + request.Key
	_, err = m.Enroll(context.Background(), request)
	require.Error(t, err)
	require.NotContains(t, err.Error(), request.Key)
}
func TestQuickEnrollmentURLParsesOnlyTheExpectedLineAndHost(t *testing.T) {
	for _, output := range []string{
		"https://app.crowdsec.net/enroll/elsewhere",
		"enroll your instance: https://app.crowdsec.net.evil/enroll/test",
		"enroll your instance: https://app.crowdsec.net:443/enroll/test",
		"enroll your instance: http://app.crowdsec.net/enroll/test",
		"enroll your instance: https://attacker.invalid/enroll/test",
	} {
		require.Empty(t, quickEnrollmentURL(output), output)
	}
	require.Equal(t, "https://app.crowdsec.net/enroll/test", quickEnrollmentURL("Please visit the following URL to enroll your instance: \x1b[1mhttps://app.crowdsec.net/enroll/test\x1b[0m"))
}
func TestConsoleEnrollmentAlreadyEnrolledAndUnreadableLinkAreNotSuccess(t *testing.T) {
	m, rt, _, request := consoleFixture()
	rt.diagnostic = "Instance already enrolled. You can use '--overwrite' to force enroll"
	result, err := m.Enroll(context.Background(), request)
	require.NoError(t, err)
	require.Equal(t, "already_enrolled", result.Outcome)
	require.Equal(t, "not_enrolled", result.State)
	require.Nil(t, result.RequestedAt)
	rt.diagnostic = "CLI changed its prose: https://app.crowdsec.net/enroll/do-not-guess"
	result, err = m.Enroll(context.Background(), request)
	require.NoError(t, err)
	require.Equal(t, "link_unreadable", result.Outcome)
	require.Empty(t, result.URL)
	require.Equal(t, "awaiting_approval", result.State)
}
func TestConsoleEnrollmentConfirmationIsExplicitAndAuthenticated(t *testing.T) {
	m, rt, _, request := consoleFixture()
	_, err := m.Enroll(context.Background(), request)
	require.NoError(t, err)
	rt.diagnostic = ""
	rt.output = `| Central API (CAPI) | ✅ authenticated |
| Enrolled | ❌ not enrolled |`
	result, err := m.CheckEnrollment(context.Background())
	require.NoError(t, err)
	require.Equal(t, "awaiting_approval", result.State)
	before := len(rt.commands)
	for range 20 {
		_, err = m.Status(context.Background())
		require.NoError(t, err)
	}
	require.Len(t, rt.commands, before)
	rt.output = `| Central API (CAPI) | ❌ unreachable |
| Enrolled | ✅ enrolled |`
	_, err = m.CheckEnrollment(context.Background())
	require.Error(t, err)
	record, err := m.enrollmentRecord(context.Background())
	require.NoError(t, err)
	require.Equal(t, "awaiting_approval", record.State)
	rt.output = `| Central API (CAPI) | ✅ authenticated |
| Enrolled | ✅ enrolled |`
	result, err = m.CheckEnrollment(context.Background())
	require.NoError(t, err)
	require.Equal(t, "enrolled", result.State)
	require.True(t, result.RestartRequired)
	require.NoError(t, m.Apply(context.Background(), CAPIChange{RestartConsole: true}))
	require.Equal(t, 1, rt.restarts)
	record, err = m.enrollmentRecord(context.Background())
	require.NoError(t, err)
	require.False(t, record.RestartRequired)
}
func TestConsoleActionsShareTierOneCooldownIncludingZeroExitWarnings(t *testing.T) {
	m, rt, store, request := consoleFixture()
	rt.diagnostic = "could not reach Central API: HTTP 403 Forbidden"
	rt.output = `| Central API (CAPI) | ❌ unreachable |`
	_, err := m.CheckEnrollment(context.Background())
	require.ErrorIs(t, err, ErrCAPIRateLimited)
	_, err = m.Enroll(context.Background(), request)
	require.ErrorIs(t, err, ErrCAPIRateLimited)
	check, err := m.CheckConnection(context.Background())
	require.NoError(t, err)
	require.Equal(t, "rate_limited", check.State)
	queued := false
	require.ErrorIs(t, m.Queue(context.Background(), CAPIChange{RestartConsole: true}, func([]byte) error { queued = true; return nil }), ErrCAPIRateLimited)
	require.False(t, queued, "a restart would authenticate during daemon startup")
	require.Len(t, rt.commands, 1)
	check.CheckedAt = time.Now().Add(-2 * time.Hour)
	require.NoError(t, m.saveConnectionRecord(context.Background(), check))
	rt.output = ""
	rt.diagnostic = "Please visit the following URL to enroll your instance: https://app.crowdsec.net/enroll/retry"
	_, err = m.Enroll(context.Background(), request)
	require.NoError(t, err)
	require.Len(t, rt.commands, 2)
	require.NotContains(t, fmt.Sprint(store.rows), "/enroll/retry")
}

func TestConsoleApprovalTokensDoNotLookLikeRateLimitErrors(t *testing.T) {
	m, rt, _, request := consoleFixture()
	rt.diagnostic = "Please visit the following URL to enroll your instance: https://app.crowdsec.net/enroll/token403value"
	result, err := m.Enroll(context.Background(), request)
	require.NoError(t, err)
	require.Contains(t, result.URL, "token403value")
}

func TestConsoleTimeoutRecordsUnknownOutcomeWithoutRetrying(t *testing.T) {
	m, rt, store, request := consoleFixture()
	rt.exit = 124
	result, err := m.Enroll(context.Background(), request)
	require.NoError(t, err)
	require.Equal(t, "awaiting_approval", result.State)
	require.Equal(t, "unknown", result.Outcome)
	require.Len(t, rt.commands, 1)
	require.Contains(t, store.rows[capiEnrollmentKey].Value, "timed out")
	m.checkMu.Lock()
	_, err = m.Enroll(context.Background(), request)
	m.checkMu.Unlock()
	require.ErrorIs(t, err, ErrCAPIBusy)
	require.Len(t, rt.commands, 1)
}

func TestConsoleEnrollmentDoesNotFollowANewWatcherRegistration(t *testing.T) {
	m, rt, _, request := consoleFixture()
	_, err := m.Enroll(context.Background(), request)
	require.NoError(t, err)
	rt.diagnostic = ""
	rt.output = `| Central API (CAPI) | ✅ authenticated |
| Enrolled | ✅ enrolled |`
	result, err := m.CheckEnrollment(context.Background())
	require.NoError(t, err)
	require.Equal(t, "enrolled", result.State)
	rt.credentials = "login: a-new-watcher\npassword: changed-password\nurl: https://api.crowdsec.net/\n"
	state, err := m.Status(context.Background())
	require.NoError(t, err)
	require.Equal(t, "not_enrolled", state.Enrollment.State)
	require.False(t, state.Enrollment.RestartRequired)
	require.Contains(t, state.Enrollment.Reason, "registration changed")
	before := len(rt.commands)
	require.NoError(t, m.Apply(context.Background(), CAPIChange{RestartConsole: true}))
	require.Zero(t, rt.restarts, "an old marker must not authorize a restart for a new watcher")
	require.Len(t, rt.commands, before)
	raw, err := json.Marshal(state.Enrollment)
	require.NoError(t, err)
	require.NotContains(t, string(raw), "watcher")
	require.NotContains(t, string(raw), "password")
}

func TestConsoleEnrollmentTableRejectsMissingAndAmbiguousAuthentication(t *testing.T) {
	for _, output := range []string{
		"| Enrolled | ✅ enrolled |",
		"| Central API (CAPI) | ✅ authenticated |",
		"| Central API (CAPI) | ✅ authenticated |\n| Enrolled | ✅ enrolled |\n| Enrolled | ❌ not enrolled |",
		"| Central API (CAPI) | ✅ authenticated |\n| Enrolled | maybe enrolled |",
	} {
		_, err := consoleEnrollmentResult(output)
		require.Error(t, err)
	}
}
