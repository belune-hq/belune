package handler

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/weiliang79/belune/internal/store/generated"
)

// auditLogDTO wraps a generated audit row but exposes `details` as raw JSON.
// The column is JSONB (already valid JSON) yet sqlc types it as []byte, which
// encoding/json would emit as a base64 string. The embedded field is shadowed
// by this shallower Details, so a NULL detail marshals to `null`.
type auditLogDTO struct {
	generated.ListAuditLogsFilteredRow
	Details json.RawMessage `json:"details"`
}

func toAuditLogDTOs(rows []generated.ListAuditLogsFilteredRow) []auditLogDTO {
	out := make([]auditLogDTO, len(rows))
	for i, r := range rows {
		out[i] = auditLogDTO{
			ListAuditLogsFilteredRow: r,
			Details:                  json.RawMessage(r.Details),
		}
	}
	return out
}

// auditFilter holds the shared audit-log filter columns.
type auditFilter struct {
	UserID       pgtype.UUID
	Action       pgtype.Text
	ResourceType pgtype.Text
	ResourceID   pgtype.Text
	From         pgtype.Timestamptz
	To           pgtype.Timestamptz
}

// parseAuditFilter reads the audit filter query params. Returns a non-empty
// error message when a value is malformed.
func parseAuditFilter(r *http.Request) (auditFilter, string) {
	var f auditFilter
	q := r.URL.Query()
	if v := q.Get("user_id"); v != "" {
		var uuid pgtype.UUID
		if err := uuid.Scan(v); err != nil {
			return f, "invalid user_id format"
		}
		f.UserID = uuid
	}
	if v := q.Get("action"); v != "" {
		f.Action = pgtype.Text{String: v, Valid: true}
	}
	if v := q.Get("resource_type"); v != "" {
		f.ResourceType = pgtype.Text{String: v, Valid: true}
	}
	if v := q.Get("resource_id"); v != "" {
		f.ResourceID = pgtype.Text{String: v, Valid: true}
	}
	if v := q.Get("from"); v != "" {
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			return f, "invalid from format, expected RFC3339"
		}
		f.From = pgtype.Timestamptz{Time: t, Valid: true}
	}
	if v := q.Get("to"); v != "" {
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			return f, "invalid to format, expected RFC3339"
		}
		f.To = pgtype.Timestamptz{Time: t, Valid: true}
	}
	return f, ""
}

func (f auditFilter) listParams(limit, offset int32) generated.ListAuditLogsFilteredParams {
	return generated.ListAuditLogsFilteredParams{
		Limit:        limit,
		Offset:       offset,
		UserID:       f.UserID,
		Action:       f.Action,
		ResourceType: f.ResourceType,
		ResourceID:   f.ResourceID,
		From:         f.From,
		To:           f.To,
	}
}

func (f auditFilter) countParams() generated.CountAuditLogsFilteredParams {
	return generated.CountAuditLogsFilteredParams{
		UserID:       f.UserID,
		Action:       f.Action,
		ResourceType: f.ResourceType,
		ResourceID:   f.ResourceID,
		From:         f.From,
		To:           f.To,
	}
}

// ListAuditLogs returns paginated, optionally filtered audit logs (admin-only).
// Query params: limit, offset, user_id, action, resource_type, resource_id, from, to.
//
//apidoc:tag platform/audit-logs
//apidoc:title Get Logs
//apidoc:order 1
func (h *Handler) ListAuditLogs(w http.ResponseWriter, r *http.Request) {
	limit, offset := parsePagination(r)

	f, errMsg := parseAuditFilter(r)
	if errMsg != "" {
		writeError(w, http.StatusBadRequest, errMsg)
		return
	}

	logs, err := h.queries.ListAuditLogsFiltered(r.Context(), f.listParams(limit, offset))
	if err != nil {
		writeServerError(w, r, "failed to list audit logs", err)
		return
	}

	count, err := h.queries.CountAuditLogsFiltered(r.Context(), f.countParams())
	if err != nil {
		writeServerError(w, r, "failed to count audit logs", err)
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"items": toAuditLogDTOs(logs),
		"total": count,
	})
}

// ListAuditActions returns the distinct set of audit actions, for filter UIs.
// GET /api/audit-logs/actions
//
//apidoc:tag platform/audit-logs
//apidoc:title Get Actions
//apidoc:order 2
func (h *Handler) ListAuditActions(w http.ResponseWriter, r *http.Request) {
	actions, err := h.queries.ListDistinctAuditActions(r.Context())
	if err != nil {
		writeServerError(w, r, "failed to list audit actions", err)
		return
	}
	writeJSON(w, http.StatusOK, actions)
}

// ExportAuditLogs streams the filtered audit logs as CSV (admin-only). Honors the
// same filters as ListAuditLogs. GET /api/audit-logs/export
//
//apidoc:tag platform/audit-logs
//apidoc:title Export Logs
//apidoc:order 3
func (h *Handler) ExportAuditLogs(w http.ResponseWriter, r *http.Request) {
	f, errMsg := parseAuditFilter(r)
	if errMsg != "" {
		writeError(w, http.StatusBadRequest, errMsg)
		return
	}

	// Bound the export rather than streaming unboundedly.
	logs, err := h.queries.ListAuditLogsFiltered(r.Context(), f.listParams(10000, 0))
	if err != nil {
		writeServerError(w, r, "failed to export audit logs", err)
		return
	}

	w.Header().Set("Content-Type", "text/csv")
	w.Header().Set("Content-Disposition",
		fmt.Sprintf("attachment; filename=audit-log-%s.csv", time.Now().Format("2006-01-02")))

	cw := csv.NewWriter(w)
	defer cw.Flush()
	_ = cw.Write([]string{"time", "action", "actor", "token", "resource_type", "resource", "ip"})
	for _, l := range logs {
		resource := l.ResourceID.String
		if l.ResourceName != "" {
			resource = l.ResourceName
		}
		ts := ""
		if l.CreatedAt.Valid {
			ts = l.CreatedAt.Time.UTC().Format(time.RFC3339)
		}
		_ = cw.Write([]string{
			ts, l.Action, l.UserEmail.String, l.TokenName, l.ResourceType, resource, l.IpAddress.String,
		})
	}
}
