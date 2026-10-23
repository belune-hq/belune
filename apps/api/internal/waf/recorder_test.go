package waf

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/belune-hq/belune/internal/store/generated"
)

// fakeStore records what the recorder writes and answers host lookups from a map.
type fakeStore struct {
	mu      sync.Mutex
	rows    []generated.BulkInsertWAFEventsParams
	apps    map[string][]pgtype.UUID
	lookups map[string]int
	insErr  error
	lookErr error
}

func (f *fakeStore) BulkInsertWAFEvents(_ context.Context, rows []generated.BulkInsertWAFEventsParams) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.insErr != nil {
		return 0, f.insErr
	}
	f.rows = append(f.rows, rows...)
	return int64(len(rows)), nil
}

func (f *fakeStore) ListApplicationIDsForHost(_ context.Context, host string) ([]pgtype.UUID, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.lookups == nil {
		f.lookups = map[string]int{}
	}
	f.lookups[host]++
	if f.lookErr != nil {
		return nil, f.lookErr
	}
	return f.apps[host], nil
}

func (f *fakeStore) written() []generated.BulkInsertWAFEventsParams {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]generated.BulkInsertWAFEventsParams(nil), f.rows...)
}

func uuidOf(b byte) pgtype.UUID { return pgtype.UUID{Bytes: [16]byte{15: b}, Valid: true} }

func newTestRecorder(f *fakeStore) *Recorder {
	r := NewRecorder(f)
	r.now = func() time.Time { return time.Unix(1792742400, 0) } // the fixtures' "now"
	r.resolver.now = r.now
	return r
}

func TestRecorder_StoresWhatTheParserKept(t *testing.T) {
	f := &fakeStore{apps: map[string][]pgtype.UUID{"app.test": {uuidOf(7)}}}
	r := newTestRecorder(f)

	for _, l := range fixtureLines(t) {
		r.HandleCaddyLine(context.Background(), l)
	}
	r.flush(context.Background(), r.drain())

	rows := f.written()
	require.Len(t, rows, 6)
	assert.Equal(t, int64(6), r.Stats().Recorded)
	assert.Zero(t, r.Stats().Malformed)

	row := rows[0]
	assert.Equal(t, "127.0.0.1", row.ClientIp.String())
	assert.Equal(t, int32(941100), row.RuleID)
	assert.Equal(t, "critical", row.Severity)
	assert.Equal(t, "attack-xss", row.Category.String)
	assert.Equal(t, "app.test", row.Host.String)
	assert.Equal(t, uuidOf(7), row.ApplicationID, "a host served by one application is attributed to it")
	assert.True(t, row.TxID.Valid)
	assert.Equal(t, 1, f.lookups["app.test"], "six events for one host are one lookup")
}

func TestRecorder_AttributionDegradesPerRow(t *testing.T) {
	f := &fakeStore{apps: map[string][]pgtype.UUID{
		"one.test":   {uuidOf(1)},
		"split.test": {uuidOf(1), uuidOf(2)}, // one host, two applications, by path
	}}
	r := newTestRecorder(f)
	for _, host := range []string{"one.test", "split.test", "nobody.test", ""} {
		id := "dzZFiXVZIsVuPAtt" // Coraza's own id: carries no host
		if host != "" {
			id = host + "_8331db49-d28c-434d-be4a-7458091191b6"
		}
		r.HandleCaddyLine(context.Background(), realLine("m", "d", "/", id))
	}
	r.flush(context.Background(), r.drain())

	rows := f.written()
	require.Len(t, rows, 4, "no event is dropped because attribution failed")
	assert.Equal(t, uuidOf(1), rows[0].ApplicationID)
	assert.False(t, rows[1].ApplicationID.Valid, "a host split by path cannot be told apart without the URI, so it is not guessed")
	assert.True(t, rows[1].Host.Valid, "...but its host is still known")
	assert.False(t, rows[2].ApplicationID.Valid, "an unknown host")
	assert.False(t, rows[3].Host.Valid, "no host in the id at all")
	assert.True(t, rows[3].TxID.Valid)
}

func TestRecorder_ALookupFailureIsNotRemembered(t *testing.T) {
	f := &fakeStore{apps: map[string][]pgtype.UUID{"app.test": {uuidOf(3)}}, lookErr: errors.New("db down")}
	r := newTestRecorder(f)
	line := realLine("m", "d", "/", "app.test_8331db49-d28c-434d-be4a-7458091191b6")

	r.HandleCaddyLine(context.Background(), line)
	r.flush(context.Background(), r.drain())
	require.False(t, f.written()[0].ApplicationID.Valid)

	// The database comes back. A cached failure would pin the host to "unknown".
	f.lookErr = nil
	r.HandleCaddyLine(context.Background(), line)
	r.flush(context.Background(), r.drain())
	assert.Equal(t, uuidOf(3), f.written()[1].ApplicationID)
}

