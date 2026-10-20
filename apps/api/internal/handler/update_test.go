package handler_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"net/http"
	"os"
	"regexp"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/belune-hq/belune/internal/runtime"
	"github.com/belune-hq/belune/internal/store/generated"
	"github.com/belune-hq/belune/internal/testutil"
	"github.com/belune-hq/belune/internal/version"
)

// setUpdateSetting seeds one settings row directly (bypassing PUT /api/settings
// — update_latest_* are deliberately absent from its allowlist) and schedules
// its removal. resetDB(t)'s TruncateAll does not clear the settings table, so
// without this a value seeded here would leak into whatever test runs next.
func setUpdateSetting(t *testing.T, key, value string) {
	t.Helper()
	ctx := context.Background()
	_, err := env.Queries.UpsertSetting(ctx, generated.UpsertSettingParams{Key: key, Value: value})
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = env.Queries.UpsertSetting(context.Background(), generated.UpsertSettingParams{Key: key, Value: ""})
	})
}

// dockerLogFrames builds a stdcopy-multiplexed stream the way Docker does for a
// non-TTY container: an 8-byte header per frame (stream id, three zero bytes,
// then a big-endian length) in front of each line.
//
// ⚠️ The length bytes are routinely PRINTABLE ASCII — a 67-byte line puts a
// literal "C" in the stream — which is why "strip the control characters" looks
// like it works and does not.
func dockerLogFrames(lines ...string) string {
	var buf bytes.Buffer
	for _, l := range lines {
		payload := l + "\n"
		hdr := make([]byte, 8)
		hdr[0] = 1 // stdout
		binary.BigEndian.PutUint32(hdr[4:], uint32(len(payload)))
		buf.Write(hdr)
		buf.WriteString(payload)
	}
	return buf.String()
}

// clearUpdateAttemptSettings removes the update-attempt record TriggerSelfUpdate
// writes whenever it actually spawns a helper.
//
// ⚠️ Any test that reaches the spawn path needs this. resetDB's TruncateAll does
// not clear the settings table (see setUpdateSetting), so the stored helper id
// survives into the next test, which then sees a stale attempt and reports
// "failed" where it expected "idle". Two tests reach that path and neither is
// obviously about settings — the conflict test's own "does NOT block" subtests
// succeed on purpose.
func clearUpdateAttemptSettings(t *testing.T) {
	t.Helper()
	t.Cleanup(func() {
		for _, k := range []string{"update_helper_id", "update_helper_target", "update_helper_started_at", "update_pull_error"} {
			_, _ = env.Queries.UpsertSetting(context.Background(),
				generated.UpsertSettingParams{Key: k, Value: ""})
		}
	})
}

// waitForHelperRecorded blocks until TriggerSelfUpdate's background goroutine has
// finished its work (the helper id is the last thing it writes on success). The
// pull and spawn happen after the 202, so assertions on them must wait.
func waitForHelperRecorded(t *testing.T) {
	t.Helper()
	require.Eventually(t, func() bool {
		got, err := env.Queries.GetSetting(context.Background(), "update_helper_id")
		return err == nil && got.Value != ""
	}, 5*time.Second, 10*time.Millisecond, "the helper was never started")
}

// withVersion overrides the running build's reported version for the duration
// of the test — go test never sets the ldflags-stamped version.Version, so it
// is "dev" (an invalid semver) by default, and the "already up to date" /
// "spawns the helper" cases need a real one to compare against.
func withVersion(t *testing.T, v string) {
	t.Helper()
	old := version.Version
	version.Version = v
	t.Cleanup(func() { version.Version = old })
}

// selfContainerIDForTest mirrors handler.selfContainerID()'s unexported logic
// (platform.go) so the fixture container this test seeds into the mock runtime
// carries the SAME id the handler will independently look up at request time —
// whether that's a real container id (running in the devcontainer, where this
// test suite normally runs) or the hostname fallback (a bare `go test`).
var selfContainerIDRe = regexp.MustCompile(`/containers/([0-9a-f]{64})`)

