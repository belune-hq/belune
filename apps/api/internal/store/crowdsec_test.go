package store_test

import (
	"context"
	"errors"
	"net/netip"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/belune-hq/belune/internal/crowdsec"
	"github.com/belune-hq/belune/internal/store/generated"
)

func wafEvent(at time.Time, ip, category, tx string) generated.BulkInsertWAFEventsParams {
	row := wafRow(at, ip, 942100, tx)
	row.Category = pgtype.Text{String: category, Valid: category != ""}
	return row
}

func candidatesFor(t *testing.T, ctx context.Context, since time.Time, threshold int32, cats ...string) []generated.WAFAutobanCandidatesRow {
	t.Helper()
	got, err := testQueries.WAFAutobanCandidates(ctx, generated.WAFAutobanCandidatesParams{
		Since:      pgtype.Timestamptz{Time: since, Valid: true},
		Categories: cats,
		Threshold:  threshold,
		RowLimit:   50,
	})
	require.NoError(t, err)
	return got
}

func TestWAFAutobanCandidates_CountsDistinctRequestsNotEvents(t *testing.T) {
	ctx := resetWAF(t)
	now := time.Now()

	// One crafted request that trips twenty rules is ONE request.
	var rows []generated.BulkInsertWAFEventsParams
	for range 20 {
		rows = append(rows, wafEvent(now, "203.0.113.1", "attack-sqli", "one-request"))
	}
	// Twelve requests, one rule each.
	for i := range 12 {
		rows = append(rows, wafEvent(now, "203.0.113.2", "attack-sqli", "req-"+string(rune('a'+i))))
	}
	_, err := testQueries.BulkInsertWAFEvents(ctx, rows)
	require.NoError(t, err)

	got := candidatesFor(t, ctx, now.Add(-time.Hour), 10, "attack-sqli")
	require.Len(t, got, 1, "twenty events from one request must not reach a threshold of ten")
	assert.Equal(t, netip.MustParseAddr("203.0.113.2"), got[0].ClientIp)
	assert.EqualValues(t, 12, got[0].Requests)
}

func TestWAFAutobanCandidates_AnEventWithNoTransactionIsItsOwnRequest(t *testing.T) {
	ctx := resetWAF(t)
	now := time.Now()
	var rows []generated.BulkInsertWAFEventsParams
	for range 10 {
		rows = append(rows, wafEvent(now, "203.0.113.3", "attack-xss", ""))
	}
	_, err := testQueries.BulkInsertWAFEvents(ctx, rows)
	require.NoError(t, err)

	got := candidatesFor(t, ctx, now.Add(-time.Hour), 10, "attack-xss")
	require.Len(t, got, 1, "ten events with no tx_id are ten requests, not one NULL group")
	assert.EqualValues(t, 10, got[0].Requests)
}

func TestWAFAutobanCandidates_OnlyTheChosenCategoriesCount(t *testing.T) {
	ctx := resetWAF(t)
	now := time.Now()
	var rows []generated.BulkInsertWAFEventsParams
	for i := range 40 {
		tx := "p-" + string(rune('A'+i%26)) + string(rune('a'+i/26))
		// CRITICAL severity, like a real attack, and benign text/plain POSTs.
		rows = append(rows, wafEvent(now, "203.0.113.4", "attack-protocol", tx))
		rows = append(rows, wafEvent(now, "203.0.113.4", "", tx+"-anomaly"))
	}
	for i := range 10 {
		rows = append(rows, wafEvent(now, "203.0.113.5", "attack-xss", "x-"+string(rune('a'+i))))
	}
	_, err := testQueries.BulkInsertWAFEvents(ctx, rows)
	require.NoError(t, err)

	got := candidatesFor(t, ctx, now.Add(-time.Hour), 10, crowdsec.BanCategories...)
	require.Len(t, got, 1, "eighty protocol and uncategorised events must not make a ban")
	assert.Equal(t, netip.MustParseAddr("203.0.113.5"), got[0].ClientIp)
	assert.Equal(t, []string{"attack-xss"}, got[0].Categories)
}

func TestWAFAutobanCandidates_WindowThresholdAndOrder(t *testing.T) {
	ctx := resetWAF(t)
	now := time.Now()
	var rows []generated.BulkInsertWAFEventsParams
	add := func(ip string, n int, at time.Time, cat string) {
		for i := range n {
			rows = append(rows, wafEvent(at, ip, cat, ip+"-"+cat+"-"+at.Format("150405")+"-"+string(rune('a'+i))))
		}
	}
	add("203.0.113.10", 15, now, "attack-sqli")
	add("203.0.113.11", 30, now, "attack-sqli")
	add("203.0.113.11", 5, now, "attack-xss")
	add("203.0.113.12", 9, now, "attack-sqli")                    // under the threshold
	add("203.0.113.13", 50, now.Add(-2*time.Hour), "attack-sqli") // outside the window
	_, err := testQueries.BulkInsertWAFEvents(ctx, rows)
	require.NoError(t, err)

	got := candidatesFor(t, ctx, now.Add(-time.Hour), 10, "attack-sqli", "attack-xss")
	require.Len(t, got, 2)
	assert.Equal(t, netip.MustParseAddr("203.0.113.11"), got[0].ClientIp, "worst offender first")
	assert.EqualValues(t, 35, got[0].Requests)
	assert.Equal(t, []string{"attack-sqli", "attack-xss"}, got[0].Categories)
	assert.Equal(t, netip.MustParseAddr("203.0.113.10"), got[1].ClientIp)
}

