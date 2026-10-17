package handler

import (
	"bufio"
	"fmt"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/weiliang79/belune/internal/naming"
	"github.com/weiliang79/belune/internal/pkg/sse"
	"github.com/weiliang79/belune/internal/store/generated"
)

//apidoc:tag applications/metrics-logs
//apidoc:title Stream Logs
//apidoc:order 4
func (h *Handler) StreamLogs(w http.ResponseWriter, r *http.Request) {
	applicationID := chi.URLParam(r, "applicationId")
	var applicationUUID pgtype.UUID
	if err := applicationUUID.Scan(applicationID); err != nil {
		writeError(w, http.StatusBadRequest, "invalid application id")
		return
	}

	if !h.canAccessApplication(r, applicationUUID) {
		writeError(w, http.StatusForbidden, "access denied")
		return
	}

	row, err := h.queries.GetApplicationWithProjectSlug(r.Context(), applicationUUID)
	if err != nil {
		writeError(w, http.StatusNotFound, "application not found")
		return
	}
	containerName := naming.ContainerName(row.ProjectSlug, row.Slug, applicationID)

	writer, err := sse.NewWriter(w)
	if err != nil {
		writeServerError(w, r, "streaming not supported", err)
		return
	}

	follow := r.URL.Query().Get("follow") == "true"

	rt, err := h.runtimes.For(r.Context(), row.ServerID)
	if err != nil {
		writer.SendEvent("error", "failed to reach the application's server")
		return
	}

	logs, err := rt.ContainerLogs(r.Context(), containerName, follow)
	if err != nil {
		writer.SendEvent("error", fmt.Sprintf("failed to get logs: %v", err))
		return
	}
	defer logs.Close()

	scanner := bufio.NewScanner(logs)
	for scanner.Scan() {
		line := scanner.Text()
		if err := writer.SendData(line); err != nil {
			return // Client disconnected
		}
	}

	writer.SendEvent("done", "log stream ended")
}

// ListApplicationLogs returns paginated, filterable historical application logs.
// GET /api/projects/{projectId}/applications/{applicationId}/logs/history
//
//apidoc:tag applications/metrics-logs
//apidoc:title Get Logs
//apidoc:order 3
func (h *Handler) ListApplicationLogs(w http.ResponseWriter, r *http.Request) {
	appID := chi.URLParam(r, "applicationId")
	var appUUID pgtype.UUID
	if err := appUUID.Scan(appID); err != nil {
		writeError(w, http.StatusBadRequest, "invalid application id")
		return
	}
	if !h.canAccessApplication(r, appUUID) {
		writeError(w, http.StatusForbidden, "access denied")
		return
	}
	h.listContainerLogs(w, r, "application", appUUID)
}

// ListDatabaseLogs returns paginated, filterable historical database logs.
// GET /api/projects/{projectId}/databases/{databaseId}/logs/history
//
//apidoc:tag databases/logs
//apidoc:title Get Logs
//apidoc:order 2
func (h *Handler) ListDatabaseLogs(w http.ResponseWriter, r *http.Request) {
	dbID := chi.URLParam(r, "databaseId")
	var dbUUID pgtype.UUID
	if err := dbUUID.Scan(dbID); err != nil {
		writeError(w, http.StatusBadRequest, "invalid database id")
		return
	}
	if !h.canAccessDatabase(r, dbUUID) {
		writeError(w, http.StatusForbidden, "access denied")
		return
	}
	h.listContainerLogs(w, r, "database", dbUUID)
}

// ListApplicationLogSessions returns the log sessions (one per deployment that
// produced log lines, plus a NULL bucket for earlier/unassigned logs) for an
// application, so the viewer can offer a session picker.
// GET /api/projects/{projectId}/applications/{applicationId}/logs/sessions
//
//apidoc:tag applications/metrics-logs
//apidoc:title Get Sessions
//apidoc:order 2
func (h *Handler) ListApplicationLogSessions(w http.ResponseWriter, r *http.Request) {
	appID := chi.URLParam(r, "applicationId")
	var appUUID pgtype.UUID
	if err := appUUID.Scan(appID); err != nil {
		writeError(w, http.StatusBadRequest, "invalid application id")
		return
	}
	if !h.canAccessApplication(r, appUUID) {
		writeError(w, http.StatusForbidden, "access denied")
		return
	}
	h.listContainerLogSessions(w, r, "application", appUUID)
}

