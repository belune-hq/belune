package handler_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/belune-hq/belune/internal/store/generated"
	"github.com/belune-hq/belune/internal/testutil"
)

type ptFixture struct {
	token, projectID, appID string
	appSlug                 string
	appUUID                 pgtype.UUID
}

func newPTFixture(t *testing.T) ptFixture {
	t.Helper()
	resetDB(t)
	token := env.SetupAdmin(t, "pt@test.com", "password123")
	projectID := extractID(env.CreateProject(t, token, "PT", "pt")["id"])
	app := env.CreateApplication(t, token, projectID, map[string]any{
		"name": "App", "type": "image", "build_type": "image", "source_image": "nginx:alpine",
	})
	f := ptFixture{token: token, projectID: projectID, appID: extractID(app["id"])}
	require.NoError(t, f.appUUID.Scan(f.appID))
	row, err := env.Queries.GetApplication(context.Background(), f.appUUID)
	require.NoError(t, err)
	f.appSlug = row.Slug
	return f
}

func (f ptFixture) path(suffix string) string {
	return fmt.Sprintf("/api/projects/%s/applications/%s/process-types%s", f.projectID, f.appID, suffix)
}

func (f ptFixture) do(t *testing.T, method, suffix string, body any) (int, map[string]any) {
	t.Helper()
	resp := env.DoRequest(t, method, f.path(suffix), body, testutil.AuthHeader(f.token))
	code := resp.StatusCode
	return code, readJSONClose(t, resp)
}

func (f ptFixture) row(t *testing.T, id string) generated.ApplicationProcessType {
	t.Helper()
	var u pgtype.UUID
	require.NoError(t, u.Scan(id))
	pt, err := env.Queries.GetProcessType(context.Background(), u)
	require.NoError(t, err)
	return pt
}

// createWorker makes a non-routable type and returns its id.
func (f ptFixture) createWorker(t *testing.T, name string, extra map[string]any) string {
	t.Helper()
	body := map[string]any{"name": name, "command": "run " + name}
	for k, v := range extra {
		body[k] = v
	}
	code, out := f.do(t, "POST", "", body)
	require.Equal(t, http.StatusCreated, code, "%v", out)
	return extractID(out["id"])
}

func TestProcessTypes_ListShowsTheBackfilledRoutableWebType(t *testing.T) {
	f := newPTFixture(t)
	resp := env.DoRequest(t, "GET", f.path(""), nil, testutil.AuthHeader(f.token))
	require.Equal(t, http.StatusOK, resp.StatusCode)
	defer func() { _ = resp.Body.Close() }()
	var list []map[string]any
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&list))
	require.Len(t, list, 1)
	assert.Equal(t, "web", list[0]["name"])
	assert.Equal(t, true, list[0]["routable"])
	assert.EqualValues(t, 1, list[0]["replicas"], "replicas is shown")
	assert.Nil(t, list[0]["cpu_limit"], "a type with no limit of its own reports null (inherits the application's)")
}

// ⛔ The one to look at first. UpdateProcessType is a FULL-ROW update, so a PUT that
// names only `command` must leave every other column exactly as it was. The WHOLE row
// is compared, so a column nobody listed (replicas, the limits) is covered too.
func TestProcessTypes_PartialPutLeavesEveryOtherColumnUntouched(t *testing.T) {
	f := newPTFixture(t)
	id := f.createWorker(t, "worker", map[string]any{
		"health_check_type": "command", "health_check_command": "pgrep worker",
	})
	// Populate the columns the API does not expose, directly, so a write that defaults
	// them shows as a difference instead of a zero staying a zero.
	_, err := env.Pool.Exec(context.Background(),
		`UPDATE application_process_types SET replicas = 3, cpu_limit = 0.5, memory_limit = 268435456 WHERE id = $1`, id)
	require.NoError(t, err)
	before := f.row(t, id)

	code, out := f.do(t, "PUT", "/"+id, map[string]any{"command": "run-something-else"})
	require.Equal(t, http.StatusOK, code, "%v", out)

	after := f.row(t, id)
	assert.Equal(t, "run-something-else", after.Command)
	assert.Equal(t, before.Name, after.Name)
	assert.Equal(t, before.Replicas, after.Replicas, "replicas is carried over, not defaulted")
	assert.Equal(t, before.Routable, after.Routable)
	assert.Equal(t, before.CpuLimit, after.CpuLimit)
	assert.Equal(t, before.MemoryLimit, after.MemoryLimit)
	assert.Equal(t, before.HealthCheckType, after.HealthCheckType)
	assert.Equal(t, before.HealthCheckCommand, after.HealthCheckCommand)

	// And the other direction: clearing is explicit, with an empty string.
	code, out = f.do(t, "PUT", "/"+id, map[string]any{"health_check_type": "", "health_check_command": ""})
	require.Equal(t, http.StatusOK, code, "%v", out)
	cleared := f.row(t, id)
	assert.False(t, cleared.HealthCheckType.Valid)
	assert.False(t, cleared.HealthCheckCommand.Valid)
	assert.Equal(t, "run-something-else", cleared.Command, "and the command is still untouched")
}

