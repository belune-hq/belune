package worker_test

import (
	"context"
	"net/netip"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/belune-hq/belune/internal/store/generated"
	"github.com/belune-hq/belune/internal/testutil"
	"github.com/belune-hq/belune/internal/waf"
	"github.com/belune-hq/belune/internal/worker"
)

func wafSweepRows(n int, age time.Duration) []generated.BulkInsertWAFEventsParams {
	rows := make([]generated.BulkInsertWAFEventsParams, n)
	for i := range rows {
		rows[i] = generated.BulkInsertWAFEventsParams{
			RecordedAt: pgtype.Timestamptz{Time: time.Now().Add(-age), Valid: true},
			ClientIp:   netip.MustParseAddr("203.0.113.9"),
			RuleID:     941100,
			Severity:   "critical",
			Message:    "m",
		}
	}
	return rows
}

func countWAF(t *testing.T) int64 {
	t.Helper()
	n, err := testQueries.CountWAFEvents(context.Background())
	require.NoError(t, err)
	return n
}

func TestWAFEventsSweep_DropsWhatIsPastItsRetention(t *testing.T) {
	ctx := context.Background()
	t.Cleanup(func() { _ = testutil.TruncateAll(ctx, testPool) })
	require.NoError(t, testutil.TruncateAll(ctx, testPool))
	h := &worker.TaskHandler{Queries: testQueries}

	_, err := testQueries.BulkInsertWAFEvents(ctx, append(wafSweepRows(4, 10*24*time.Hour), wafSweepRows(3, time.Hour)...))
	require.NoError(t, err)

	h.HandleWAFEventsSweep(ctx)
	assert.EqualValues(t, 3, countWAF(t), "the default is a week")
}

func TestWAFEventsSweep_RetentionIsASetting_AndABadValueIsNotTrusted(t *testing.T) {
	ctx := context.Background()
	t.Cleanup(func() {
		_ = testutil.TruncateAll(ctx, testPool)
		_, _ = testPool.Exec(ctx, `DELETE FROM settings WHERE key = $1`, waf.SettingRetentionDays)
	})
	require.NoError(t, testutil.TruncateAll(ctx, testPool))
	h := &worker.TaskHandler{Queries: testQueries}

	set := func(v string) {
		_, err := testQueries.UpsertSetting(ctx, generated.UpsertSettingParams{Key: waf.SettingRetentionDays, Value: v})
		require.NoError(t, err)
	}
	seed := func() {
		_, err := testQueries.BulkInsertWAFEvents(ctx, append(wafSweepRows(2, 40*24*time.Hour), wafSweepRows(2, 2*24*time.Hour)...))
		require.NoError(t, err)
	}

	set("1")
	seed()
	h.HandleWAFEventsSweep(ctx)
	assert.EqualValues(t, 0, countWAF(t), "a one-day retention removes rows two days old")

	set("90")
	seed()
	h.HandleWAFEventsSweep(ctx)
	assert.EqualValues(t, 4, countWAF(t), "a 90-day retention keeps the 40-day-old rows")

	// A value that is not a sane number must not become "keep nothing" or "keep for
	// ever": it falls back to the default.
	require.NoError(t, testutil.TruncateAll(ctx, testPool))
	for _, bad := range []string{"0", "-5", "abc", "99999", ""} {
		set(bad)
		assert.Equal(t, waf.DefaultRetentionDays, waf.RetentionDays(ctx, testQueries), "setting %q", bad)
	}
}

// The reason age is not enough: a flood writes more in an hour than a week of
// ordinary traffic would, and it is the moment the disk can least afford it.
func TestWAFEventsSweep_ACapHoldsWhateverTheAge(t *testing.T) {
	ctx := context.Background()
	t.Cleanup(func() { _ = testutil.TruncateAll(ctx, testPool) })
	require.NoError(t, testutil.TruncateAll(ctx, testPool))

	// Insert a little over the cap, all of it recent, in one statement's worth of rows.
	_, err := testPool.Exec(ctx, `
		INSERT INTO waf_events (recorded_at, client_ip, rule_id, severity, message)
		SELECT now(), '203.0.113.9', 941100, 'critical', 'm' FROM generate_series(1, $1)`, waf.MaxEvents+137)
	require.NoError(t, err)
	require.EqualValues(t, waf.MaxEvents+137, countWAF(t))

	aged, trimmed, err := waf.Sweep(ctx, testQueries, time.Now())
	require.NoError(t, err)
	assert.Zero(t, aged, "none of it is old")
	assert.EqualValues(t, 137, trimmed)
	assert.EqualValues(t, waf.MaxEvents, countWAF(t))

	// Idempotent: a second pass has nothing to do.
	_, trimmed, err = waf.Sweep(ctx, testQueries, time.Now())
	require.NoError(t, err)
	assert.Zero(t, trimmed)
}
