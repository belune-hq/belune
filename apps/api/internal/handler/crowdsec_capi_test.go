package handler_test

import (
	"encoding/json"
	"errors"
	"net/http"
	"testing"

	"github.com/belune-hq/belune/internal/testutil"
	"github.com/belune-hq/belune/internal/worker"
	"github.com/stretchr/testify/require"
)

func TestCrowdSecCAPIQueuesAndAuditsWithoutRestartingInHTTP(t *testing.T) {
	f := crowdsecFixture(t)
	env.Asynq.Tasks = nil
	beforeRestarts := len(env.Runtime.RestartCalls)
	body := map[string]any{"enabled": true, "sharing": map[string]bool{"share_manual_decisions": false, "share_custom": true, "share_tainted": false, "share_context": false}}
	code, result := f.json("PUT", "/api/maintenance/proxy/crowdsec/capi", body)
	require.Equal(t, http.StatusAccepted, code)
	require.Equal(t, "queued", result["status"])
	require.Len(t, env.Asynq.Tasks, 1)
	task := env.Asynq.Tasks[0]
	require.Equal(t, worker.TypeCrowdSecCAPI, task.TypeName)
	var queued map[string]any
	require.NoError(t, json.Unmarshal(task.Payload, &queued))
	require.Equal(t, body["enabled"], queued["enabled"])
	require.Len(t, env.Runtime.RestartCalls, beforeRestarts, "HTTP must only enqueue")
	code, _ = f.json("PUT", "/api/maintenance/proxy/crowdsec/capi", body)
	require.Equal(t, http.StatusConflict, code)
	require.Len(t, env.Asynq.Tasks, 1)
	f.awaitAudit("update_crowdsec_capi")
	details := f.auditDetails("update_crowdsec_capi")
	require.Equal(t, true, details["enabled"])
	require.Equal(t, false, details["sharing"].(map[string]any)["share_tainted"])
}

func TestCrowdSecCAPIValidatesBeforeEnqueueAndRecoversFromQueueFailure(t *testing.T) {
	f := crowdsecFixture(t)
	env.Asynq.Tasks = nil
	for _, body := range []any{map[string]any{}, map[string]any{"enabled": "yes"}, map[string]any{"enrollment_key": "not-supported"}, map[string]any{"sharing": map[string]any{"share_context": "yes"}}} {
		code, _ := f.json("PUT", "/api/maintenance/proxy/crowdsec/capi", body)
		require.Equal(t, http.StatusBadRequest, code)
	}
	require.Empty(t, env.Asynq.Tasks)
	env.Asynq.EnqueueErr = errors.New("queue unavailable")
	env.Asynq.EnqueueOnce = true
	t.Cleanup(func() { env.Asynq.EnqueueErr = nil })
	code, _ := f.json("PUT", "/api/maintenance/proxy/crowdsec/capi", map[string]bool{"enabled": false})
	require.Equal(t, http.StatusInternalServerError, code)
	code, _ = f.json("PUT", "/api/maintenance/proxy/crowdsec/capi", map[string]bool{"enabled": false})
	require.Equal(t, http.StatusAccepted, code)
}

func TestCrowdSecCAPIRequiresAdminSession(t *testing.T) {
	f := crowdsecFixture(t)
	token := mintScoped(t, f.token, []string{"write"})
	original := f.token
	f.token = token
	for _, method := range []string{"GET", "PUT"} {
		code, _ := f.json(method, "/api/maintenance/proxy/crowdsec/capi", map[string]bool{"enabled": false})
		require.Equal(t, http.StatusForbidden, code)
	}
	f.token = original
	response := env.DoRequest(t, "POST", "/api/users", map[string]string{"email": "member@test.com", "password": "password123", "role": "member"}, testutil.AuthHeader(original))
	require.Equal(t, http.StatusCreated, response.StatusCode)
	_ = response.Body.Close()
	// A member session must also fail both routes.
	f.token = env.LoginAs(t, "member@test.com", "password123")
	for _, method := range []string{"GET", "PUT"} {
		code, _ := f.json(method, "/api/maintenance/proxy/crowdsec/capi", map[string]bool{"enabled": false})
		require.Equal(t, http.StatusForbidden, code)
	}
}
