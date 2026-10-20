package service

import (
	"context"
	"encoding/json"
	"log/slog"
	"regexp"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/belune-hq/belune/internal/pkg/redact"
	"github.com/belune-hq/belune/internal/store/generated"
)

const (
	auditBufferSize = 256
	// auditEnqueueTimeout caps how long Log() will wait for buffer space
	// before falling back to a direct DB insert. Keeps the caller from
	// blocking the request indefinitely under back-pressure.
	auditEnqueueTimeout = 50 * time.Millisecond
	// auditDrainTimeout bounds the synchronous drain when Run() exits so
	// shutdown doesn't hang on a slow database.
	auditDrainTimeout = 5 * time.Second
	// auditSyncInsertTimeout bounds direct-insert fallbacks invoked from
	// Log() when the buffer is full.
	auditSyncInsertTimeout = 2 * time.Second
)

// sensitiveDetailKey matches map keys that should never be persisted in
// audit_logs.details. Match is case-insensitive on the whole key. Compared
// against keys, not values — value-side scrubbing is handled by redact.Error
// applied to free-text strings.
var sensitiveDetailKey = regexp.MustCompile(`(?i)password|token|secret|credential|signature|api[_-]?key|webhook[_-]?secret|authorization|cookie`)

// auditEntry is an internal type for the buffered channel.
type auditEntry struct {
	UserID       string
	TokenID      string
	Action       string
	ResourceType string
	ResourceID   string
	Details      map[string]any
	IPAddress    string
}

// AuditService provides non-blocking audit log capture.
// It writes entries to a buffered channel and a single goroutine drains them to the DB.
type AuditService struct {
	queries *generated.Queries
	ch      chan auditEntry
}

func NewAuditService(queries *generated.Queries) *AuditService {
	return &AuditService{
		queries: queries,
		ch:      make(chan auditEntry, auditBufferSize),
	}
}

// Run drains the audit channel and inserts entries to the database.
// Must be called in a goroutine. Stops when ctx is cancelled (drains remaining entries first).
func (s *AuditService) Run(ctx context.Context) {
	for {
		select {
		case entry, ok := <-s.ch:
			if !ok {
				return
			}
			s.insert(ctx, entry)
		case <-ctx.Done():
			// Drain remaining entries on a bounded shutdown context so a
			// stalled DB cannot hold up process exit indefinitely.
			drainCtx, cancel := context.WithTimeout(context.Background(), auditDrainTimeout)
			defer cancel()
			for {
				select {
				case entry, ok := <-s.ch:
					if !ok {
						return
					}
					s.insert(drainCtx, entry)
				case <-drainCtx.Done():
					slog.Warn("audit: drain timeout exceeded, dropping remaining entries",
						"buffered", len(s.ch))
					return
				default:
					return
				}
			}
		}
	}
}

func (s *AuditService) insert(ctx context.Context, entry auditEntry) {
	var userUUID pgtype.UUID
	if entry.UserID != "" {
		_ = userUUID.Scan(entry.UserID)
	}

	var tokenUUID pgtype.UUID
	if entry.TokenID != "" {
		_ = tokenUUID.Scan(entry.TokenID)
	}

	var detailsJSON []byte
	if entry.Details != nil {
		detailsJSON, _ = json.Marshal(entry.Details)
	}

	err := s.queries.CreateAuditLog(ctx, generated.CreateAuditLogParams{
		UserID:       userUUID,
		Action:       entry.Action,
		ResourceType: entry.ResourceType,
		ResourceID:   pgtype.Text{String: entry.ResourceID, Valid: entry.ResourceID != ""},
		Details:      detailsJSON,
		IpAddress:    pgtype.Text{String: entry.IPAddress, Valid: entry.IPAddress != ""},
		TokenID:      tokenUUID,
	})
	if err != nil {
		slog.Warn("audit: failed to insert log", "action", entry.Action, "error", err)
	}
}

// Log sends an audit entry to the buffered channel.
//
// Back-pressure handling: a non-blocking send is tried first; if the buffer
// is full, the caller waits up to auditEnqueueTimeout for space; if still
// full, the entry is written synchronously to the DB on a bounded context.
// Audit entries must never silently disappear, so a slow consumer turns into
// caller back-pressure rather than data loss.
//
// Caller provides userID, tokenID, and ipAddress directly (avoids import
// cycle with middleware). tokenID is "" for a session JWT — it cannot be
// backfilled onto rows written before api_tokens existed, so an empty string
// here correctly means "this was a human action", not "unknown".
func (s *AuditService) Log(userID, tokenID, ipAddress, action, resourceType, resourceID string, details map[string]any) {
	entry := auditEntry{
		UserID:       userID,
		TokenID:      tokenID,
		Action:       action,
		ResourceType: resourceType,
		ResourceID:   resourceID,
		Details:      redactDetails(details),
		IPAddress:    ipAddress,
	}

	select {
	case s.ch <- entry:
		return
	default:
	}

	// Buffer full — wait briefly for space before falling back.
	timer := time.NewTimer(auditEnqueueTimeout)
	defer timer.Stop()
	select {
	case s.ch <- entry:
		return
	case <-timer.C:
	}

	// Fallback: synchronous write so the audit record is preserved.
	slog.Warn("audit: channel full, writing synchronously", "action", action)
	ctx, cancel := context.WithTimeout(context.Background(), auditSyncInsertTimeout)
	defer cancel()
	s.insert(ctx, entry)
}

// redactDetails returns a deep-copy of details with sensitive keys masked
// and free-text string values run through redact.Error so any embedded
// credentials in URLs / headers are scrubbed before persistence.
//
// Redaction happens at write time (not just at log time) so the audit_logs
// table itself is safe to dump or replicate without re-leaking secrets.
func redactDetails(in map[string]any) map[string]any {
	if in == nil {
		return nil
	}
	out := make(map[string]any, len(in))
	for k, v := range in {
		if sensitiveDetailKey.MatchString(k) {
			out[k] = "[REDACTED]"
			continue
		}
		switch tv := v.(type) {
		case string:
			out[k] = redact.Error(tv)
		case map[string]any:
			out[k] = redactDetails(tv)
		default:
			out[k] = v
		}
	}
	return out
}
