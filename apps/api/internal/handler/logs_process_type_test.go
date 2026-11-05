package handler_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/belune-hq/belune/internal/status"
	"github.com/belune-hq/belune/internal/store/generated"
	"github.com/belune-hq/belune/internal/testutil"
)

// seedTypedLogApp creates an application with web + queue + scheduler instances,
// one deployment, and returns what the log tests need.
func seedTypedLogApp(t *testing.T) (token, projectID, appID string, appUUID, depID pgtype.UUID, instByType map[string]generated.ApplicationInstance) {
	t.Helper()
	resetDB(t)
	token = env.SetupAdmin(t, "admin@test.com", "password123")
	project := env.CreateProject(t, token, "Log Project", "log-project")
	projectID = extractID(project["id"])
	app := env.CreateApplication(t, token, projectID, map[string]any{
		"name": "Log App", "type": "image", "build_type": "image", "source_image": "nginx:latest",
	})
	appID = extractID(app["id"])
	require.NoError(t, appUUID.Scan(appID))
	ctx := context.Background()

	dep, err := env.Queries.CreateDeployment(ctx, generated.CreateDeploymentParams{
		ApplicationID: appUUID, Status: status.DeploymentSuccess, TriggeredBy: "manual",
	})
	require.NoError(t, err)
	depID = dep.ID

	// Creation already seeded the routable "web" type and instance.
	existing, err := env.Queries.ListInstancesByApplication(ctx, appUUID)
	require.NoError(t, err)
	require.Len(t, existing, 1)
	instByType = map[string]generated.ApplicationInstance{"web": existing[0]}
	for _, name := range []string{"queue", "scheduler"} {
		pt, err := env.Queries.CreateProcessType(ctx, generated.CreateProcessTypeParams{
			ApplicationID: appUUID, Name: name, Replicas: 1, Routable: false,
		})
		require.NoError(t, err)
		inst, err := env.Queries.CreateInstance(ctx, generated.CreateInstanceParams{
			ApplicationID: appUUID, ProcessTypeID: pt.ID, Idx: 1,
			ContainerName: fmt.Sprintf("%s-%s", existing[0].ContainerName, name), Status: "running",
		})
		require.NoError(t, err)
		instByType[name] = inst
	}
	return
}

func insertTypedLog(t *testing.T, appUUID, dep pgtype.UUID, container, processType, msg string) {
	t.Helper()
	require.NoError(t, env.Queries.InsertContainerLog(context.Background(), generated.InsertContainerLogParams{
		SourceType: "application", SourceID: appUUID, Level: "info", Stream: "stdout", Message: msg,
		DeploymentID: dep, ContainerID: pgtype.Text{String: container, Valid: true},
		ProcessType: pgtype.Text{String: processType, Valid: processType != ""},
	}))
}

// Unknown comes back as "" on the sessions row (the cast must be honest); the live
// payload uses null. The picker defect: three containers of ONE deployment must come back as three
// sessions that say what they are, not three identical rows.
func TestLogSessions_CarryTheProcessType(t *testing.T) {
	token, projectID, appID, appUUID, dep, _ := seedTypedLogApp(t)
	insertTypedLog(t, appUUID, dep, "ctr-web", "web", "w1")
	insertTypedLog(t, appUUID, dep, "ctr-queue", "queue", "q1")
	insertTypedLog(t, appUUID, dep, "ctr-sched", "scheduler", "s1")
	insertTypedLog(t, appUUID, dep, "ctr-old", "", "pre-upgrade line") // unknown

	resp := env.DoRequest(t, "GET", fmt.Sprintf("/api/projects/%s/applications/%s/logs/sessions", projectID, appID), nil, testutil.AuthHeader(token))
	require.Equal(t, http.StatusOK, resp.StatusCode)
	byContainer := map[string]any{}
	for _, s := range testutil.ReadJSONArray(t, resp) {
		row := s.(map[string]any)
		byContainer[row["container_id"].(string)] = row["process_type"]
	}
	assert.Equal(t, map[string]any{"ctr-web": "web", "ctr-queue": "queue", "ctr-sched": "scheduler", "ctr-old": ""}, byContainer)
}

// The reason the type is a column: filtering by it reaches lines from container
// generations that no instance row points at any more.
func TestLogHistory_ProcessTypeFilterSpansGenerations(t *testing.T) {
	token, projectID, appID, appUUID, dep, _ := seedTypedLogApp(t)
	insertTypedLog(t, appUUID, dep, "ctr-queue-gen1", "queue", "yesterday: queue failed")
	insertTypedLog(t, appUUID, dep, "ctr-queue-gen2", "queue", "today: queue ok")
	insertTypedLog(t, appUUID, dep, "ctr-web-gen2", "web", "web line")
	insertTypedLog(t, appUUID, dep, "ctr-unknown", "", "unknown line")

	get := func(query string) []string {
		resp := env.DoRequest(t, "GET", fmt.Sprintf("/api/projects/%s/applications/%s/logs/history%s", projectID, appID, query), nil, testutil.AuthHeader(token))
		require.Equal(t, http.StatusOK, resp.StatusCode)
		var msgs []string
		for _, l := range testutil.ReadJSONArray(t, resp) {
			msgs = append(msgs, l.(map[string]any)["message"].(string))
		}
		return msgs
	}
	assert.ElementsMatch(t, []string{"yesterday: queue failed", "today: queue ok"}, get("?process_type=queue"))
	assert.Len(t, get(""), 4, "no filter keeps today's behaviour: every line")
	assert.Empty(t, get("?process_type=nonexistent"))
}

