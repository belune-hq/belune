package handler

import (
	"context"
	"encoding/json"
	"log/slog"
	"sync"
	"time"

	"github.com/belune-hq/belune/internal/runtime"
)

// platformChannel carries "an update is underway" to every connected client.
// Authorized by wsOpenChannels; the payload is exactly what GET /api/version
// already exposes.
const platformChannel = "platform"

// updateProbeTTL bounds how stale the public `updating` flag's container read
// may be. A var so the test binary can disable the cache (export_test.go);
// elsewhere it is a constant in effect.
//
// 5s because /api/version is public, unrate-limited and polled by every open
// tab, while ListAllContainers takes no filter and lists the whole host.
var updateProbeTTL = 5 * time.Second

// updateProbeTimeout bounds one container listing for the public flag.
const updateProbeTimeout = 3 * time.Second

// updateProbe answers "is a self-update helper running, and since when" for the
// public version endpoint, behind a short cache.
//
// Only the container read is cached. h.updateStarting is a free atomic, so the
// pull window is reported the instant it opens rather than up to a TTL late.
type updateProbe struct {
	mu      sync.Mutex
	now     func() time.Time // injectable for tests; nil means time.Now
	readAt  time.Time
	read    bool
	running bool
	since   time.Time
}

func (p *updateProbe) clock() time.Time {
	if p.now != nil {
		return p.now()
	}
	return time.Now()
}

// helper reports whether an update helper is running and when it was created.
//
// The lock is held across the Docker call on purpose: concurrent requests that
// arrive while it is stale queue behind one read instead of each issuing their
// own. A failed read reports "not running" and is cached like any other — a
// Docker outage must not turn every poll into another failing call, and the
// worst case is the flag being false for one TTL.
func (p *updateProbe) helper(ctx context.Context, rt runtime.ContainerRuntime) (bool, time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.read && p.clock().Sub(p.readAt) < updateProbeTTL {
		return p.running, p.since
	}
	// Its own context, not the request's: the answer is shared by every client,
	// so one caller's closed tab or timeout must not be cached as "no update"
	// for a TTL — and a wedged daemon must not hold the lock (and so every
	// poller) for the route's whole 15s.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), updateProbeTimeout)
	defer cancel()
	c, running, err := findUpdateHelper(ctx, rt)
	if err != nil {
		slog.Warn("update: could not list containers for the public update flag", "error", err)
		running = false
	}
	p.read, p.readAt, p.running, p.since = true, p.clock(), running, c.CreatedAt
	return p.running, p.since
}

// markRunning records a helper this process has just started. Without it the
// flag would drop to false the moment updateStarting clears (the helper now
// exists, but the cache may still hold the pre-spawn "none") and flicker for up
// to a TTL in the middle of an update.
func (p *updateProbe) markRunning(since time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.read, p.readAt, p.running, p.since = true, p.clock(), true, since
}

// updateState is the public view: whether an update is happening right now and
// how long it has been going, as of this request.
//
// Derived, never stored: a failed update self-clears because an exited helper
// stops being "running", whereas a stored flag would need expiry logic — an
// update that dies at the backup step never replaces the API, so nothing would
// ever clear it. And it deliberately omits the target version, which is not
// public.
func (h *Handler) updateState(ctx context.Context) (updating bool, elapsed time.Duration) {
	now := time.Now()
	if rt, err := h.runtimes.Local(ctx); err == nil {
		if running, since := h.updateProbe.helper(ctx, rt); running {
			// The helper is created only AFTER the pull, so its CreatedAt alone
			// would restart the clock at the handover — and a tab opened after
			// it, with no earlier reading to keep, would be told a four-minute
			// update had just begun. Prefer the pull's start when this process
			// has one. Bounded by the pull timeout so a stale value left by an
			// earlier failed attempt cannot inflate a later host-run update, and
			// zero (never set, e.g. after the API container was replaced) falls
			// back to CreatedAt alone.
			if began := h.updateBeganAt.Load(); began != 0 {
				b := time.Unix(0, began)
				if b.Before(since) && since.Sub(b) <= updatePullTimeout {
					since = b
				}
			}
			return true, clampElapsed(now.Sub(since))
		}
	}
	// The pull window: an update is underway but no helper exists yet. Only the
	// dashboard path has one this process can see — a host-run update.sh pulls in
	// its own process, so that stretch stays blind by design (the launcher must
	// stay dumb).
	if h.updateStarting.Load() {
		return true, clampElapsed(now.Sub(time.Unix(0, h.updateBeganAt.Load())))
	}
	return false, 0
}

func clampElapsed(d time.Duration) time.Duration {
	if d < 0 {
		return 0
	}
	return d.Truncate(time.Second)
}

// publishUpdating tells connected clients the flag changed. Promptness only —
// GET /api/version stays the source of truth, so a dropped frame costs a poll.
func (h *Handler) publishUpdating(updating bool) {
	if h.hub == nil {
		return
	}
	data, err := json.Marshal(map[string]bool{"updating": updating})
	if err != nil {
		return
	}
	h.hub.Broadcast(platformChannel, "update", data)
}