func selfContainerIDForTest(t *testing.T) string {
	t.Helper()
	if data, err := os.ReadFile("/proc/self/mountinfo"); err == nil {
		if m := selfContainerIDRe.FindSubmatch(data); m != nil {
			return string(m[1])
		}
	}
	host, err := os.Hostname()
	require.NoError(t, err)
	return host
}

func TestTriggerSelfUpdate_RequiresPassword(t *testing.T) {
	resetDB(t)
	token := env.SetupAdmin(t, "admin@test.com", "password123")

	resp := env.DoRequest(t, "POST", "/api/maintenance/update", map[string]string{},
		testutil.AuthHeader(token))
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
	resp.Body.Close()
}

// TestTriggerSelfUpdate_RequiresSecondFactorWhenEnrolled mirrors
// TestHostShell_RequiresTheSecondFactorWhenEnrolled: this ends with the
// control-plane container replacing itself, at least as privileged as the host
// shell, so the same step-up shape (password alone is not enough once a second
// factor is enrolled) applies.
func TestTriggerSelfUpdate_RequiresSecondFactorWhenEnrolled(t *testing.T) {
	resetDB(t)
	token := env.SetupAdmin(t, "admin@test.com", "password123")
	secret, _, token := enableTOTP(t, token)

	resp := env.DoRequest(t, "POST", "/api/maintenance/update", map[string]string{
		"password": "password123",
	}, testutil.AuthHeader(token))
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode,
		"password-only step-up must not trigger an update for a 2FA user")
	assert.Contains(t, testutil.ReadJSON(t, resp)["error"], "verification code")

	// With the code the gate passes — it then fails further along (no update
	// cached), which is exactly how far this test is meant to reach.
	resp = env.DoRequest(t, "POST", "/api/maintenance/update", map[string]string{
		"password": "password123", "code": loginCode(t, secret),
	}, testutil.AuthHeader(token))
	assert.NotEqual(t, http.StatusUnauthorized, resp.StatusCode,
		"a correct code must get past the step-up gate")
	resp.Body.Close()
}

func TestTriggerSelfUpdate_RefusesWhenNoUpdateCached(t *testing.T) {
	resetDB(t)
	token := env.SetupAdmin(t, "admin@test.com", "password123")

	resp := env.DoRequest(t, "POST", "/api/maintenance/update", map[string]string{
		"password": "password123",
	}, testutil.AuthHeader(token))
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
	assert.Contains(t, testutil.ReadJSON(t, resp)["error"], "no update available")
}

// TestTriggerSelfUpdate_RefusesADevBuild guards the trap a naive semver
// compare would fall into: "dev" (what every `go build` without the release
// ldflags reports) is not a valid semver, and comparing it anyway would either
// panic or silently treat every release as "not newer".
func TestTriggerSelfUpdate_RefusesADevBuild(t *testing.T) {
	resetDB(t)
	token := env.SetupAdmin(t, "admin@test.com", "password123")
	setUpdateSetting(t, "update_latest_version", "9.9.9")

	resp := env.DoRequest(t, "POST", "/api/maintenance/update", map[string]string{
		"password": "password123",
	}, testutil.AuthHeader(token))
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
	assert.Contains(t, testutil.ReadJSON(t, resp)["error"], "no version to update from")
}

func TestTriggerSelfUpdate_RefusesWhenAlreadyUpToDate(t *testing.T) {
	resetDB(t)
	token := env.SetupAdmin(t, "admin@test.com", "password123")
	withVersion(t, "v1.2.3")
	setUpdateSetting(t, "update_latest_version", "1.2.3")

	resp := env.DoRequest(t, "POST", "/api/maintenance/update", map[string]string{
		"password": "password123",
	}, testutil.AuthHeader(token))
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
	assert.Contains(t, testutil.ReadJSON(t, resp)["error"], "already up to date")
}