// ?process_type= tails that type's container; absent tails the routable one.
func TestStreamLogs_ProcessTypeSelectsTheContainer(t *testing.T) {
	token, projectID, appID, _, _, inst := seedTypedLogApp(t)
	env.Runtime.ContainerLogsBody_ = map[string]string{
		inst["web"].ContainerName:   "web output\n",
		inst["queue"].ContainerName: "queue output\n",
	}
	stream := func(query string) string {
		resp := env.DoRequest(t, "GET", fmt.Sprintf("/api/projects/%s/applications/%s/logs%s", projectID, appID, query), nil, testutil.AuthHeader(token))
		defer func() { _ = resp.Body.Close() }()
		b, _ := io.ReadAll(resp.Body)
		return string(b)
	}
	assert.Contains(t, stream("?process_type=queue"), "queue output")
	assert.Contains(t, stream(""), "web output", "absent param keeps the routable type")

	resp := env.DoRequest(t, "GET", fmt.Sprintf("/api/projects/%s/applications/%s/logs?process_type=nope", projectID, appID), nil, testutil.AuthHeader(token))
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
}

// Terminal: ?process_type=queue opens the shell in the queue container, absent in
// the routable one, unknown is a 404 -- and two sessions into two containers of one
// application are both live (the manager keys on a generated id, caps per USER).
func TestTerminal_ProcessTypeSelectsTheContainer(t *testing.T) {
	token, projectID, appID, _, _, inst := seedTypedLogApp(t)
	env.Runtime.ExecTTYRequested = nil
	open := func(query string) (int, string) {
		resp := env.DoRequest(t, "POST", fmt.Sprintf("/api/projects/%s/applications/%s/terminal%s", projectID, appID, query), map[string]any{}, testutil.AuthHeader(token))
		defer func() { _ = resp.Body.Close() }()
		var out map[string]string
		_ = json.NewDecoder(resp.Body).Decode(&out)
		return resp.StatusCode, out["session_id"]
	}

	code, queueSession := open("?process_type=queue")
	require.Equal(t, http.StatusCreated, code)
	code, webSession := open("")
	require.Equal(t, http.StatusCreated, code)
	assert.NotEqual(t, queueSession, webSession, "two terminals into one application are two sessions")
	assert.Equal(t, []string{inst["queue"].ContainerName, inst["web"].ContainerName}, env.Runtime.ExecTTYRequested)

	code, _ = open("?process_type=nope")
	assert.Equal(t, http.StatusNotFound, code)
}

// Per-instance restart: only that container is stopped and started, the others and
// the application's own status are left alone, and the action is audited.
func TestRestartInstance_RestartsOnlyThatContainerAndIsAudited(t *testing.T) {
	token, projectID, appID, _, _, inst := seedTypedLogApp(t)
	env.Runtime.StopCalls, env.Runtime.StartCalls = nil, nil
	base := auditedServerURL(t)

	q := inst["queue"]
	req, err := http.NewRequest("POST", fmt.Sprintf("%s/api/projects/%s/applications/%s/instances/%s/restart", base, projectID, appID, uuidStr(q.ID)), nil)
	require.NoError(t, err)
	for k, v := range testutil.AuthHeader(token) {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	assert.Equal(t, []string{q.ContainerName}, env.Runtime.StopCalls, "only the queue container is stopped")
	assert.Equal(t, []string{q.ContainerName}, env.Runtime.StartCalls, "and started again")

	var details []byte
	require.Eventually(t, func() bool {
		return env.Pool.QueryRow(context.Background(),
			`SELECT details FROM audit_logs WHERE action = 'restart_instance' AND resource_id = $1`, appID).Scan(&details) == nil
	}, 2*time.Second, 10*time.Millisecond, "a restart must write an audit row")
	assert.Contains(t, string(details), q.ContainerName)

	// An instance of another application is a 404, not a restart.
	resp2 := env.DoRequest(t, "POST", fmt.Sprintf("/api/projects/%s/applications/%s/instances/%s/restart", projectID, appID, "00000000-0000-4000-8000-000000000000"), nil, testutil.AuthHeader(token))
	defer func() { _ = resp2.Body.Close() }()
	assert.Equal(t, http.StatusNotFound, resp2.StatusCode)
}

func uuidStr(u pgtype.UUID) string { return u.String() }

// The API reports what each type actually runs with, from the one server-side rule,
// so the UI no longer mirrors it.
func TestListProcessTypes_ReportsTheResolvedHealth(t *testing.T) {
	token, projectID, appID, appUUID, _, _ := seedTypedLogApp(t)
	ctx := context.Background()
	_, err := env.Pool.Exec(ctx, `UPDATE applications SET health_check_type = 'http' WHERE id = $1`, appUUID)
	require.NoError(t, err)
	// web: routable, unset (inherits http). queue: non-routable, unset (none).
	// scheduler: non-routable, sets command.
	_, err = env.Pool.Exec(ctx, `UPDATE application_process_types SET health_check_type = 'command', health_check_command = 'true' WHERE application_id = $1 AND name = 'scheduler'`, appUUID)
	require.NoError(t, err)

	resp := env.DoRequest(t, "GET", fmt.Sprintf("/api/projects/%s/applications/%s/process-types", projectID, appID), nil, testutil.AuthHeader(token))
	require.Equal(t, http.StatusOK, resp.StatusCode)
	got := map[string]map[string]any{}
	for _, row := range testutil.ReadJSONArray(t, resp) {
		m := row.(map[string]any)
		got[m["name"].(string)] = m["resolved_health"].(map[string]any)
	}
	assert.Equal(t, map[string]any{"kind": "http", "source": "inherited"}, got["web"])
	assert.Equal(t, map[string]any{"kind": "none", "source": "default"}, got["queue"], "a worker never inherits http")
	assert.Equal(t, map[string]any{"kind": "command", "source": "set"}, got["scheduler"])
}
