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

// FINDING 1 probe: does the reported elapsed survive the pull->helper handover?
func TestGetVersion_ElapsedSurvivesTheHandover(t *testing.T) {
	resetDB(t)
	token := env.SetupAdmin(t, "admin@test.com", "password123")
	withVersion(t, "v1.2.3")
	setUpdateSetting(t, "update_latest_version", "1.3.0")
	setUpdateSetting(t, "update_latest_requires_host_update", "false")
	clearUpdateAttemptSettings(t)
	updateTestSelf(t)

	release := make(chan struct{})
	started := make(chan struct{})
	env.Runtime.PullFunc = func(_ context.Context, _ string) error {
		close(started)
		<-release
		// The real spawn happens right after the pull returns, so the helper's
		// CreatedAt is "now" — long after the update actually began.
		env.Runtime.ListAllContainers_ = append(env.Runtime.ListAllContainers_,
			runtime.ContainerInfo{ID: "h", Status: "running", CreatedAt: time.Now(),
				Labels: map[string]string{runtime.LabelHelper: "true", runtime.LabelUpdateHelper: "true"}})
		return nil
	}
	t.Cleanup(func() { env.Runtime.PullFunc = nil })

	resp := env.DoRequest(t, "POST", "/api/maintenance/update",
		map[string]string{"password": "password123"}, testutil.AuthHeader(token))
	testutil.ReadJSON(t, resp)
	require.Equal(t, http.StatusAccepted, resp.StatusCode)
	<-started

	// A slow pull: 1.5s of real update time elapses before any helper exists.
	time.Sleep(1500 * time.Millisecond)
	during := env.DoRequest(t, "GET", "/api/version", nil, nil)
	b1 := testutil.ReadJSON(t, during)
	t.Logf("DURING the pull : updating=%v updating_for=%v", b1["updating"], b1["updating_for"])

	close(release)
	waitForHelperRecorded(t)
	time.Sleep(200 * time.Millisecond)

	after := env.DoRequest(t, "GET", "/api/version", nil, nil)
	b2 := testutil.ReadJSON(t, after)
	t.Logf("AFTER  handover : updating=%v updating_for=%v", b2["updating"], b2["updating_for"])

	d1, _ := b1["updating_for"].(float64)
	d2, _ := b2["updating_for"].(float64)
	assert.GreaterOrEqual(t, d2, d1,
		"elapsed must not go BACKWARDS at the handover: a tab opening after it is told the update just started")
}