// The two uniqueness conflicts are different conditions and must say so.
func TestProcessTypes_TheTwoConflictsAreDistinguishedByIndex(t *testing.T) {
	f := newPTFixture(t)
	f.createWorker(t, "worker", nil)

	code, dup := f.do(t, "POST", "", map[string]any{"name": "worker"})
	require.Equal(t, http.StatusConflict, code)
	assert.Contains(t, dup["error"], "name already exists")
	assert.NotContains(t, dup["error"], "routable")

	// The application already has its backfilled routable type.
	code, second := f.do(t, "POST", "", map[string]any{"name": "frontend", "routable": true})
	require.Equal(t, http.StatusConflict, code)
	assert.Contains(t, second["error"], "already has a routable process type")
	assert.NotContains(t, second["error"], "name already exists", "a user adding a second routable type must not be told their name is taken")
}

func TestProcessTypes_NameAndRoutableAreNotPatchable(t *testing.T) {
	f := newPTFixture(t)
	id := f.createWorker(t, "worker", nil)

	// Name and routable are identities, not patchable.
	code, _ := f.do(t, "PUT", "/"+id, map[string]any{"routable": true})
	assert.Equal(t, http.StatusBadRequest, code)
	code, _ = f.do(t, "PUT", "/"+id, map[string]any{"name": "renamed"})
	assert.Equal(t, http.StatusBadRequest, code)
}

// A worker set to http verifies nothing (only a command check gates a non-routable
// type), so it is refused rather than stored and silently ignored.
func TestProcessTypes_HTTPHealthCheckIsRefusedOnANonRoutableType(t *testing.T) {
	f := newPTFixture(t)
	code, out := f.do(t, "POST", "", map[string]any{"name": "worker", "health_check_type": "http"})
	assert.Equal(t, http.StatusBadRequest, code)
	assert.Contains(t, out["error"], "routable")

	id := f.createWorker(t, "worker", nil)
	code, _ = f.do(t, "PUT", "/"+id, map[string]any{"health_check_type": "http"})
	assert.Equal(t, http.StatusBadRequest, code)

	// command and none are fine on a worker; all three on the routable type.
	code, _ = f.do(t, "PUT", "/"+id, map[string]any{"health_check_type": "none"})
	assert.Equal(t, http.StatusOK, code)
	web := f.webTypeID(t)
	code, _ = f.do(t, "PUT", "/"+web, map[string]any{"health_check_type": "http"})
	assert.Equal(t, http.StatusOK, code)

	// A command check with no command anywhere to inherit is refused too.
	code, out = f.do(t, "PUT", "/"+id, map[string]any{"health_check_type": "command"})
	assert.Equal(t, http.StatusBadRequest, code)
	assert.Contains(t, out["error"], "health_check_command")
}

func (f ptFixture) webTypeID(t *testing.T) string {
	t.Helper()
	pt, err := env.Queries.GetProcessTypeByName(context.Background(), generated.GetProcessTypeByNameParams{ApplicationID: f.appUUID, Name: "web"})
	require.NoError(t, err)
	return pgUUID(pt.ID)
}

var workerName = regexp.MustCompile(`-worker-1-[0-9a-f]{8}$`)

func TestProcessTypes_CreatingOneWritesItsInstanceWithAMintedName(t *testing.T) {
	f := newPTFixture(t)
	code, out := f.do(t, "POST", "", map[string]any{"name": "worker", "command": "bundle exec sidekiq"})
	require.Equal(t, http.StatusCreated, code, "%v", out)

	insts, err := env.Queries.ListInstancesByApplication(context.Background(), f.appUUID)
	require.NoError(t, err)
	require.Len(t, insts, 2, "the backfilled web instance and the new one")
	var found bool
	for _, in := range insts {
		if workerName.MatchString(in.ContainerName) && strings.HasPrefix(in.ContainerName, f.appSlug+"-worker-1-") {
			found = true
			assert.False(t, in.DeploymentID.Valid, "no deployment has created this container yet, and the column stays nullable")
			assert.EqualValues(t, 1, in.Idx)
			assert.Equal(t, "pending", in.Status)
		} else {
			assert.Equal(t, f.appSlug, in.ContainerName, "the existing web instance is never renamed")
		}
	}
	assert.True(t, found, "a new name is {appSlug}-{type}-{idx}-{8 hex}")

	// Nothing was deployed by the POST.
	assert.Empty(t, env.Asynq.Tasks, "creating a type must not enqueue a deploy")
}

