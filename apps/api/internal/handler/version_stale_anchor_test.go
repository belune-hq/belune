package handler_test

import (
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/belune-hq/belune/internal/runtime"
	"github.com/belune-hq/belune/internal/testutil"
)

// A FAILED dashboard attempt leaves updateBeganAt set (nothing clears it). Does
// that stale anchor inflate a LATER host-run update's elapsed?
func TestGetVersion_FailedAttemptDoesNotInflateALaterHostRunUpdate(t *testing.T) {
	resetDB(t)
	token := env.SetupAdmin(t, "admin@test.com", "password123")
	withVersion(t, "v1.2.3")
	setUpdateSetting(t, "update_latest_version", "1.3.0")
	setUpdateSetting(t, "update_latest_requires_host_update", "false")
	clearUpdateAttemptSettings(t)
	updateTestSelf(t)

	// 1. A dashboard attempt that fails at the pull. Nothing was touched.
	env.Runtime.PullErr = errors.New("manifest unknown")
	resp := env.DoRequest(t, "POST", "/api/maintenance/update",
		map[string]string{"password": "password123"}, testutil.AuthHeader(token))
	testutil.ReadJSON(t, resp)
	require.Equal(t, http.StatusAccepted, resp.StatusCode)
	require.Eventually(t, func() bool {
		return getUpdateStatus(t, token)["state"] == "failed"
	}, 5*time.Second, 10*time.Millisecond)
	env.Runtime.PullErr = nil

	// 2. Time passes, then someone SSHes in and runs scripts/update.sh. That
	//    helper was created JUST NOW — its update is seconds old, not minutes.
	time.Sleep(1100 * time.Millisecond)
	env.Runtime.ListAllContainers_ = append(env.Runtime.ListAllContainers_,
		runtime.ContainerInfo{ID: "hostrun", Status: "running", CreatedAt: time.Now(),
			Labels: map[string]string{runtime.LabelHelper: "true", runtime.LabelUpdateHelper: "true"}})

	body := getPublicVersion(t)
	t.Logf("host-run update, seconds old: updating=%v updating_for=%v", body["updating"], body["updating_for"])
	elapsed, _ := body["updating_for"].(float64)
	assert.Less(t, elapsed, 1.0,
		"elapsed must come from THIS update, not from a failed attempt 1.5s earlier")
}
