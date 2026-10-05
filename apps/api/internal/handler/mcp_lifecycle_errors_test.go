package handler_test

import (
	"errors"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/weiliang79/belune/internal/testutil"
)

// A container that will not start or stop must be reported as a failure and
// must NOT be recorded as having started or stopped: the status column is what
// the dashboard and the reconciler believe.
func TestMCP_StartStop_ContainerFailureIsReportedNotRecorded(t *testing.T) {
	resetDB(t)
	adminToken := env.SetupAdmin(t, "mcp-lifecycle-fail@test.com", "password123")
	_, appID := lifecycleApp(t, adminToken)
	token := mintScoped(t, adminToken, []string{"write"})
	before := getApp(t, appID).Status
	t.Cleanup(func() { env.Runtime.StartErr, env.Runtime.StopErr = nil, nil })

	env.Runtime.StopErr = errors.New("docker: no such container")
	msg := toolErrorText(t, callToolArgs(t, token, "stop_application", triggerArgs(appID)))
	assert.Equal(t, "failed to stop the application: the container could not be stopped. "+
		"Check list_deployments to see whether it has ever been deployed", msg)
	assert.NotContains(t, msg, "docker", "the runtime's own text must not reach the caller")
	assert.Equal(t, before, getApp(t, appID).Status)

	env.Runtime.StartErr = errors.New("docker: no such container")
	msg = toolErrorText(t, callToolArgs(t, token, "start_application", triggerArgs(appID)))
	assert.Equal(t, "failed to start the application: the container could not be started. "+
		"Check list_deployments to see whether it has ever been deployed", msg)
	assert.Equal(t, before, getApp(t, appID).Status)
}

// The REST handlers map the service's sentinel errors back onto their original
// messages; that mapping had no coverage before the mock could fail.
func TestStopStartApplication_ContainerFailureIs500(t *testing.T) {
	resetDB(t)
	adminToken := env.SetupAdmin(t, "rest-lifecycle-fail@test.com", "password123")
	projectID, appID := lifecycleApp(t, adminToken)
	t.Cleanup(func() { env.Runtime.StartErr, env.Runtime.StopErr = nil, nil })
	before := getApp(t, appID).Status

	env.Runtime.StopErr = errors.New("boom")
	resp := env.DoRequest(t, "POST", appActionPath(projectID, appID, "stop"), nil, testutil.AuthHeader(adminToken))
	assert.Equal(t, http.StatusInternalServerError, resp.StatusCode)
	assert.Equal(t, "failed to stop application", testutil.ReadJSON(t, resp)["error"])

	env.Runtime.StartErr = errors.New("boom")
	resp = env.DoRequest(t, "POST", appActionPath(projectID, appID, "start"), nil, testutil.AuthHeader(adminToken))
	assert.Equal(t, http.StatusInternalServerError, resp.StatusCode)
	assert.Equal(t, "failed to start application", testutil.ReadJSON(t, resp)["error"])

	assert.Equal(t, before, getApp(t, appID).Status, "a failed container action must not change the recorded status")
}