func TestTriggerSelfUpdate_RefusesWhenRequiresHostUpdate(t *testing.T) {
	resetDB(t)
	token := env.SetupAdmin(t, "admin@test.com", "password123")
	withVersion(t, "v1.2.3")
	setUpdateSetting(t, "update_latest_version", "1.3.0")
	setUpdateSetting(t, "update_latest_requires_host_update", "true")

	resp := env.DoRequest(t, "POST", "/api/maintenance/update", map[string]string{
		"password": "password123",
	}, testutil.AuthHeader(token))
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
	assert.Contains(t, testutil.ReadJSON(t, resp)["error"], "run scripts/update.sh on the host")
}

// TestTriggerSelfUpdate_SpawnsHelperWithResolvedTarget is the happy path: past
// every gate, it resolves this container's own image and compose working
// directory and hands scripts/update.sh exactly the cached target version —
// never re-resolving "latest" itself.
func TestTriggerSelfUpdate_SpawnsHelperWithResolvedTarget(t *testing.T) {
	resetDB(t)
	token := env.SetupAdmin(t, "admin@test.com", "password123")
	withVersion(t, "v1.2.3")
	setUpdateSetting(t, "update_latest_version", "1.3.0")
	setUpdateSetting(t, "update_latest_requires_host_update", "false")
	clearUpdateAttemptSettings(t)
	env.Runtime.PullCalls = nil

	env.Runtime.ListAllContainers_ = []runtime.ContainerInfo{
		{
			ID:    selfContainerIDForTest(t),
			Image: "ghcr.io/weiliang79/belune:v1.2.3",
			Labels: map[string]string{
				"com.docker.compose.project.working_dir": "/opt/belune",
			},
		},
	}
	t.Cleanup(func() { env.Runtime.ListAllContainers_ = nil })

	resp := env.DoRequest(t, "POST", "/api/maintenance/update", map[string]string{
		"password": "password123",
	}, testutil.AuthHeader(token))
	body := testutil.ReadJSON(t, resp)
	require.Equal(t, http.StatusAccepted, resp.StatusCode, "%v", body)
	assert.Equal(t, "1.3.0", body["target"])

	waitForHelperRecorded(t)
	require.Len(t, env.Runtime.SpawnUpdateHelperCalls, 1)
	call := env.Runtime.SpawnUpdateHelperCalls[0]
	assert.Equal(t, "1.3.0", call.Version)
	assert.Equal(t, "/opt/belune", call.WorkingDir)
	// The TARGET image, v-stripped like update.sh builds it — not the running
	// container's ("…:v1.2.3" above), which would run the OLD updater.
	assert.Equal(t, "ghcr.io/weiliang79/belune:1.3.0", call.TargetImage)
	// …and it was pulled first: container creation fails on an absent image.
	assert.Equal(t, []string{"ghcr.io/weiliang79/belune:1.3.0"}, env.Runtime.PullCalls)

	// The attempt is recorded, which is what lets GetSelfUpdateStatus report on
	// a helper that dies immediately instead of the dashboard claiming forever
	// that an update is under way.
	//
	// ⚠️ Cleaned up here because resetDB's TruncateAll does not clear settings
	// (see setUpdateSetting): without this the stored helper id leaks into the
	// next test, which then sees a stale attempt and reports "failed" where it
	// expected "idle". Found by running the suite, not this test alone.
	ctx := context.Background()
	for _, k := range []string{"update_helper_id", "update_helper_target", "update_helper_started_at"} {
		got, err := env.Queries.GetSetting(ctx, k)
		require.NoError(t, err, "%s must be recorded", k)
		assert.NotEmpty(t, got.Value, "%s must be recorded", k)
	}
}

// updateTestSelf seeds the container that stands in for this one.
func updateTestSelf(t *testing.T) {
	t.Helper()
	env.Runtime.ListAllContainers_ = []runtime.ContainerInfo{{
		ID:     selfContainerIDForTest(t),
		Image:  "ghcr.io/weiliang79/belune:v1.2.3",
		Labels: map[string]string{"com.docker.compose.project.working_dir": "/opt/belune"},
	}}
	env.Runtime.PullCalls = nil
	env.Runtime.SpawnUpdateHelperCalls = nil
	t.Cleanup(func() {
		env.Runtime.ListAllContainers_ = nil
		env.Runtime.PullErr = nil
		env.Runtime.PullFunc = nil
		env.Runtime.SpawnUpdateHelperCalls = nil
	})
}