func TestCrowdSecConfig_NoRowUntilFirstWrite(t *testing.T) {
	ctx := resetWAF(t)
	_, err := testQueries.GetCrowdSecConfig(ctx)
	assert.True(t, errors.Is(err, pgx.ErrNoRows))

	row, err := testQueries.SetCrowdSecEnabled(ctx, true)
	require.NoError(t, err)
	assert.True(t, row.Enabled)
	assert.False(t, row.AutobanEnabled, "automatic bans stay off unless asked for")
	assert.EqualValues(t, 10, row.AutobanThreshold)
	assert.NotContains(t, row.AutobanCategories, "attack-protocol")
	assert.Empty(t, row.BouncerKeyEncrypted)
}

func TestCrowdSecConfig_FirstKeyWins(t *testing.T) {
	ctx := resetWAF(t)

	first, err := testQueries.SetCrowdSecBouncerKeyIfUnset(ctx, []byte("first"))
	require.NoError(t, err)
	second, err := testQueries.SetCrowdSecBouncerKeyIfUnset(ctx, []byte("second"))
	require.NoError(t, err)
	assert.Equal(t, "first", string(first.BouncerKeyEncrypted))
	assert.Equal(t, "first", string(second.BouncerKeyEncrypted),
		"the loser must learn the winner's key, not store its own")

	rotated, err := testQueries.ReplaceCrowdSecBouncerKey(ctx, []byte("rotated"))
	require.NoError(t, err)
	assert.Equal(t, "rotated", string(rotated.BouncerKeyEncrypted))

	// Writing one column leaves the others alone.
	on, err := testQueries.SetCrowdSecEnabled(ctx, true)
	require.NoError(t, err)
	assert.Equal(t, "rotated", string(on.BouncerKeyEncrypted))
}

func TestCrowdSecConfig_TheDatabaseRefusesWhatCodeShould(t *testing.T) {
	ctx := resetWAF(t)
	good := generated.SetCrowdSecAutobanParams{
		AutobanEnabled: true, AutobanThreshold: 10, AutobanWindowMinutes: 10,
		AutobanDurationMinutes: 60, AutobanMaxActive: 25, AutobanCategories: []string{"attack-sqli"},
	}
	_, err := testQueries.SetCrowdSecAutoban(ctx, good)
	require.NoError(t, err)

	mutate := map[string]func(p *generated.SetCrowdSecAutobanParams){
		"protocol category": func(p *generated.SetCrowdSecAutobanParams) { p.AutobanCategories = []string{"attack-protocol"} },
		"scanner category": func(p *generated.SetCrowdSecAutobanParams) {
			p.AutobanCategories = []string{"attack-reputation-scanner"}
		},
		"threshold of one":    func(p *generated.SetCrowdSecAutobanParams) { p.AutobanThreshold = 1 },
		"permanent-ish ban":   func(p *generated.SetCrowdSecAutobanParams) { p.AutobanDurationMinutes = 100000 },
		"unbounded cap":       func(p *generated.SetCrowdSecAutobanParams) { p.AutobanMaxActive = 100000 },
		"window over one day": func(p *generated.SetCrowdSecAutobanParams) { p.AutobanWindowMinutes = 5000 },
	}
	for name, f := range mutate {
		p := good
		f(&p)
		_, err := testQueries.SetCrowdSecAutoban(ctx, p)
		assert.Error(t, err, name)
	}

	// The closed list in code and the CHECK in the schema must be the same list.
	for _, c := range crowdsec.BanCategories {
		p := good
		p.AutobanCategories = []string{c}
		_, err := testQueries.SetCrowdSecAutoban(ctx, p)
		assert.NoError(t, err, "%s is in BanCategories but the schema refuses it", c)
	}
}

func TestListCrowdSecUnbansSince(t *testing.T) {
	ctx := resetWAF(t)
	insert := func(action, resource string, ago time.Duration) {
		_, err := testPool.Exec(ctx,
			`INSERT INTO audit_logs (action, resource_type, resource_id, created_at) VALUES ($1, 'crowdsec', $2, $3)`,
			action, resource, time.Now().Add(-ago))
		require.NoError(t, err)
	}
	insert("crowdsec_unban", "203.0.113.9", 5*time.Minute)
	insert("crowdsec_unban", "203.0.113.9", 6*time.Minute)
	insert("crowdsec_unban", "198.51.100.0/24", time.Minute)
	insert("crowdsec_unban", "203.0.113.200", 3*time.Hour) // before the window
	insert("crowdsec_ban", "203.0.113.77", time.Minute)    // a ban is not an unban

	got, err := testQueries.ListCrowdSecUnbansSince(ctx, pgtype.Timestamptz{Time: time.Now().Add(-time.Hour), Valid: true})
	require.NoError(t, err)
	var vals []string
	for _, g := range got {
		vals = append(vals, g.String)
	}
	assert.ElementsMatch(t, []string{"203.0.113.9", "198.51.100.0/24"}, vals)
}
