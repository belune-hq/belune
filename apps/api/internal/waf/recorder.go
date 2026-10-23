package waf

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/belune-hq/belune/internal/store/generated"
)

const (
	// queueSize bounds the events waiting for the database. The hook runs inline on
	// the log collector's read path and must never block it, so a full queue drops
	// the event and counts it: under a flood the detection log is allowed to be
	// incomplete, the proxy's log pipeline is not allowed to stall.
	queueSize  = 4096
	batchSize  = 200
	flushEvery = 2 * time.Second

	// resolveTTL is how long a host's application is remembered, either way. Short
	// enough that a domain moved to another application is picked up within a
	// minute, long enough that a burst against one host is one query.
	resolveTTL     = time.Minute
	resolveMaxSize = 4096

	// plausible bounds on a line's own timestamp; outside them the event is
	// stamped with the time it was read.
	maxClockSkew = time.Minute
	maxAge       = 24 * time.Hour
)

// Store is the slice of the database the recorder uses.
type Store interface {
	BulkInsertWAFEvents(ctx context.Context, arg []generated.BulkInsertWAFEventsParams) (int64, error)
	ListApplicationIDsForHost(ctx context.Context, hostname string) ([]pgtype.UUID, error)
}

var _ Store = (*generated.Queries)(nil)

// Stats is the recorder's account of itself since the process started. Dropped and
// malformed are the two ways an event is lost without a trace in the table, so
// they are surfaced rather than left to be inferred from a count that looks low.
type Stats struct {
	Since       time.Time `json:"since"`
	Recorded    int64     `json:"recorded"`
	Dropped     int64     `json:"dropped"`
	Malformed   int64     `json:"malformed"`
	LastError   string    `json:"last_error,omitempty"`
	LastErrorAt time.Time `json:"last_error_at,omitzero"`
}

// Recorder turns Caddy's WAF lines into rows.
type Recorder struct {
	store Store
	now   func() time.Time

	queue chan Event

	recorded  atomic.Int64
	dropped   atomic.Int64
	malformed atomic.Int64
	since     time.Time

	errMu     sync.Mutex
	lastErr   string
	lastErrAt time.Time

	resolver *hostResolver
}

// NewRecorder builds a recorder. Run must be started for anything to be stored.
func NewRecorder(store Store) *Recorder {
	now := time.Now
	return &Recorder{
		store:    store,
		now:      now,
		queue:    make(chan Event, queueSize),
		since:    now(),
		resolver: newHostResolver(store, now),
	}
}

// HandleCaddyLine is the log collector's line hook for Caddy. It runs inline on
// the collector's read path, for every line Caddy writes, so it parses, queues and
// returns: no database, no lock held, nothing that can wait.
func (r *Recorder) HandleCaddyLine(_ context.Context, line string) {
	ev, out := ParseCaddyLine(line)
	switch out {
	case NotWAF:
		return
	case Malformed:
		r.malformed.Add(1)
		return
	}
	select {
	case r.queue <- ev:
	default:
		r.dropped.Add(1)
	}
}

// Stats reports what the recorder has done since it started.
func (r *Recorder) Stats() Stats {
	r.errMu.Lock()
	lastErr, lastErrAt := r.lastErr, r.lastErrAt
	r.errMu.Unlock()
	return Stats{
		Since:       r.since,
		Recorded:    r.recorded.Load(),
		Dropped:     r.dropped.Load(),
		Malformed:   r.malformed.Load(),
		LastError:   lastErr,
		LastErrorAt: lastErrAt,
	}
}