func getUpdateStatus(t *testing.T, token string) map[string]any {
	t.Helper()
	resp := env.DoRequest(t, "GET", "/api/maintenance/update/status", nil, testutil.AuthHeader(token))
	require.Equal(t, http.StatusOK, resp.StatusCode)
	return testutil.ReadJSON(t, resp)
}

// TestTriggerSelfUpdate_PullOutlivesTheRequest is the regression for the bug
// that would have shipped: this route sits inside the 15 s withTimeout group,
// which cancels the request context, so a pull on r.Context() is cut off at 15 s
// and NO update ever starts — only on hosts/releases where the pull is slow, so a
// fast dev pull hides it. A mock that returns instantly cannot see this.
//
// Rather than sleeping past 15 s, this holds the pull open until AFTER the
// response has been written — by which point both the request and the
// TimeoutHandler's contexts are cancelled — and proves the pull's own context is
// not, then lets it finish and expects the helper to start.
func TestTriggerSelfUpdate_PullOutlivesTheRequest(t *testing.T) {
	resetDB(t)
	token := env.SetupAdmin(t, "admin@test.com", "password123")
	withVersion(t, "v1.2.3")
	setUpdateSetting(t, "update_latest_version", "1.3.0")
	setUpdateSetting(t, "update_latest_requires_host_update", "false")
	clearUpdateAttemptSettings(t)
	updateTestSelf(t)

	var mu sync.Mutex
	var ctxErrAfterResponse error
	pullStarted, release := make(chan struct{}), make(chan struct{})
	env.Runtime.PullFunc = func(ctx context.Context, _ string) error {
		close(pullStarted)
		<-release
		mu.Lock()
		ctxErrAfterResponse = ctx.Err()
		mu.Unlock()
		return nil
	}

	resp := env.DoRequest(t, "POST", "/api/maintenance/update", map[string]string{
		"password": "password123",
	}, testutil.AuthHeader(token))
	body := testutil.ReadJSON(t, resp)
	require.Equal(t, http.StatusAccepted, resp.StatusCode, "%v", body)
	<-pullStarted

	// While it pulls: no helper yet, a second click is refused, and the status
	// says running rather than idle.
	assert.Empty(t, env.Runtime.SpawnUpdateHelperCalls)
	st := getUpdateStatus(t, token)
	assert.Equal(t, "running", st["state"], "%v", st)
	assert.Equal(t, "1.3.0", st["target"])
	again := env.DoRequest(t, "POST", "/api/maintenance/update", map[string]string{
		"password": "password123",
	}, testutil.AuthHeader(token))
	testutil.ReadJSON(t, again)
	assert.Equal(t, http.StatusConflict, again.StatusCode, "the guard must hold for the whole pull, not just the handler")

	close(release)
	waitForHelperRecorded(t)
	mu.Lock()
	defer mu.Unlock()
	assert.NoError(t, ctxErrAfterResponse, "the pull's context must not be cancelled when the request ends")
	require.Len(t, env.Runtime.SpawnUpdateHelperCalls, 1)
}

