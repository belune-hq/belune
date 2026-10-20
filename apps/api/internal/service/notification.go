package service

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/redis/go-redis/v9"

	"github.com/belune-hq/belune/internal/store/generated"
)

const (
	notificationBufferSize = 256
	// notificationEnqueueTimeout caps how long Notify() waits for buffer space
	// before falling back to a direct insert, so a slow consumer turns into
	// brief caller back-pressure rather than data loss.
	notificationEnqueueTimeout = 50 * time.Millisecond
	// notificationDrainTimeout bounds the synchronous drain on shutdown.
	notificationDrainTimeout = 5 * time.Second
	// notificationSyncInsertTimeout bounds direct-insert fallbacks.
	notificationSyncInsertTimeout = 2 * time.Second
)

// NotificationChannel is the Redis pub/sub channel a user's live SSE stream
// subscribes to. One channel per recipient keeps fan-out trivial.
func NotificationChannel(userID string) string {
	return "notifications:user:" + userID
}

// notificationEntry is the internal buffered-channel type.
type notificationEntry struct {
	UserID string
	Type   string
	Title  string
	Body   string
	Link   string
}

// NotificationService provides non-blocking, persist-then-push notifications.
// It mirrors AuditService: a buffered channel drained by a single goroutine.
// The DB is the source of truth (persisted before push); the live SSE push is
// best-effort, and the frontend reconciles from the DB on mount.
//
// Audit and notifications are PARALLEL siblings off a domain event, never
// chained — callers emit to both independently.
type NotificationService struct {
	queries *generated.Queries
	rdb     *redis.Client
	ch      chan notificationEntry
}

func NewNotificationService(queries *generated.Queries, rdb *redis.Client) *NotificationService {
	return &NotificationService{
		queries: queries,
		rdb:     rdb,
		ch:      make(chan notificationEntry, notificationBufferSize),
	}
}

// Run drains the channel and persists entries. Call in a goroutine; stops when
// ctx is cancelled (draining remaining entries on a bounded context first).
func (s *NotificationService) Run(ctx context.Context) {
	for {
		select {
		case entry, ok := <-s.ch:
			if !ok {
				return
			}
			s.insert(ctx, entry)
		case <-ctx.Done():
			drainCtx, cancel := context.WithTimeout(context.Background(), notificationDrainTimeout)
			defer cancel()
			for {
				select {
				case entry, ok := <-s.ch:
					if !ok {
						return
					}
					s.insert(drainCtx, entry)
				case <-drainCtx.Done():
					slog.Warn("notify: drain timeout exceeded, dropping remaining entries",
						"buffered", len(s.ch))
					return
				default:
					return
				}
			}
		}
	}
}

func (s *NotificationService) insert(ctx context.Context, entry notificationEntry) {
	var userUUID pgtype.UUID
	if err := userUUID.Scan(entry.UserID); err != nil {
		slog.Warn("notify: invalid recipient id, dropping", "user_id", entry.UserID)
		return
	}

	row, err := s.queries.CreateNotification(ctx, generated.CreateNotificationParams{
		UserID: userUUID,
		Type:   entry.Type,
		Title:  entry.Title,
		Body:   entry.Body,
		Link:   pgtype.Text{String: entry.Link, Valid: entry.Link != ""},
	})
	if err != nil {
		slog.Warn("notify: failed to insert notification", "type", entry.Type, "error", err)
		return
	}

	// Best-effort live push. The row is already persisted, so a publish failure
	// only delays delivery until the recipient's next page load.
	if s.rdb != nil {
		if payload, mErr := json.Marshal(row); mErr == nil {
			if pErr := s.rdb.Publish(ctx, NotificationChannel(entry.UserID), payload).Err(); pErr != nil {
				slog.Debug("notify: live publish failed", "user_id", entry.UserID, "error", pErr)
			}
		}
	}
}

// Notify queues a notification for one recipient. Non-blocking with the same
// back-pressure handling as AuditService.Log: try, brief wait, then a bounded
// synchronous insert so notifications are not silently lost.
func (s *NotificationService) Notify(userID, notifType, title, body, link string) {
	if userID == "" {
		return
	}
	entry := notificationEntry{
		UserID: userID,
		Type:   notifType,
		Title:  title,
		Body:   body,
		Link:   link,
	}

	select {
	case s.ch <- entry:
		return
	default:
	}

	timer := time.NewTimer(notificationEnqueueTimeout)
	defer timer.Stop()
	select {
	case s.ch <- entry:
		return
	case <-timer.C:
	}

	slog.Warn("notify: channel full, writing synchronously", "type", notifType)
	ctx, cancel := context.WithTimeout(context.Background(), notificationSyncInsertTimeout)
	defer cancel()
	s.insert(ctx, entry)
}