func TestProcessTypes_ANameThatCouldNotBeAContainerIsA400(t *testing.T) {
	f := newPTFixture(t)
	_, err := env.Pool.Exec(context.Background(), `UPDATE applications SET slug = $2 WHERE id = $1`, f.appID, strings.Repeat("a", 250))
	require.NoError(t, err)
	code, out := f.do(t, "POST", "", map[string]any{"name": "worker"})
	assert.Equal(t, http.StatusBadRequest, code)
	assert.Contains(t, out["error"], "255")

	for _, bad := range []string{"", "Worker", "1worker", "has_underscore", strings.Repeat("a", 21)} {
		code, _ = f.do(t, "POST", "", map[string]any{"name": bad})
		assert.Equal(t, http.StatusBadRequest, code, "name %q", bad)
	}
}

func TestProcessTypes_TheRoutableTypeCannotBeDeleted(t *testing.T) {
	f := newPTFixture(t)
	web := f.webTypeID(t)
	code, out := f.do(t, "DELETE", "/"+web, nil)
	require.Equal(t, http.StatusConflict, code)
	assert.Contains(t, out["error"], "routable")

	insts, err := env.Queries.ListInstancesByApplication(context.Background(), f.appUUID)
	require.NoError(t, err)
	assert.Len(t, insts, 1, "the instance the proxy routes to survives")
}

func TestProcessTypes_DeletingAWorkerRemovesItsRowsAndItsContainer(t *testing.T) {
	f := newPTFixture(t)
	id := f.createWorker(t, "worker", nil)
	stored := f.instances(t, id)[0].ContainerName
	env.Runtime.StopCalls, env.Runtime.RemoveCalls = nil, nil

	code, out := f.do(t, "DELETE", "/"+id, nil)
	require.Equal(t, http.StatusOK, code, "%v", out)

	assert.Contains(t, env.Runtime.StopCalls, stored)
	assert.Contains(t, env.Runtime.RemoveCalls, stored, "removed now, not left to the 24h orphan sweep")
	assert.NotContains(t, env.Runtime.RemoveCalls, f.appSlug, "the web container is not touched")

	insts, err := env.Queries.ListInstancesByApplication(context.Background(), f.appUUID)
	require.NoError(t, err)
	assert.Len(t, insts, 1)
	types, err := env.Queries.ListProcessTypesByApplication(context.Background(), f.appUUID)
	require.NoError(t, err)
	assert.Len(t, types, 1)
}

func TestProcessTypes_AnotherApplicationsTypeIsNotFound(t *testing.T) {
	f := newPTFixture(t)
	other := env.CreateApplication(t, f.token, f.projectID, map[string]any{
		"name": "Other", "type": "image", "build_type": "image", "source_image": "nginx:alpine",
	})
	otherID := extractID(other["id"])
	var otherUUID pgtype.UUID
	require.NoError(t, otherUUID.Scan(otherID))
	pt, err := env.Queries.GetProcessTypeByName(context.Background(), generated.GetProcessTypeByNameParams{ApplicationID: otherUUID, Name: "web"})
	require.NoError(t, err)

	code, _ := f.do(t, "PUT", "/"+pgUUID(pt.ID), map[string]any{"command": "x"})
	assert.Equal(t, http.StatusNotFound, code, "the type must belong to the application in the path")
	code, _ = f.do(t, "DELETE", "/"+pgUUID(pt.ID), nil)
	assert.Equal(t, http.StatusNotFound, code)
}

func pgUUID(u pgtype.UUID) string { return u.String() }

// A freshly created application checks nothing and says so: the column default is
// 'none' (000081), not an http probe with no path.
func TestProcessTypes_ANewApplicationStartsWithNoHealthCheckAndSaysSo(t *testing.T) {
	f := newPTFixture(t)

	app, err := env.Queries.GetApplication(context.Background(), f.appUUID)
	require.NoError(t, err)
	assert.Equal(t, "none", app.HealthCheckType)
	assert.False(t, app.HealthCheckPath.Valid, "no path is guessed: a probe that expects 2xx would eject an app that redirects")

	resp := env.DoRequest(t, "GET", f.path(""), nil, testutil.AuthHeader(f.token))
	require.Equal(t, http.StatusOK, resp.StatusCode)
	defer func() { _ = resp.Body.Close() }()
	var list []map[string]any
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&list))
	require.Len(t, list, 1)
	assert.Equal(t, map[string]any{"kind": "none", "source": "inherited", "inert": false}, list[0]["resolved_health"])
}