// TestTriggerSelfUpdate_FailedPullIsReportedAndSpawnsNothing: a target that
// cannot be pulled (typo'd or unpublished tag, registry down) must never start a
// helper, and the operator must be told why — through the status endpoint, since
// the 202 has long gone by the time the pull fails.
func TestTriggerSelfUpdate_FailedPullIsReportedAndSpawnsNothing(t *testing.T) {
	resetDB(t)
	token := env.SetupAdmin(t, "admin@test.com", "password123")
	withVersion(t, "v1.2.3")
	setUpdateSetting(t, "update_latest_version", "1.3.0")
	setUpdateSetting(t, "update_latest_requires_host_update", "false")
	clearUpdateAttemptSettings(t)
	updateTestSelf(t)
	env.Runtime.PullErr = errors.New("toomanyrequests: rate limit exceeded token=abc123")

	resp := env.DoRequest(t, "POST", "/api/maintenance/update", map[string]string{
		"password": "password123",
	}, testutil.AuthHeader(token))
	testutil.ReadJSON(t, resp)
	require.Equal(t, http.StatusAccepted, resp.StatusCode)

	var st map[string]any
	require.Eventually(t, func() bool {
		st = getUpdateStatus(t, token)
		return st["state"] == "failed"
	}, 5*time.Second, 10*time.Millisecond, "a failed pull must surface as a failed status: %v", st)
	assert.Contains(t, st["reason"], "ghcr.io/weiliang79/belune:1.3.0")
	assert.Contains(t, st["reason"], "Nothing has changed")
	// The real cause, not a guess — and redacted.
	assert.Contains(t, st["reason"], "rate limit exceeded")
	assert.NotContains(t, st["reason"], "abc123")
	assert.NotContains(t, st["reason"], "does that version exist")
	assert.Empty(t, env.Runtime.SpawnUpdateHelperCalls, "nothing may be spawned after a failed pull")

	// Not wedged: the guard is released, and a retry clears the old failure.
	env.Runtime.PullErr = nil
	resp = env.DoRequest(t, "POST", "/api/maintenance/update", map[string]string{
		"password": "password123",
	}, testutil.AuthHeader(token))
	testutil.ReadJSON(t, resp)
	require.Equal(t, http.StatusAccepted, resp.StatusCode)
	waitForHelperRecorded(t)
	assert.Len(t, env.Runtime.SpawnUpdateHelperCalls, 1)
}

// TestTriggerSelfUpdate_RefusesWhenAnUpdateIsAlreadyRunning covers the one gate
// that is not about the target version: a helper already at work.
//
// Nothing else stops a second run. The helper is created with an empty name so
// Docker never reports a conflict, and every other gate passes identically on a
// second click — the cached target has not moved and this container has not been
// replaced yet. Two update.sh processes inside the same window compute the same
// CURRENT_VERSION and so the same .env.backup-<v>/.infra-backup-<v> filenames,
// and the second clobbers the first: the files update.sh tells the operator are
// their way back.
//
// The subtests are the whole point. Matching on LabelHelper would have been the
// obvious implementation and is wrong — it would let a running volume restore
// block an update — and an exited helper must not lock the operator out of
// retrying after a failed update.
func TestTriggerSelfUpdate_RefusesWhenAnUpdateIsAlreadyRunning(t *testing.T) {
	self := runtime.ContainerInfo{
		ID:     selfContainerIDForTest(t),
		Image:  "ghcr.io/weiliang79/belune:v1.2.3",
		Labels: map[string]string{"com.docker.compose.project.working_dir": "/opt/belune"},
	}
	helper := func(status string, labels map[string]string) runtime.ContainerInfo {
		return runtime.ContainerInfo{ID: "helper123", Status: status, Labels: labels}
	}
	updateLabels := map[string]string{runtime.LabelHelper: "true", runtime.LabelUpdateHelper: "true"}
	otherLabels := map[string]string{runtime.LabelHelper: "true"}

	for _, tc := range []struct {
		name    string
		extra   runtime.ContainerInfo
		want    int
		spawned int
	}{
		{"a running update helper blocks", helper("running", updateLabels), http.StatusConflict, 0},
		{"a created update helper blocks, it is about to run", helper("created", updateLabels), http.StatusConflict, 0},
		{"an exited update helper does NOT block — a failed update must stay retryable", helper("exited", updateLabels), http.StatusAccepted, 1},
		{"a running backup/restore helper does NOT block — different job entirely", helper("running", otherLabels), http.StatusAccepted, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resetDB(t)
			token := env.SetupAdmin(t, "admin@test.com", "password123")
			withVersion(t, "v1.2.3")
			setUpdateSetting(t, "update_latest_version", "1.3.0")
			setUpdateSetting(t, "update_latest_requires_host_update", "false")
			clearUpdateAttemptSettings(t)

			env.Runtime.ListAllContainers_ = []runtime.ContainerInfo{self, tc.extra}
			env.Runtime.SpawnUpdateHelperCalls = nil
			t.Cleanup(func() {
				env.Runtime.ListAllContainers_ = nil
				env.Runtime.SpawnUpdateHelperCalls = nil
			})

			resp := env.DoRequest(t, "POST", "/api/maintenance/update", map[string]string{
				"password": "password123",
			}, testutil.AuthHeader(token))
			body := testutil.ReadJSON(t, resp)
			require.Equal(t, tc.want, resp.StatusCode, "%v", body)

			// The status alone is not the guarantee — no helper may be spawned.
			// A 202 spawns after the response, so wait for it; a 409 must not.
			if tc.spawned > 0 {
				waitForHelperRecorded(t)
			}
			assert.Len(t, env.Runtime.SpawnUpdateHelperCalls, tc.spawned)
		})
	}
}

