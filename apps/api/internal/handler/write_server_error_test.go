package handler

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The defect this exists for: every 500 discarded its cause at the point it was
// known. An operator saw "failed to list scheduled tasks" and the server log
// held nothing at all, so a dropped database column was indistinguishable from
// a dead connection — diagnosing one took a schema diff against a freshly
// migrated database.
func TestWriteServerError_LogsTheCauseAndKeepsItOutOfTheBody(t *testing.T) {
	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet,
		"/api/projects/p/applications/a/scheduled-tasks", nil)
	cause := fmt.Errorf("query failed: %w",
		errors.New(`ERROR: column r.picked_up_at does not exist (SQLSTATE 42703)`))

	writeServerError(rec, req, "failed to list scheduled tasks", cause)

	require.Equal(t, http.StatusInternalServerError, rec.Code)

	// The client is told what failed, never why — the schema is not theirs.
	var body map[string]string
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.Equal(t, "failed to list scheduled tasks", body["error"])
	assert.NotContains(t, rec.Body.String(), "picked_up_at",
		"the cause leaked into the response")

	// The log carries the cause, plus what is needed to tie it to the
	// access-log line for the same request.
	line := logs.String()
	require.NotEmpty(t, line, "a 500 produced no log line at all — the original defect")
	assert.Contains(t, line, "picked_up_at", "the cause was discarded again")
	assert.Contains(t, line, "failed to list scheduled tasks")
	assert.Contains(t, line, `"method":"GET"`)
	assert.Contains(t, line, "/scheduled-tasks")
	assert.Contains(t, line, "request_id")
}

// A 500 a handler computes itself has no error to carry, but which handler
// answered 500 is still worth knowing — that was unknowable before.
func TestWriteServerError_LogsEvenWithoutAnError(t *testing.T) {
	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/host-shell", nil)
	writeServerError(rec, req, "could not determine host-shell helper image", nil)

	assert.Equal(t, http.StatusInternalServerError, rec.Code)
	line := logs.String()
	assert.Contains(t, line, "could not determine host-shell helper image")
	assert.Contains(t, line, "/api/host-shell")
	assert.NotContains(t, line, `"error"`, "no error to report, so none should be logged")
}

// Secrets reach these errors — a failing request can carry a token in a URL.
// redact.Error is what keeps them out of the log, and this pins that it runs.
func TestWriteServerError_RedactsTheCause(t *testing.T) {
	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/git/repos", nil)
	writeServerError(rec, req, "failed to reach the git provider",
		errors.New("clone https://x-access-token:ghs_SUPERSECRETVALUE@github.com/o/r failed"))

	line := logs.String()
	assert.NotContains(t, line, "ghs_SUPERSECRETVALUE", "a token reached the log")
	assert.Contains(t, line, "failed to reach the git provider")
}
