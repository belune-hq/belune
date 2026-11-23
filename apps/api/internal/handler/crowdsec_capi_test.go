package handler_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/belune-hq/belune/internal/runtime"
	"io"
	"net/http"
	"strings"
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
	for _, method := range []string{"GET", "PUT", "POST"} {
		path := "/api/maintenance/proxy/crowdsec/capi"
		if method == "POST" {
			path += "/check"
		}
		code, _ := f.json(method, path, map[string]bool{"enabled": false})
		require.Equal(t, http.StatusForbidden, code)
	}
	f.token = original
	response := env.DoRequest(t, "POST", "/api/users", map[string]string{"email": "member@test.com", "password": "password123", "role": "member"}, testutil.AuthHeader(original))
	require.Equal(t, http.StatusCreated, response.StatusCode)
	_ = response.Body.Close()
	// A member session must also fail both routes.
	f.token = env.LoginAs(t, "member@test.com", "password123")
	for _, method := range []string{"GET", "PUT", "POST"} {
		path := "/api/maintenance/proxy/crowdsec/capi"
		if method == "POST" {
			path += "/check"
		}
		code, _ := f.json(method, path, map[string]bool{"enabled": false})
		require.Equal(t, http.StatusForbidden, code)
	}
}

func TestCrowdSecCAPICheckIsExplicitAuditedAndProtectsCooldown(t *testing.T) {
	f := crowdsecFixture(t)
	oldExec, oldContainers := env.Runtime.ExecFunc, env.Runtime.ListSystemContainers_
	t.Cleanup(func() { env.Runtime.ExecFunc = oldExec; env.Runtime.ListSystemContainers_ = oldContainers })
	env.Runtime.ListSystemContainers_ = []runtime.ContainerInfo{{Name: "crowdsec", Labels: map[string]string{"belune-system": "crowdsec"}}}
	upstream := 0
	env.Runtime.ExecFunc = func(_ context.Context, _ string, cmd []string, _ io.Reader, out, stderr io.Writer) (int, error) {
		if cmd[0] == "cscli" && cmd[1] == "capi" {
			upstream++
			_, err := io.WriteString(stderr, "HTTP 403 Forbidden private-secret")
			return 1, err
		}
		text := ""
		if cmd[0] == "cscli" {
			t.Fatalf("status polling invoked an upstream-capable cscli command: %v", cmd)
		} else if cmd[len(cmd)-1] == "/etc/crowdsec/online_api_credentials.yaml" {
			text = "login: test\npassword: private-secret\nurl: https://api.crowdsec.net/\n"
		} else if cmd[len(cmd)-1] == "/etc/crowdsec/config.yaml.local" {
			text = "api:\n  server:\n    online_client:\n      credentials_path: /etc/crowdsec/online_api_credentials.yaml\n"
		}
		_, err := io.WriteString(out, text)
		return 0, err
	}
	path := "/api/maintenance/proxy/crowdsec/capi"
	for range 5 {
		code, _ := f.json("GET", path, nil)
		require.Equal(t, http.StatusOK, code)
	}
	require.Zero(t, upstream)
	code, result := f.json("POST", path+"/check", nil)
	require.Equal(t, http.StatusOK, code)
	require.Equal(t, "rate_limited", result["state"])
	require.Contains(t, result["reason"], "about an hour")
	require.NotContains(t, result["reason"], "private-secret")
	require.Equal(t, 1, upstream)
	code, _ = f.json("POST", path+"/check", nil)
	require.Equal(t, http.StatusOK, code)
	require.Equal(t, 1, upstream)
	for range 5 {
		code, state := f.json("GET", path, nil)
		require.Equal(t, http.StatusOK, code)
		require.Equal(t, "rate_limited", state["connection"].(map[string]any)["state"])
	}
	require.Equal(t, 1, upstream)
	f.awaitAudit("check_crowdsec_capi", "check_crowdsec_capi")
}

