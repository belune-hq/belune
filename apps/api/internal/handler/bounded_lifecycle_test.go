package handler_test

import (
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/belune-hq/belune/internal/handler"
	"github.com/belune-hq/belune/internal/store/generated"
	"github.com/belune-hq/belune/internal/testutil"
)

// neverFinishes is a stop whose process ignores SIGTERM: it only returns when its own deadline does.
func neverFinishes(ctx context.Context, _ string) error {
	<-ctx.Done()
	return ctx.Err()
}

// ⛔ One call, gone. An application whose containers ignore SIGTERM used to answer 503 at 15s and
// leave it half-deleted (container Exited (137) but not removed, row still there), so deleting it
// "needed two tries". The delete is BOUNDED (5s polite stop, forced removal), so it answers inside
// the 15s window with the application really gone.
func TestDeleteApplication_AnswersWithinTheWindowEvenWhenTheStopNeverFinishes(t *testing.T) {
	resetDB(t)
	token := env.SetupAdmin(t, "admin@test.com", "password123")
	project := env.CreateProject(t, token, "Test Project", "test-project")
	projectID := extractID(project["id"])
	app := env.CreateApplication(t, token, projectID, map[string]any{
		"name": "Doomed", "type": "git", "build_type": "dockerfile", "source_repo": "https://github.com/test/repo",
	})
	appID := extractID(app["id"])

	env.Runtime.RemoveCalls = nil
	env.Runtime.StopFunc = neverFinishes
	t.Cleanup(func() { env.Runtime.StopFunc = nil })

	start := time.Now()
	resp := env.DoRequest(t, "DELETE", fmt.Sprintf("/api/projects/%s/applications/%s", projectID, appID), nil, testutil.AuthHeader(token))
	elapsed := time.Since(start)

	require.Equal(t, http.StatusOK, resp.StatusCode, "an answer, not a 503 at the 15s timeout")
	assert.Equal(t, "deleted", testutil.ReadJSON(t, resp)["status"])
	assert.Less(t, elapsed, 12*time.Second)
	assert.NotEmpty(t, env.Runtime.RemoveCalls, "the container was removed")

	resp = env.DoRequest(t, "GET", fmt.Sprintf("/api/projects/%s/applications/%s", projectID, appID), nil, testutil.AuthHeader(token))
	assert.Equal(t, http.StatusNotFound, resp.StatusCode, "and the application is really gone")
	_ = resp.Body.Close()
}

func seedRunningDatabase(t *testing.T, projectID string) (id string, uuid pgtype.UUID) {
	t.Helper()
	var proj pgtype.UUID
	require.NoError(t, proj.Scan(projectID))
	db, err := env.Queries.CreateDatabase(context.Background(), generated.CreateDatabaseParams{
		ProjectID: proj, Type: "other", Name: "slowdb", Slug: "slowdb-12345678", Version: "1",
		Status: "running", CredentialsEncrypted: []byte{0x00}, BackupMode: "none",
	})
	require.NoError(t, err)
	return extractID(db.ID.String()), db.ID
}

// A database whose container is slow to stop answers 202 with the database, and the restart FINISHES
// on its own: the container is started again and the status recorded after the request returned.
func TestRestartDatabase_ASlowStopAnswers202AndStillCompletes(t *testing.T) {
	resetDB(t)
	token := env.SetupAdmin(t, "admin@test.com", "password123")
	project := env.CreateProject(t, token, "Test Project", "test-project")
	projectID := extractID(project["id"])
	dbID, dbUUID := seedRunningDatabase(t, projectID)

	t.Cleanup(handler.SetAppActionWait(100 * time.Millisecond))
	env.Runtime.StopCalls, env.Runtime.StartCalls = nil, nil
	env.Runtime.StopFunc = func(context.Context, string) error { time.Sleep(500 * time.Millisecond); return nil }
	t.Cleanup(func() { env.Runtime.StopFunc = nil })

	resp := env.DoRequest(t, "POST", fmt.Sprintf("/api/projects/%s/databases/%s/restart", projectID, dbID), nil, testutil.AuthHeader(token))
	require.Equal(t, http.StatusAccepted, resp.StatusCode, "past the wait the answer is 202, not an error")
	body := testutil.ReadJSON(t, resp)
	assert.Equal(t, dbID, extractID(body["id"]), "202 carries the database, so clients keep their shape")

	require.Eventually(t, func() bool { return len(env.Runtime.StartCalls) > 0 },
		5*time.Second, 25*time.Millisecond, "the start follows the slow stop after the request has returned")
	require.Eventually(t, func() bool {
		got, err := env.Queries.GetDatabase(context.Background(), dbUUID)
		return err == nil && got.Status == "running"
	}, 5*time.Second, 25*time.Millisecond, "and the status is recorded")
}

// The client going away mid-stop must not strand the database.
func TestRestartDatabase_AClientThatDisconnectsMidStopStrandsNothing(t *testing.T) {
	resetDB(t)
	token := env.SetupAdmin(t, "admin@test.com", "password123")
	project := env.CreateProject(t, token, "Test Project", "test-project")
	projectID := extractID(project["id"])
	dbID, dbUUID := seedRunningDatabase(t, projectID)

	t.Cleanup(handler.SetAppActionWait(5 * time.Second)) // long: the CLIENT leaves first
	env.Runtime.StopCalls, env.Runtime.StartCalls = nil, nil
	env.Runtime.StopFunc = func(context.Context, string) error { time.Sleep(400 * time.Millisecond); return nil }
	t.Cleanup(func() { env.Runtime.StopFunc = nil })

	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, "POST",
		env.Server.URL+fmt.Sprintf("/api/projects/%s/databases/%s/restart", projectID, dbID), nil)
	require.NoError(t, err)
	for k, v := range testutil.AuthHeader(token) {
		req.Header.Set(k, v)
	}
	go func() { time.Sleep(100 * time.Millisecond); cancel() }()
	if resp, err := env.Server.Client().Do(req); err == nil {
		_ = resp.Body.Close()
	}

	require.Eventually(t, func() bool { return len(env.Runtime.StartCalls) > 0 },
		5*time.Second, 25*time.Millisecond, "the start still follows the stop although the client is gone")
	require.Eventually(t, func() bool {
		got, err := env.Queries.GetDatabase(context.Background(), dbUUID)
		return err == nil && got.Status == "running"
	}, 5*time.Second, 25*time.Millisecond)
}
