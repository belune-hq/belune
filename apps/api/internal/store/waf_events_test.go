package store_test

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
)

func wafRow(at time.Time, ip string, rule int32, tx string) generated.BulkInsertWAFEventsParams {
	return generated.BulkInsertWAFEventsParams{
		RecordedAt: pgtype.Timestamptz{Time: at, Valid: true},
		ClientIp:   netip.MustParseAddr(ip),
		RuleID:     rule,
		Severity:   "critical",
		Message:    "rule message",
		TxID:       pgtype.Text{String: tx, Valid: tx != ""},
	}
}

func resetWAF(t *testing.T) context.Context {
	t.Helper()
	t.Cleanup(func() { _ = testutil.TruncateAll(context.Background(), testPool) })
	require.NoError(t, testutil.TruncateAll(context.Background(), testPool))
	return context.Background()
}

func TestWAFEvents_InsertAndPage(t *testing.T) {
	ctx := resetWAF(t)
	now := time.Now()

	rows := make([]generated.BulkInsertWAFEventsParams, 0, 25)
	for i := 0; i < 25; i++ {
		rows = append(rows, wafRow(now.Add(time.Duration(i)*time.Second), "203.0.113.9", 941100, "tx"))
	}
	n, err := testQueries.BulkInsertWAFEvents(ctx, rows)
	require.NoError(t, err)
	assert.EqualValues(t, 25, n)

	// Keyset pagination: newest first, each page resumes strictly below the last id,
	// so no row repeats or is skipped even while the table is being written to.
	seen := map[int64]bool{}
	var before pgtype.Int8
	for page := 0; page < 3; page++ {
		got, err := testQueries.ListWAFEvents(ctx, generated.ListWAFEventsParams{BeforeID: before, RowLimit: 10})
		require.NoError(t, err)
		for i, e := range got {
			assert.False(t, seen[e.ID], "event %d appeared twice", e.ID)
			seen[e.ID] = true
			if i > 0 {
				assert.Less(t, e.ID, got[i-1].ID, "newest first")
			}
		}
		if len(got) < 10 {
			assert.Equal(t, 5, len(got), "the last page holds the remainder")
			break
		}
		before = pgtype.Int8{Int64: got[len(got)-1].ID, Valid: true}
	}
	assert.Len(t, seen, 25)
}

func TestWAFEvents_FilterByClientAndRule(t *testing.T) {
	ctx := resetWAF(t)
	now := time.Now()
	_, err := testQueries.BulkInsertWAFEvents(ctx, []generated.BulkInsertWAFEventsParams{
		wafRow(now, "203.0.113.9", 941100, "a"),
		wafRow(now, "203.0.113.9", 942100, "a"),
		wafRow(now, "198.51.100.7", 941100, "b"),
		wafRow(now, "2001:db8::1", 941100, "c"),
	})
	require.NoError(t, err)

	ip := netip.MustParseAddr("203.0.113.9")
	got, err := testQueries.ListWAFEvents(ctx, generated.ListWAFEventsParams{ClientIp: &ip, RowLimit: 10})
	require.NoError(t, err)
	assert.Len(t, got, 2, "an address filter is exact")

	got, err = testQueries.ListWAFEvents(ctx, generated.ListWAFEventsParams{RuleID: pgtype.Int4{Int32: 941100, Valid: true}, RowLimit: 10})
	require.NoError(t, err)
	assert.Len(t, got, 3)

	v6 := netip.MustParseAddr("2001:db8::1")
	got, err = testQueries.ListWAFEvents(ctx, generated.ListWAFEventsParams{ClientIp: &v6, RowLimit: 10})
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, v6, got[0].ClientIp, "IPv6 round-trips")
}

func TestWAFEvents_SummaryHandlesRulesWithNoCategory(t *testing.T) {
	ctx := resetWAF(t)
	now := time.Now()
	withCat := wafRow(now, "203.0.113.9", 941100, "a")
	withCat.Category = pgtype.Text{String: "attack-xss", Valid: true}
	noCat := wafRow(now, "198.51.100.7", 949110, "a") // the anomaly-score rule carries no attack-* tag
	noCat.Severity = "unknown"
	_, err := testQueries.BulkInsertWAFEvents(ctx, []generated.BulkInsertWAFEventsParams{
		withCat, withCat, noCat, wafRow(now.Add(-48*time.Hour), "192.0.2.1", 1, "old"),
	})
	require.NoError(t, err)

	since := pgtype.Timestamptz{Time: now.Add(-time.Hour), Valid: true}
	totals, err := testQueries.WAFEventTotals(ctx, since)
	require.NoError(t, err)
	assert.EqualValues(t, 3, totals.Events, "the old row is outside the window")
	assert.EqualValues(t, 2, totals.Clients)
	assert.EqualValues(t, 1, totals.Requests, "rules sharing a tx id are one request")

	rules, err := testQueries.TopWAFRules(ctx, generated.TopWAFRulesParams{Since: since, RowLimit: 5})
	require.NoError(t, err, "a rule with no category must not break the aggregation")
	require.Len(t, rules, 2)
	assert.EqualValues(t, 941100, rules[0].RuleID)
	assert.EqualValues(t, 2, rules[0].Events)
	assert.Equal(t, "attack-xss", rules[0].Category)
	assert.Equal(t, "", rules[1].Category)

	clients, err := testQueries.TopWAFClients(ctx, generated.TopWAFClientsParams{Since: since, RowLimit: 5})
	require.NoError(t, err)
	require.Len(t, clients, 2)
	assert.Equal(t, netip.MustParseAddr("203.0.113.9"), clients[0].ClientIp)
}

