package handler_test

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/belune-hq/belune/internal/runtime"
	"github.com/belune-hq/belune/internal/testutil"
)

// getPublicVersion reads /api/version with NO credentials: the flag is public
// by design, so a test that sent a token would not prove that.
func getPublicVersion(t *testing.T) map[string]any {
	t.Helper()
	resp := env.DoRequest(t, "GET", "/api/version", nil, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	return testutil.ReadJSON(t, resp)
}

func TestGetVersion_UpdatingFlag(t *testing.T) {
	updateLabels := map[string]string{runtime.LabelHelper: "true", runtime.LabelUpdateHelper: "true"}
	otherLabels := map[string]string{runtime.LabelHelper: "true"}

	for _, tc := range []struct {
		name   string
		helper *runtime.ContainerInfo
		want   bool
	}{
		{"no helper", nil, false},
		{"a running update helper", &runtime.ContainerInfo{ID: "h", Status: "running", Labels: updateLabels, CreatedAt: time.Now().Add(-80 * time.Second)}, true},
		{"a created update helper — about to run", &runtime.ContainerInfo{ID: "h", Status: "created", Labels: updateLabels, CreatedAt: time.Now()}, true},
		// The self-clearing property: a failed update leaves an exited helper and
		// must not leave every client latched on "updating" forever.
		{"an exited update helper", &runtime.ContainerInfo{ID: "h", Status: "exited", Labels: updateLabels, CreatedAt: time.Now()}, false},
		{"a running backup helper is not an update", &runtime.ContainerInfo{ID: "h", Status: "running", Labels: otherLabels, CreatedAt: time.Now()}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resetDB(t)
			updateTestSelf(t)
			if tc.helper != nil {
				env.Runtime.ListAllContainers_ = append(env.Runtime.ListAllContainers_, *tc.helper)
			}

			body := getPublicVersion(t)
			assert.Equal(t, tc.want, body["updating"], "%v", body)
			if tc.want && tc.helper.Status == "running" {
				assert.InDelta(t, 80, body["updating_for"], 5, "elapsed comes from the helper's creation time")
			}
			if !tc.want {
				assert.EqualValues(t, 0, body["updating_for"])
			}
			assert.NotContains(t, body, "target", "the target version is not public")
		})
	}
}

// The pull window is the long, silent one on a cold host: the 202 has been sent,
// no helper exists, and a flag derived only from containers would read false for
// all of it.
func TestGetVersion_UpdatingDuringThePullWindow(t *testing.T) {
	resetDB(t)
	token := env.SetupAdmin(t, "admin@test.com", "password123")
	withVersion(t, "v1.2.3")
	setUpdateSetting(t, "update_latest_version", "1.3.0")
	setUpdateSetting(t, "update_latest_requires_host_update", "false")
	clearUpdateAttemptSettings(t)
	updateTestSelf(t)

	pullStarted, release := make(chan struct{}), make(chan struct{})
	env.Runtime.PullFunc = func(context.Context, string) error {
		close(pullStarted)
		<-release
		return nil
	}

	assert.Equal(t, false, getPublicVersion(t)["updating"], "idle before the click")

	resp := env.DoRequest(t, "POST", "/api/maintenance/update", map[string]string{
		"password": "password123",
	}, testutil.AuthHeader(token))
	testutil.ReadJSON(t, resp)
	require.Equal(t, http.StatusAccepted, resp.StatusCode)
	<-pullStarted

	assert.Empty(t, env.Runtime.SpawnUpdateHelperCalls, "precondition: still pulling, no helper")
	assert.Equal(t, true, getPublicVersion(t)["updating"], "must be true while only the pull is running")

	close(release)
	waitForHelperRecorded(t)
}

func TestGetVersion_FailedPullClearsTheFlag(t *testing.T) {
	resetDB(t)
	token := env.SetupAdmin(t, "admin@test.com", "password123")
	withVersion(t, "v1.2.3")
	setUpdateSetting(t, "update_latest_version", "1.3.0")
	setUpdateSetting(t, "update_latest_requires_host_update", "false")
	clearUpdateAttemptSettings(t)
	updateTestSelf(t)
	env.Runtime.PullErr = assert.AnError

	resp := env.DoRequest(t, "POST", "/api/maintenance/update", map[string]string{
		"password": "password123",
	}, testutil.AuthHeader(token))
	testutil.ReadJSON(t, resp)
	require.Equal(t, http.StatusAccepted, resp.StatusCode)

	require.Eventually(t, func() bool {
		st := getUpdateStatus(t, token)
		return st["state"] == "failed"
	}, 5*time.Second, 10*time.Millisecond)
	assert.Equal(t, false, getPublicVersion(t)["updating"], "a failed update must not leave clients latched")
}
