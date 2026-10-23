package waf

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/belune-hq/belune/internal/store/generated"
)

const (
	// SettingRetentionDays names the setting that overrides DefaultRetentionDays.
	SettingRetentionDays = "waf_events_retention_days"

	DefaultRetentionDays = 7
	maxRetentionDays     = 365

	// MaxEvents caps the table whatever its age. A detection table grows without
	// bound exactly when the install is under attack, which is the worst moment for
	// it to fill the disk — and age alone does not prevent that: a flood can write
	// more in an hour than a week of ordinary traffic would. At roughly 150 bytes a
	// row this is a few tens of megabytes.
	MaxEvents = 200_000
)

// SweepStore is the slice of the database the sweep uses.
type SweepStore interface {
	GetSetting(ctx context.Context, key string) (generated.Setting, error)
	DeleteOldWAFEvents(ctx context.Context, before pgtype.Timestamptz) (int64, error)
	TrimWAFEvents(ctx context.Context, keep int32) (int64, error)
}

var _ SweepStore = (*generated.Queries)(nil)

// RetentionDays is how long events are kept: the setting when it holds a sane
// number, the default otherwise. A bad value must not turn into "keep nothing" or
// "keep for ever".
func RetentionDays(ctx context.Context, q interface {
	GetSetting(ctx context.Context, key string) (generated.Setting, error)
}) int {
	s, err := q.GetSetting(ctx, SettingRetentionDays)
	if err != nil {
		return DefaultRetentionDays
	}
	days, err := strconv.Atoi(strings.TrimSpace(s.Value))
	if err != nil || days < 1 || days > maxRetentionDays {
		return DefaultRetentionDays
	}
	return days
}

// Sweep deletes events past their retention and then trims the table to MaxEvents,
// newest kept. Both limits apply: whichever is hit first wins.
func Sweep(ctx context.Context, q SweepStore, now time.Time) (aged, trimmed int64, err error) {
	days := RetentionDays(ctx, q)
	cutoff := pgtype.Timestamptz{Time: now.Add(-time.Duration(days) * 24 * time.Hour), Valid: true}
	if aged, err = q.DeleteOldWAFEvents(ctx, cutoff); err != nil {
		return 0, 0, fmt.Errorf("deleting events past their retention: %w", err)
	}
	if trimmed, err = q.TrimWAFEvents(ctx, MaxEvents); err != nil {
		return aged, 0, fmt.Errorf("trimming events to the row cap: %w", err)
	}
	return aged, trimmed, nil
}
