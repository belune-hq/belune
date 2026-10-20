package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/hibiken/asynq"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/belune-hq/belune/internal/notify"
)

// NotifyDeliverPayload is the task payload for TypeNotifyDeliver: one enqueued
// task per (channel, event) pair. The event travels fully-formed in the payload
// (link already absolutised) so the worker only has to load the channel's config
// and hand it to the provider.
type NotifyDeliverPayload struct {
	ChannelID string       `json:"channel_id"`
	Event     notify.Event `json:"event"`
}

// NewNotifyDeliverTask builds a TypeNotifyDeliver task on the low-priority queue
// with bounded retries — provider delivery is best-effort and must never crowd
// out deploys or backups.
func NewNotifyDeliverTask(channelID string, ev notify.Event) (*asynq.Task, error) {
	payload, err := json.Marshal(NotifyDeliverPayload{ChannelID: channelID, Event: ev})
	if err != nil {
		return nil, fmt.Errorf("marshal notify deliver payload: %w", err)
	}
	return asynq.NewTask(TypeNotifyDeliver, payload,
		asynq.MaxRetry(3),
		asynq.Queue("low"),
	), nil
}

// HandleNotifyDeliverTask delivers one event to one channel. Success stamps
// last_sent_at and clears last_error; a send failure is retried by asynq and,
// once retries are exhausted, stamped as last_error so the UI shows why. A
// skipped channel (disabled or deleted since dispatch) is silently dropped.
func (h *TaskHandler) HandleNotifyDeliverTask(ctx context.Context, t *asynq.Task) error {
	var p NotifyDeliverPayload
	if err := json.Unmarshal(t.Payload(), &p); err != nil {
		return fmt.Errorf("unmarshal notify deliver payload: %w", err)
	}
	if h.NotifyChannels == nil {
		return nil
	}

	var channelID pgtype.UUID
	if err := channelID.Scan(p.ChannelID); err != nil {
		// A malformed id can never succeed on retry — drop it.
		slog.Warn("notify: invalid channel id in deliver task, dropping", "channel_id", p.ChannelID)
		return nil
	}

	delivered, err := h.NotifyChannels.Deliver(ctx, channelID, p.Event)
	if err != nil {
		// A permanent failure (misconfigured/corrupt channel) can't recover on
		// retry — stamp it now and skip the backoff schedule. A transient failure
		// is stamped only once retries are exhausted, so a later success doesn't
		// leave a stale error on the row.
		permanent := errors.Is(err, notify.ErrPermanent)
		retried, _ := asynq.GetRetryCount(ctx)
		maxRetry, _ := asynq.GetMaxRetry(ctx)
		if permanent || retried >= maxRetry {
			if mErr := h.NotifyChannels.MarkError(ctx, channelID, err.Error(), p.Event.Type); mErr != nil {
				slog.Warn("notify: failed to record channel error", "channel_id", p.ChannelID, "error", mErr)
			}
		}
		wrapped := fmt.Errorf("deliver to channel %s: %w", p.ChannelID, err)
		if permanent {
			return errors.Join(wrapped, asynq.SkipRetry)
		}
		return wrapped
	}
	if delivered {
		if err := h.NotifyChannels.MarkSent(ctx, channelID, p.Event.Type); err != nil {
			slog.Warn("notify: failed to record channel send", "channel_id", p.ChannelID, "error", err)
		}
	}
	return nil
}

// dispatchToChannels fans a domain event out to every enabled channel subscribed
// to its type, enqueuing one delivery task each. Called once per event at the
// domain boundary (not per in-app recipient), so a single event never produces
// duplicate provider messages. Never blocks the caller and never fails the
// originating work — failures are logged and dropped.
func (h *TaskHandler) dispatchToChannels(ctx context.Context, ev notify.Event) {
	if h.NotifyChannels == nil || h.Enqueuer == nil {
		return
	}
	ev.Link = h.absoluteLink(ev.Link)

	channels, err := h.Queries.ListEnabledChannelsForEvent(ctx, ev.Type)
	if err != nil {
		slog.Warn("notify: failed to list channels for event", "type", ev.Type, "error", err)
		return
	}
	for _, ch := range channels {
		task, err := NewNotifyDeliverTask(formatUUID(ch.ID), ev)
		if err != nil {
			slog.Warn("notify: failed to build deliver task", "channel", ch.Name, "error", err)
			continue
		}
		if _, err := h.Enqueuer.Enqueue(task); err != nil {
			slog.Warn("notify: failed to enqueue deliver task", "channel", ch.Name, "error", err)
		}
	}
}

// absoluteLink turns a dashboard-relative path into an absolute URL using
// PUBLIC_BASE_URL. It returns "" when the base URL is unset (links are omitted
// rather than sent broken) or the path is already absolute-safe.
func (h *TaskHandler) absoluteLink(rel string) string {
	base := ""
	if h.Config != nil {
		base = strings.TrimRight(h.Config.PublicBaseURL, "/")
	}
	if rel == "" || base == "" {
		return ""
	}
	if strings.HasPrefix(rel, "http://") || strings.HasPrefix(rel, "https://") {
		return rel
	}
	if !strings.HasPrefix(rel, "/") {
		rel = "/" + rel
	}
	return base + rel
}