func TestRecorder_AnswersAreRememberedForAMinuteOnly(t *testing.T) {
	f := &fakeStore{apps: map[string][]pgtype.UUID{"app.test": {uuidOf(3)}}}
	r := newTestRecorder(f)
	now := r.now()
	clock := &now
	r.resolver.now = func() time.Time { return *clock }

	r.resolver.application(context.Background(), "app.test")
	r.resolver.application(context.Background(), "app.test")
	assert.Equal(t, 1, f.lookups["app.test"])

	*clock = clock.Add(resolveTTL + time.Second)
	r.resolver.application(context.Background(), "app.test")
	assert.Equal(t, 2, f.lookups["app.test"], "a domain moved to another application is noticed")
}

func TestRecorder_TheHookNeverBlocks(t *testing.T) {
	r := newTestRecorder(&fakeStore{})
	line := realLine("m", "d", "/", goodID)

	// Nothing is running Run, so the queue fills. The hook is on the log
	// collector's read path: it must drop and count, not wait.
	done := make(chan struct{})
	go func() {
		for i := 0; i < queueSize+500; i++ {
			r.HandleCaddyLine(context.Background(), line)
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("HandleCaddyLine blocked on a full queue")
	}
	assert.Equal(t, int64(500), r.Stats().Dropped)
}

func TestRecorder_CountsWhatItCannotUse(t *testing.T) {
	r := newTestRecorder(&fakeStore{})
	r.HandleCaddyLine(context.Background(), `{"level":"info","logger":"tls","msg":"x"}`)
	r.HandleCaddyLine(context.Background(), realLine("m", "d", "/", goodID)[:140])
	r.HandleCaddyLine(context.Background(), asLine(`[client "1.2.3.4"] Coraza: Warning. not the grammar`))

	st := r.Stats()
	assert.Equal(t, int64(2), st.Malformed, "lines that claim to be rule matches and are not")
	assert.Empty(t, r.drain(), "nothing from them is queued")
}

func TestRecorder_ADatabaseErrorIsReportedAndRecovered(t *testing.T) {
	f := &fakeStore{insErr: errors.New("connection refused")}
	r := newTestRecorder(f)
	r.HandleCaddyLine(context.Background(), realLine("m", "d", "/", goodID))
	r.flush(context.Background(), r.drain())

	st := r.Stats()
	assert.Zero(t, st.Recorded)
	assert.Equal(t, "connection refused", st.LastError)
	assert.False(t, st.LastErrorAt.IsZero())

	f.insErr = nil
	r.HandleCaddyLine(context.Background(), realLine("m", "d", "/", goodID))
	r.flush(context.Background(), r.drain())
	assert.Equal(t, int64(1), r.Stats().Recorded)
}

func TestRecorder_ImplausibleTimestampsAreReplaced(t *testing.T) {
	f := &fakeStore{}
	r := newTestRecorder(f)
	now := r.now()

	for _, at := range []time.Time{{}, now.Add(48 * time.Hour), now.Add(-72 * time.Hour), now.Add(-time.Hour)} {
		ev, out := ParseCaddyLine(realLine("m", "d", "/", goodID))
		require.Equal(t, Matched, out)
		ev.At = at
		r.queue <- ev
	}
	r.flush(context.Background(), r.drain())

	rows := f.written()
	require.Len(t, rows, 4)
	assert.Equal(t, now, rows[0].RecordedAt.Time, "no time")
	assert.Equal(t, now, rows[1].RecordedAt.Time, "from the future")
	assert.Equal(t, now, rows[2].RecordedAt.Time, "from days ago")
	assert.Equal(t, now.Add(-time.Hour), rows[3].RecordedAt.Time, "a believable time is kept")
}

func TestRecorder_RunFlushesWhatIsLeftOnShutdown(t *testing.T) {
	f := &fakeStore{}
	r := newTestRecorder(f)
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() { r.Run(ctx); close(done) }()
	for i := 0; i < 3; i++ {
		r.HandleCaddyLine(context.Background(), realLine("m", "d", "/", goodID))
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return")
	}
	assert.Len(t, f.written(), 3, "events queued before shutdown are not lost")
}
