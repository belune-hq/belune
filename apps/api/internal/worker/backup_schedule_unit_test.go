package worker

import (
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/belune-hq/belune/internal/store/generated"
)

func ts(t time.Time) pgtype.Timestamptz {
	return pgtype.Timestamptz{Time: t, Valid: true}
}

func TestBackupConfigDue(t *testing.T) {
	now := time.Date(2026, 6, 30, 12, 30, 0, 0, time.UTC)

	cases := []struct {
		name     string
		schedule string
		created  time.Time
		lastRun  pgtype.Timestamptz
		want     bool
	}{
		{
			name:     "hourly, last run two hours ago -> due",
			schedule: "0 * * * *",
			lastRun:  ts(now.Add(-2 * time.Hour)),
			want:     true,
		},
		{
			name:     "hourly, last run one minute ago -> not due",
			schedule: "0 * * * *",
			lastRun:  ts(now.Add(-1 * time.Minute)),
			want:     false,
		},
		{
			name:     "daily, last run yesterday -> due",
			schedule: "0 0 * * *",
			lastRun:  ts(now.Add(-25 * time.Hour)),
			want:     true,
		},
		{
			name:     "daily, never run, created an hour ago -> not due (next is tomorrow midnight)",
			schedule: "0 0 * * *",
			created:  now.Add(-1 * time.Hour),
			want:     false,
		},
		{
			name:     "never run, created before a passed occurrence -> due",
			schedule: "0 * * * *", // hourly at :00; 12:00 fired since created 11:00
			created:  now.Add(-90 * time.Minute),
			want:     true,
		},
		{
			name:     "invalid schedule -> not due",
			schedule: "not a cron",
			lastRun:  ts(now.Add(-48 * time.Hour)),
			want:     false,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			created := c.created
			if created.IsZero() {
				created = now.Add(-48 * time.Hour)
			}
			cfg := generated.DatabaseBackupConfig{
				ID:        pgtype.UUID{Bytes: [16]byte{1}, Valid: true},
				Schedule:  c.schedule,
				CreatedAt: ts(created),
				LastRunAt: c.lastRun,
			}
			if got := backupConfigDue(cfg, now); got != c.want {
				t.Fatalf("backupConfigDue(%q) = %v, want %v", c.schedule, got, c.want)
			}
		})
	}
}

func TestVolumeBackupConfigDue(t *testing.T) {
	now := time.Date(2026, 6, 30, 12, 30, 0, 0, time.UTC)

	cases := []struct {
		name     string
		schedule string
		created  time.Time
		lastRun  pgtype.Timestamptz
		want     bool
	}{
		{
			name:     "hourly, last run two hours ago -> due",
			schedule: "0 * * * *",
			lastRun:  ts(now.Add(-2 * time.Hour)),
			want:     true,
		},
		{
			name:     "hourly, last run one minute ago -> not due",
			schedule: "0 * * * *",
			lastRun:  ts(now.Add(-1 * time.Minute)),
			want:     false,
		},
		{
			name:     "daily, never run, created an hour ago -> not due",
			schedule: "0 0 * * *",
			created:  now.Add(-1 * time.Hour),
			want:     false,
		},
		{
			name:     "empty schedule (manual only) -> not due",
			schedule: "",
			lastRun:  ts(now.Add(-48 * time.Hour)),
			want:     false,
		},
		{
			name:     "invalid schedule -> not due",
			schedule: "not a cron",
			lastRun:  ts(now.Add(-48 * time.Hour)),
			want:     false,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			created := c.created
			if created.IsZero() {
				created = now.Add(-48 * time.Hour)
			}
			cfg := generated.ApplicationVolumeBackupConfig{
				ID:        pgtype.UUID{Bytes: [16]byte{2}, Valid: true},
				Schedule:  c.schedule,
				CreatedAt: ts(created),
				LastRunAt: c.lastRun,
			}
			if got := volumeBackupConfigDue(cfg, now); got != c.want {
				t.Fatalf("volumeBackupConfigDue(%q) = %v, want %v", c.schedule, got, c.want)
			}
		})
	}
}