// ListDatabaseLogSessions is the database counterpart. Databases have no
// deployments, so this collapses to a single NULL-bucket session, but it keeps
// the viewer's data shape uniform across resource types.
// GET /api/projects/{projectId}/databases/{databaseId}/logs/sessions
//
//apidoc:tag databases/logs
//apidoc:title Get Sessions
//apidoc:order 1
func (h *Handler) ListDatabaseLogSessions(w http.ResponseWriter, r *http.Request) {
	dbID := chi.URLParam(r, "databaseId")
	var dbUUID pgtype.UUID
	if err := dbUUID.Scan(dbID); err != nil {
		writeError(w, http.StatusBadRequest, "invalid database id")
		return
	}
	if !h.canAccessDatabase(r, dbUUID) {
		writeError(w, http.StatusForbidden, "access denied")
		return
	}
	h.listContainerLogSessions(w, r, "database", dbUUID)
}

// maxLogSessions bounds the session picker. A long-lived application gains a
// session per deploy, and all of them would otherwise be returned and rendered
// in one dropdown. The most recent are the ones anyone reads; older logs remain
// reachable through the unfiltered view.
const maxLogSessions = 50

func (h *Handler) listContainerLogSessions(w http.ResponseWriter, r *http.Request, sourceType string, sourceID pgtype.UUID) {
	sessions, err := h.queries.ListContainerLogSessions(r.Context(), generated.ListContainerLogSessionsParams{
		SourceType: sourceType,
		SourceID:   sourceID,
		Limit:      maxLogSessions,
	})
	if err != nil {
		writeServerError(w, r, "failed to list log sessions", err)
		return
	}
	writeJSON(w, http.StatusOK, sessions)
}

// listContainerLogs is the shared implementation behind the application and
// database log-history endpoints.
//
// Query params:
//
//	q      — keyword search (case-insensitive substring match on message)
//	level  — filter by level: "debug" | "info" | "warning" | "error"
//	stream — filter by stream: "stdout" or "stderr"
//	since  — RFC3339 timestamp lower bound (inclusive)
//	until  — RFC3339 timestamp upper bound (inclusive)
//	limit  — page size (default 500, max 1000)
//	offset — pagination offset
func (h *Handler) listContainerLogs(w http.ResponseWriter, r *http.Request, sourceType string, sourceID pgtype.UUID) {
	q := r.URL.Query()
	limit, offset := parseLogspagination(r)

	params := generated.SearchContainerLogsParams{
		SourceType: sourceType,
		SourceID:   sourceID,
		Limit:      limit,
		Offset:     offset,
	}
	if v := q.Get("q"); v != "" {
		params.Q = pgtype.Text{String: v, Valid: true}
	}
	if v := q.Get("level"); v == "debug" || v == "info" || v == "warning" || v == "error" {
		params.Level = pgtype.Text{String: v, Valid: true}
	}
	if v := q.Get("stream"); v == "stdout" || v == "stderr" {
		params.Stream = pgtype.Text{String: v, Valid: true}
	}
	if v := q.Get("since"); v != "" {
		if t, err := time.Parse(time.RFC3339, v); err == nil {
			params.Since = pgtype.Timestamptz{Time: t, Valid: true}
		}
	}
	if v := q.Get("until"); v != "" {
		if t, err := time.Parse(time.RFC3339, v); err == nil {
			params.Until = pgtype.Timestamptz{Time: t, Valid: true}
		}
	}
	// Session filter. "none" selects the unassigned (NULL) bucket — logs from
	// before this feature or from non-deployment containers; a UUID selects that
	// one deployment's session; absent means all sessions.
	// Sessions are keyed by container generation, so every source type has them —
	// including databases, which have no deployment. "none" selects the
	// unassigned bucket of rows collected before sessions existed.
	if v := q.Get("session"); v == "none" {
		params.Unassigned = pgtype.Bool{Bool: true, Valid: true}
	} else if v != "" {
		params.ContainerID = pgtype.Text{String: v, Valid: true}
	}

	logs, err := h.queries.SearchContainerLogs(r.Context(), params)
	if err != nil {
		writeServerError(w, r, "failed to list logs", err)
		return
	}

	writeJSON(w, http.StatusOK, logs)
}

// parseLogspagination is like parsePagination but with a higher default limit for log pages.
func parseLogspagination(r *http.Request) (int32, int32) {
	limit := int32(500)
	offset := int32(0)
	if v := r.URL.Query().Get("limit"); v != "" {
		var parsed int32
		if n, err := fmt.Sscanf(v, "%d", &parsed); n == 1 && err == nil && parsed > 0 && parsed <= 1000 {
			limit = parsed
		}
	}
	if v := r.URL.Query().Get("offset"); v != "" {
		var parsed int32
		if n, err := fmt.Sscanf(v, "%d", &parsed); n == 1 && err == nil && parsed >= 0 {
			offset = parsed
		}
	}
	return limit, offset
}