func TestConsoleEnrollmentIsAdminSessionOnlyAndNeverQueuesTheKey(t *testing.T) {
	f := crowdsecFixture(t)
	oldExec, oldContainers := env.Runtime.ExecFunc, env.Runtime.ListSystemContainers_
	t.Cleanup(func() { env.Runtime.ExecFunc = oldExec; env.Runtime.ListSystemContainers_ = oldContainers })
	env.Runtime.ListSystemContainers_ = []runtime.ContainerInfo{{Name: "crowdsec", Labels: map[string]string{"belune-system": "crowdsec"}}}
	calls := 0
	env.Runtime.ExecFunc = func(_ context.Context, _ string, cmd []string, _ io.Reader, out, stderr io.Writer) (int, error) {
		text := ""
		if cmd[0] == "sh" && strings.Contains(cmd[2], "console enroll") {
			calls++
			_, err := io.WriteString(stderr, "Watcher successfully enrolled. Visit https://app.crowdsec.net to accept it.")
			return 0, err
		}
		if cmd[0] == "timeout" {
			calls++
			text = `| Central API (CAPI) | ✅ authenticated |
| Enrolled | ❌ not enrolled |`
		} else {
			switch cmd[len(cmd)-1] {
			case "/etc/crowdsec/config.yaml.local":
				text = "api:\n  server:\n    online_client:\n      credentials_path: /etc/crowdsec/online_api_credentials.yaml\n"
			case "/etc/crowdsec/online_api_credentials.yaml":
				text = "login: watcher\npassword: do-not-return\nurl: https://api.crowdsec.net/\n"
			}
		}
		_, err := io.WriteString(out, text)
		return 0, err
	}
	path := "/api/maintenance/proxy/crowdsec/capi"
	body := map[string]any{"mode": "key", "key": "one-shot-never-persist", "consent": true, "sharing": map[string]bool{"share_custom": true, "share_tainted": true}}
	original := f.token
	token := mintScoped(t, original, []string{"write"})
	// Test the actual token gate, independent of business validation.
	f.token = token
	for _, endpoint := range []string{"/enroll", "/enrollment/check"} {
		code, _ := f.json("POST", path+endpoint, body)
		require.Equal(t, http.StatusForbidden, code)
	}
	f.token = original
	response := env.DoRequest(t, "POST", "/api/users", map[string]string{"email": "console-member@test.com", "password": "password123", "role": "member"}, testutil.AuthHeader(original))
	require.Equal(t, http.StatusCreated, response.StatusCode)
	_ = response.Body.Close()
	f.token = env.LoginAs(t, "console-member@test.com", "password123")
	for _, endpoint := range []string{"/enroll", "/enrollment/check"} {
		code, _ := f.json("POST", path+endpoint, body)
		require.Equal(t, http.StatusForbidden, code)
	}
	f.token = original
	before := len(env.Asynq.Tasks)
	code, result := f.json("POST", path+"/enroll", body)
	require.Equal(t, http.StatusOK, code)
	require.Equal(t, "awaiting_approval", result["state"])
	require.NotContains(t, fmt.Sprint(result), "one-shot-never-persist")
	require.Len(t, env.Asynq.Tasks, before, "enrollment keys never enter Redis job payloads")
	code, result = f.json("POST", path+"/enrollment/check", nil)
	require.Equal(t, http.StatusOK, code)
	require.Equal(t, "awaiting_approval", result["state"])
	for range 5 {
		code, result = f.json("GET", path, nil)
		require.Equal(t, http.StatusOK, code)
		require.Equal(t, "awaiting_approval", result["enrollment"].(map[string]any)["state"])
	}
	require.Equal(t, 2, calls)
	f.awaitAudit("enroll_crowdsec_console", "check_crowdsec_console")
	var leaks int
	require.NoError(t, env.Pool.QueryRow(context.Background(), "SELECT count(*) FROM settings WHERE value LIKE '%one-shot-never-persist%'").Scan(&leaks))
	require.Zero(t, leaks)
	require.NoError(t, env.Pool.QueryRow(context.Background(), "SELECT count(*) FROM audit_logs WHERE details::text LIKE '%one-shot-never-persist%'").Scan(&leaks))
	require.Zero(t, leaks)
}
