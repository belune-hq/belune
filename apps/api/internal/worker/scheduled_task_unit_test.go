package worker

import (
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/stretchr/testify/assert"

	"github.com/weiliang79/belune/internal/store/generated"
)

func TestScheduledTaskDue(t *testing.T) {
	now := time.Date(2026, 6, 30, 12, 30, 0, 0, time.UTC)
	mk := func(cron, tz string, created time.Time, last pgtype.Timestamptz) generated.ApplicationScheduledTask {
		return generated.ApplicationScheduledTask{
			Cron: cron, Timezone: tz,
			CreatedAt: pgtype.Timestamptz{Time: created, Valid: true}, LastRunAt: last,
		}
	}

	t.Run("new task is baselined at created_at, not fired immediately", func(t *testing.T) {
		// Created 12:05 with a 03:00 schedule: next is tomorrow.
		assert.False(t, scheduledTaskDue(mk("0 3 * * *", "UTC", now.Add(-25*time.Minute), pgtype.Timestamptz{}), now))
	})

	t.Run("fires once after downtime, never a herd", func(t *testing.T) {
		// Hourly task, last ran 6h ago: due now. After the sweep stamps now, it
		// is not due again — one catch-up run, not six.
		task := mk("0 * * * *", "UTC", now.Add(-48*time.Hour), ts(now.Add(-6*time.Hour)))
		assert.True(t, scheduledTaskDue(task, now))
		task.LastRunAt = ts(now)
		assert.False(t, scheduledTaskDue(task, now))
	})

	t.Run("schedule is evaluated in the task's timezone", func(t *testing.T) {
		// 20:00 UTC on the 29th: 03:00 MYT on the 30th is 19:00 UTC on the 29th.
		last := ts(time.Date(2026, 6, 29, 18, 0, 0, 0, time.UTC))
		at := time.Date(2026, 6, 29, 19, 0, 30, 0, time.UTC)
		created := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
		assert.True(t, scheduledTaskDue(mk("0 3 * * *", "Asia/Kuala_Lumpur", created, last), at))
		assert.False(t, scheduledTaskDue(mk("0 3 * * *", "UTC", created, last), at))
	})

	t.Run("an invalid stored schedule is not due", func(t *testing.T) {
		assert.False(t, scheduledTaskDue(mk("garbage", "UTC", now.Add(-time.Hour), pgtype.Timestamptz{}), now))
		assert.False(t, scheduledTaskDue(mk("* * * * *", "Mars/Olympus", now.Add(-time.Hour), pgtype.Timestamptz{}), now))
	})
}

func TestCappedOutputKeepsHeadAndTail(t *testing.T) {
	c := newCappedOutput(20)
	_, _ = c.Write([]byte(strings.Repeat("a", 10)))
	_, _ = c.Write([]byte(strings.Repeat("b", 30)))
	_, _ = c.Write([]byte(strings.Repeat("c", 10)))
	got := c.String()
	assert.True(t, strings.HasPrefix(got, strings.Repeat("a", 10)), got)
	assert.True(t, strings.HasSuffix(got, strings.Repeat("c", 10)), got)
	assert.Contains(t, got, "30 bytes omitted")

	small := newCappedOutput(1024)
	_, _ = small.Write([]byte("hello"))
	assert.Equal(t, "hello", small.String())
}