func TestWAFEvents_RetentionAndRowCap(t *testing.T) {
	ctx := resetWAF(t)
	now := time.Now()
	rows := []generated.BulkInsertWAFEventsParams{
		wafRow(now.Add(-10*24*time.Hour), "203.0.113.9", 1, "x"),
		wafRow(now.Add(-8*24*time.Hour), "203.0.113.9", 2, "x"),
		wafRow(now.Add(-1*time.Hour), "203.0.113.9", 3, "x"),
	}
	for i := 0; i < 7; i++ {
		rows = append(rows, wafRow(now, "203.0.113.9", int32(10+i), "x"))
	}
	_, err := testQueries.BulkInsertWAFEvents(ctx, rows)
	require.NoError(t, err)

	aged, err := testQueries.DeleteOldWAFEvents(ctx, pgtype.Timestamptz{Time: now.Add(-7 * 24 * time.Hour), Valid: true})
	require.NoError(t, err)
	assert.EqualValues(t, 2, aged)

	// Fewer rows than the cap: nothing is trimmed.
	trimmed, err := testQueries.TrimWAFEvents(ctx, 100)
	require.NoError(t, err)
	assert.Zero(t, trimmed)

	// Over the cap: the newest `keep` rows survive, whatever their age.
	trimmed, err = testQueries.TrimWAFEvents(ctx, 5)
	require.NoError(t, err)
	assert.EqualValues(t, 3, trimmed)
	left, err := testQueries.ListWAFEvents(ctx, generated.ListWAFEventsParams{RowLimit: 50})
	require.NoError(t, err)
	require.Len(t, left, 5)
	for _, e := range left {
		assert.GreaterOrEqual(t, e.RuleID, int32(12), "the oldest rows were the ones removed")
	}
}

func TestWAFEvents_SeverityIsConstrained(t *testing.T) {
	ctx := resetWAF(t)
	bad := wafRow(time.Now(), "203.0.113.9", 1, "x")
	bad.Severity = "apocalyptic"
	_, err := testQueries.BulkInsertWAFEvents(ctx, []generated.BulkInsertWAFEventsParams{bad})
	require.Error(t, err, "the table refuses a severity the parser would not produce")

	unknown := wafRow(time.Now(), "203.0.113.9", 949110, "x")
	unknown.Severity = "unknown" // what Coraza reports for the anomaly-score summary rule
	_, err = testQueries.BulkInsertWAFEvents(ctx, []generated.BulkInsertWAFEventsParams{unknown})
	require.NoError(t, err)
}

func TestWAFEvents_ApplicationAttributionSurvivesAnApplicationDelete(t *testing.T) {
	ctx := resetWAF(t)
	user, err := testQueries.CreateUser(ctx, generated.CreateUserParams{Email: "waf@test.com", PasswordHash: "h", Role: "admin"})
	require.NoError(t, err)
	proj, err := testQueries.CreateProject(ctx, generated.CreateProjectParams{
		Name: "p", Slug: "p", UserID: user.ID, ServerID: testutil.LocalServerID(t, ctx, testQueries),
	})
	require.NoError(t, err)
	app, err := testQueries.CreateApplication(ctx, generated.CreateApplicationParams{
		ProjectID: proj.ID, Name: "a", Slug: "a", Type: "image", BuildType: "image",
	})
	require.NoError(t, err)

	row := wafRow(time.Now(), "203.0.113.9", 941100, "x")
	row.ApplicationID = app.ID
	row.Host = pgtype.Text{String: "shop.example.com", Valid: true}
	_, err = testQueries.BulkInsertWAFEvents(ctx, []generated.BulkInsertWAFEventsParams{row})
	require.NoError(t, err)

	_, err = testPool.Exec(ctx, `DELETE FROM applications WHERE id = $1`, app.ID)
	require.NoError(t, err, "an application with detection events can still be deleted")

	got, err := testQueries.ListWAFEvents(ctx, generated.ListWAFEventsParams{RowLimit: 10})
	require.NoError(t, err)
	require.Len(t, got, 1, "the event outlives the application")
	assert.False(t, got[0].ApplicationID.Valid, "attribution degrades to NULL")
	assert.Equal(t, "shop.example.com", got[0].Host.String, "and the host is still known")
}

// COPY fails whole, so what the tables refuse is what a batch can lose: the
// parser has to stay inside these, and these have to refuse what it must not
// produce.
func TestWAFEvents_DispositionAndScoreAreConstrained(t *testing.T) {
	ctx := resetWAF(t)
	insert := func(mut func(*generated.BulkInsertWAFEventsParams)) error {
		r := wafRow(time.Now(), "203.0.113.9", 949110, "x")
		mut(&r)
		_, err := testQueries.BulkInsertWAFEvents(ctx, []generated.BulkInsertWAFEventsParams{r})
		return err
	}
	text := func(s string) pgtype.Text { return pgtype.Text{String: s, Valid: true} }

	require.NoError(t, insert(func(*generated.BulkInsertWAFEventsParams) {}), "both columns are nullable")
	require.NoError(t, insert(func(r *generated.BulkInsertWAFEventsParams) {
		r.Disposition = text("blocked")
		r.AnomalyScore = pgtype.Int4{Int32: 999999, Valid: true}
	}))
	require.NoError(t, insert(func(r *generated.BulkInsertWAFEventsParams) { r.Disposition = text("detected") }))
	require.Error(t, insert(func(r *generated.BulkInsertWAFEventsParams) { r.Disposition = text("denied") }))
	require.Error(t, insert(func(r *generated.BulkInsertWAFEventsParams) {
		r.AnomalyScore = pgtype.Int4{Int32: 1000000, Valid: true}
	}))
	require.Error(t, insert(func(r *generated.BulkInsertWAFEventsParams) { r.AnomalyScore = pgtype.Int4{Int32: -1, Valid: true} }))
}