// TestGetSelfUpdateStatus covers the gap that made a failed update invisible:
// TriggerSelfUpdate answers 202 the moment the helper container is CREATED and
// never waits to see whether the script survived, so a helper dying on its
// first line left the dashboard saying "started" and then showing nothing.
//
// ⚠️ The "landed" case is the one that makes this non-trivial. A SUCCESSFUL
// update also leaves an exited helper behind, so "exited" cannot mean failure
// on its own — the version has to decide. A successful update replaces this
// container, so the process answering is the new build, and its version being
// at or past the target is what proves the update worked.
func TestGetSelfUpdateStatus(t *testing.T) {
	const path = "/api/maintenance/update/status"

	t.Run("idle when nothing has been attempted", func(t *testing.T) {
		resetDB(t)
		token := env.SetupAdmin(t, "admin@test.com", "password123")
		resp := env.DoRequest(t, "GET", path, nil, testutil.AuthHeader(token))
		require.Equal(t, http.StatusOK, resp.StatusCode)
		assert.Equal(t, "idle", testutil.ReadJSON(t, resp)["state"])
	})

	t.Run("running while the helper is alive", func(t *testing.T) {
		resetDB(t)
		token := env.SetupAdmin(t, "admin@test.com", "password123")
		withVersion(t, "v1.2.3")
		setUpdateSetting(t, "update_helper_id", "helper123")
		setUpdateSetting(t, "update_helper_target", "1.3.0")

		env.Runtime.ListAllContainers_ = []runtime.ContainerInfo{
			{ID: "helper123", Status: "running"},
		}
		t.Cleanup(func() { env.Runtime.ListAllContainers_ = nil })

		body := testutil.ReadJSON(t, env.DoRequest(t, "GET", path, nil, testutil.AuthHeader(token)))
		assert.Equal(t, "running", body["state"])
		assert.Equal(t, "1.3.0", body["target"])
	})

	t.Run("failed when the helper exited and the version never moved", func(t *testing.T) {
		resetDB(t)
		token := env.SetupAdmin(t, "admin@test.com", "password123")
		withVersion(t, "v1.2.3") // still the OLD version — the update did not land
		setUpdateSetting(t, "update_helper_id", "helper123")
		setUpdateSetting(t, "update_helper_target", "1.3.0")

		env.Runtime.ListAllContainers_ = []runtime.ContainerInfo{
			{ID: "helper123", Status: "exited"},
		}
		// ⚠️ Framed the way the real daemon frames it, not as a clean string.
		// A plain string passes whatever parsing you write; the live stream has
		// TWO layers on top, and an earlier version of this handler mangled
		// both — the reason reached the UI as
		// "C2026-…Z   [info]  Updating · R2026-…Z bash: scripts/…".
		env.Runtime.ContainerLogsTail_ = dockerLogFrames(
			"2026-09-18T03:56:18.926926951Z   [info]  Updating v1.2.3 -> v1.3.0",
			"2026-09-18T03:56:18.928660353Z bash: scripts/update.sh: No such file or directory",
		)
		t.Cleanup(func() {
			env.Runtime.ListAllContainers_ = nil
			env.Runtime.ContainerLogsTail_ = ""
		})

		body := testutil.ReadJSON(t, env.DoRequest(t, "GET", path, nil, testutil.AuthHeader(token)))
		assert.Equal(t, "failed", body["state"])
		// The helper's own output is the only place the cause appears — quoting
		// it is the whole point, so a generic "it failed" is not enough here.
		reason, _ := body["reason"].(string)
		assert.Contains(t, reason, "bash: scripts/update.sh: No such file or directory")
		// ...and it must be READABLE. Both framing layers stripped: no stdcopy
		// length byte surviving as a stray letter, no timestamp prefix left in.
		assert.NotContains(t, reason, "2026-09-18T03:56",
			"the RFC3339Nano prefix ContainerLogsTail asks for must be stripped")
		assert.Regexp(t, `^\[info\]`, reason,
			"must start at the message, with no stdcopy header residue in front")
	})

	t.Run("⚠️ NOT failed when the update landed, though the helper also exited", func(t *testing.T) {
		resetDB(t)
		token := env.SetupAdmin(t, "admin@test.com", "password123")
		withVersion(t, "v1.3.0") // the new build IS the target — it worked
		setUpdateSetting(t, "update_helper_id", "helper123")
		setUpdateSetting(t, "update_helper_target", "1.3.0")

		env.Runtime.ListAllContainers_ = []runtime.ContainerInfo{
			{ID: "helper123", Status: "exited"},
		}
		t.Cleanup(func() { env.Runtime.ListAllContainers_ = nil })

		body := testutil.ReadJSON(t, env.DoRequest(t, "GET", path, nil, testutil.AuthHeader(token)))
		assert.Equal(t, "idle", body["state"],
			"an exited helper after a SUCCESSFUL update must not be reported as a failure")

		// The record is cleared, so a later unrelated failure cannot inherit it.
		body = testutil.ReadJSON(t, env.DoRequest(t, "GET", path, nil, testutil.AuthHeader(token)))
		assert.Equal(t, "idle", body["state"])
	})

	t.Run("failed when the helper is gone entirely", func(t *testing.T) {
		resetDB(t)
		token := env.SetupAdmin(t, "admin@test.com", "password123")
		withVersion(t, "v1.2.3")
		setUpdateSetting(t, "update_helper_id", "reaped-long-ago")
		setUpdateSetting(t, "update_helper_target", "1.3.0")

		env.Runtime.ListAllContainers_ = []runtime.ContainerInfo{}
		t.Cleanup(func() { env.Runtime.ListAllContainers_ = nil })

		body := testutil.ReadJSON(t, env.DoRequest(t, "GET", path, nil, testutil.AuthHeader(token)))
		assert.Equal(t, "failed", body["state"])
	})
}