// Run writes queued events in batches until ctx is cancelled, then writes what is
// left.
func (r *Recorder) Run(ctx context.Context) {
	tick := time.NewTicker(flushEvery)
	defer tick.Stop()

	batch := make([]Event, 0, batchSize)
	for {
		select {
		case ev := <-r.queue:
			batch = append(batch, ev)
			if len(batch) >= batchSize {
				r.flush(ctx, batch)
				batch = batch[:0]
			}
		case <-tick.C:
			if len(batch) > 0 {
				r.flush(ctx, batch)
				batch = batch[:0]
			}
		case <-ctx.Done():
			// Bounded, like the log collector's own shutdown flush: a slow database
			// must not hold the process open.
			flushCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			r.flush(flushCtx, append(batch, r.drain()...))
			return
		}
	}
}

// drain takes whatever is queued without waiting.
func (r *Recorder) drain() []Event {
	var out []Event
	for {
		select {
		case ev := <-r.queue:
			out = append(out, ev)
		default:
			return out
		}
	}
}

func (r *Recorder) flush(ctx context.Context, events []Event) {
	if len(events) == 0 {
		return
	}
	now := r.now()
	rows := make([]generated.BulkInsertWAFEventsParams, 0, len(events))
	for _, ev := range events {
		at := ev.At
		if at.IsZero() || at.After(now.Add(maxClockSkew)) || at.Before(now.Add(-maxAge)) {
			at = now
		}
		row := generated.BulkInsertWAFEventsParams{
			RecordedAt: pgtype.Timestamptz{Time: at, Valid: true},
			ClientIp:   ev.ClientIP,
			RuleID:     int32(ev.RuleID),
			Severity:   ev.Severity,
			Category:   text(ev.Category),
			Message:    ev.Message,
			Host:       text(ev.Host),
			TxID:       text(ev.TxID),
		}
		// Attribution is best-effort and per row: a failed lookup costs the
		// application, never the event.
		row.ApplicationID = r.resolver.application(ctx, ev.Host)
		rows = append(rows, row)
	}

	if _, err := r.store.BulkInsertWAFEvents(ctx, rows); err != nil {
		r.noteError(err)
		slog.Warn("waf: failed to record detection events", "events", len(rows), "error", err)
		return
	}
	r.recorded.Add(int64(len(rows)))
}

func (r *Recorder) noteError(err error) {
	r.errMu.Lock()
	r.lastErr, r.lastErrAt = err.Error(), r.now()
	r.errMu.Unlock()
}

func text(s string) pgtype.Text { return pgtype.Text{String: s, Valid: s != ""} }

// hostResolver maps a host to the application that serves it, when exactly one
// does.
type hostResolver struct {
	store Store
	now   func() time.Time

	mu    sync.Mutex
	cache map[string]resolved
}

type resolved struct {
	app     pgtype.UUID
	expires time.Time
}

func newHostResolver(store Store, now func() time.Time) *hostResolver {
	return &hostResolver{store: store, now: now, cache: make(map[string]resolved)}
}

// application returns the application serving host, or an invalid UUID when the
// host is unknown or is split across several applications by path. The event
// carries no path, so a split host cannot be told apart and is left unattributed
// rather than guessed at.
func (h *hostResolver) application(ctx context.Context, host string) pgtype.UUID {
	if host == "" {
		return pgtype.UUID{}
	}
	now := h.now()
	h.mu.Lock()
	if c, ok := h.cache[host]; ok && now.Before(c.expires) {
		h.mu.Unlock()
		return c.app
	}
	h.mu.Unlock()

	ids, err := h.store.ListApplicationIDsForHost(ctx, host)
	if err != nil {
		// Not cached: a transient failure must not pin a host to "unknown".
		return pgtype.UUID{}
	}
	var app pgtype.UUID
	if len(ids) == 1 && ids[0].Valid {
		app = ids[0]
	}

	h.mu.Lock()
	if len(h.cache) >= resolveMaxSize {
		// Hosts come from the transaction id, so a scanner sweeping a wildcard
		// domain can mint them without limit. Start over rather than grow.
		h.cache = make(map[string]resolved)
	}
	h.cache[host] = resolved{app: app, expires: now.Add(resolveTTL)}
	h.mu.Unlock()
	return app
}