// TestTriggerUpdateCheck covers the on-demand manifest check.
//
// ⚠️ It exists because "@every 24h" first activates after a FULL interval, so
// without it a fresh install reports nothing about updates for its first day,
// and flipping update_check_enabled on appears to do nothing for just as long —
// neither distinguishable from the feature being broken.
func TestTriggerUpdateCheck(t *testing.T) {
	const path = "/api/maintenance/update/check"

	t.Run("queues the check", func(t *testing.T) {
		resetDB(t)
		token := env.SetupAdmin(t, "admin@test.com", "password123")

		resp := env.DoRequest(t, "POST", path, nil, testutil.AuthHeader(token))
		body := testutil.ReadJSON(t, resp)
		require.Equal(t, http.StatusAccepted, resp.StatusCode, "%v", body)
		assert.Equal(t, "queued", body["status"])
	})

	// The toggle promises no outbound request when off. A manual button that
	// ignored it would make that promise false, so the API refuses rather than
	// relying on the UI to disable the control.
	t.Run("refuses when checks are turned off", func(t *testing.T) {
		resetDB(t)
		token := env.SetupAdmin(t, "admin@test.com", "password123")
		setUpdateSetting(t, "update_check_enabled", "false")

		resp := env.DoRequest(t, "POST", path, nil, testutil.AuthHeader(token))
		assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
		resp.Body.Close()
	})
}
