package crowdsec

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/belune-hq/belune/internal/pkg/redact"
	"github.com/belune-hq/belune/internal/store/generated"
)

// BanCategories are the WAF rule categories that may count towards a ban: the
// injection and file-inclusion classes, where a rule match is an attempt to take
// something over. It is a closed list, mirrored by a CHECK on the column.
//
// Absent on purpose:
//   - attack-protocol (CRS 920xxx/921xxx): request-format policy. 920420 "request
//     content type is not allowed by policy" fired on ordinary text/plain POSTs. Its
//     severity is CRITICAL like the real attacks', so severity cannot be the key —
//     the category is.
//   - attack-reputation-scanner (913xxx): a User-Agent match, which a legitimate
//     scanner the operator runs themselves trips by design.
//   - attack-generic, attack-multipart-header, attack-fixation, attack-disclosure:
//     either response-phase, or too broad to ban on.
//   - rules with no category, including the anomaly-score summary.
var BanCategories = []string{
	"attack-injection-java", "attack-injection-php", "attack-lfi", "attack-rce",
	"attack-rfi", "attack-sqli", "attack-ssrf", "attack-ssti", "attack-xss",
}

const (
	maxCandidates = 200
)

// Validate checks a policy and normalises its category list.
func (p *Policy) Validate() error {
	switch {
	case p.Threshold < 3 || p.Threshold > 1000:
		return errors.New("threshold must be between 3 and 1000 requests")
	case p.WindowMinutes < 1 || p.WindowMinutes > 1440:
		return errors.New("window must be between 1 and 1440 minutes")
	case p.DurationMinutes < 5 || p.DurationMinutes > 10080:
		return errors.New("ban duration must be between 5 minutes and 7 days")
	case p.MaxActive < 1 || p.MaxActive > 500:
		return errors.New("the active-ban cap must be between 1 and 500")
	}
	cats := slices.Clone(p.Categories)
	slices.Sort(cats)
	cats = slices.Compact(cats)
	for _, c := range cats {
		if !slices.Contains(BanCategories, c) {
			return fmt.Errorf("%q is not a category that can lead to a ban", c)
		}
	}
	if p.Enabled && len(cats) == 0 {
		return errors.New("pick at least one category, or turn automatic bans off")
	}
	if cats == nil {
		cats = []string{}
	}
	p.Categories = cats
	return nil
}

// AutobanStatus is what the evaluator has done since the process started.
type AutobanStatus struct {
	LastRun time.Time `json:"last_run,omitzero"`
	// LastError is why the last run could not complete, empty when it did.
	LastError string `json:"last_error,omitempty"`
	// Active is how many automatic bans are in force now.
	Active int `json:"active"`
	// Banned counts bans issued by this process.
	Banned int64 `json:"banned"`
	// Skipped is how many addresses over the threshold the last run left alone,
	// because the cap was reached, they cannot be banned, or an admin lifted them.
	Skipped int `json:"skipped"`
	// Capped is whether the last run stopped at the active-ban cap. When this is
	// true, something is tripping rules at a rate worth looking at.
	Capped bool `json:"capped"`
}

type autobanRun struct {
	// running keeps one evaluation at a time in this process. The scheduler's own
	// uniqueness covers the queue; this covers a manual trigger and a slow pass.
	running sync.Mutex

	mu     sync.Mutex
	status AutobanStatus
}

func (a *autobanRun) set(f func(*AutobanStatus)) {
	a.mu.Lock()
	f(&a.status)
	a.mu.Unlock()
}

// Autoban reports what the evaluator has done.
func (s *Service) Autoban() AutobanStatus {
	s.autoban.mu.Lock()
	defer s.autoban.mu.Unlock()
	return s.autoban.status
}

// EvaluateAutoban bans addresses whose WAF detections cross the policy's line. It
// does nothing unless both the feature and the policy are switched on.
//
// Everything below is a limit on how much harm it can do, because a false positive
// here bans a real visitor:
//   - it counts distinct requests in the ban-worthy categories only;
//   - it never bans a private or reserved address, an allowlisted one, or one an
//     admin lifted inside the window;
//   - at most MaxActive of its bans are in force at once;
//   - every ban expires, and every one is audited.
func (s *Service) EvaluateAutoban(ctx context.Context) error {
	if !s.autoban.running.TryLock() {
		return nil
	}
	defer s.autoban.running.Unlock()

	cfg, err := s.Config(ctx)
	if err != nil {
		return err
	}
	if !cfg.Enabled || !cfg.Autoban.Enabled {
		return nil
	}
	p := cfg.Autoban
	now := s.now()

	fail := func(err error) error {
		s.autoban.set(func(st *AutobanStatus) { st.LastRun, st.LastError = now, redact.Error(err.Error()) })
		return err
	}

	decisions, err := s.ctl.Decisions(ctx)
	if err != nil {
		return fail(err)
	}
	active := 0
	for _, d := range decisions {
		if strings.HasPrefix(d.Reason, ReasonPrefixWAF) {
			active++
		}
	}

	window := time.Duration(p.WindowMinutes) * time.Minute
	candidates, err := s.store.WAFAutobanCandidates(ctx, generated.WAFAutobanCandidatesParams{
		Since:      pgtype.Timestamptz{Time: now.Add(-window), Valid: true},
		Categories: p.Categories,
		Threshold:  int32(p.Threshold),
		RowLimit:   maxCandidates,
	})
	if err != nil {
		return fail(fmt.Errorf("reading WAF detections: %w", err))
	}

	lifted, err := s.liftedSince(ctx, now.Add(-window))
	if err != nil {
		return fail(err)
	}

	duration := time.Duration(p.DurationMinutes) * time.Minute
	var banned int64
	skipped, capped := 0, false
	for _, c := range candidates {
		ip := c.ClientIp.Unmap()
		target := netip.PrefixFrom(ip, ip.BitLen())

		if covers(decisions, ip) {
			continue
		}
		if _, ok := lifted[targetValue(target)]; ok || CheckBannable(target) != nil {
			skipped++
			continue
		}
		if active >= p.MaxActive {
			capped = true
			skipped++
			continue
		}

		reason := CleanReason(fmt.Sprintf("%s%s: %d requests in %dm",
			ReasonPrefixWAF, strings.Join(c.Categories, ", "), c.Requests, p.WindowMinutes))
		err := s.ctl.Ban(ctx, target, duration, reason)
		if errors.Is(err, ErrAllowlisted) {
			skipped++
			continue
		}
		if err != nil {
			s.autoban.set(func(st *AutobanStatus) { st.Banned += banned })
			return fail(err)
		}
		active++
		banned++
		slog.Info("crowdsec: banned an address for WAF detections", "ip", ip, "requests", c.Requests, "categories", c.Categories)
		s.recordSystem("crowdsec_autoban", targetValue(target), map[string]any{
			"requests": c.Requests, "categories": c.Categories,
			"window_minutes": p.WindowMinutes, "duration": duration.String(),
		})
	}

	s.autoban.set(func(st *AutobanStatus) {
		st.LastRun, st.LastError = now, ""
		st.Active, st.Skipped, st.Capped = active, skipped, capped
		st.Banned += banned
	})
	if capped {
		slog.Warn("crowdsec: automatic bans are at their cap; more addresses are over the threshold than will be banned",
			"cap", p.MaxActive, "skipped", skipped)
	}
	return nil
}

// liftedSince is the set of targets an admin unbanned since t.
func (s *Service) liftedSince(ctx context.Context, t time.Time) (map[string]struct{}, error) {
	rows, err := s.store.ListCrowdSecUnbansSince(ctx, pgtype.Timestamptz{Time: t, Valid: true})
	if err != nil {
		return nil, fmt.Errorf("reading recent unbans: %w", err)
	}
	out := make(map[string]struct{}, len(rows))
	for _, r := range rows {
		if r.Valid {
			out[r.String] = struct{}{}
		}
	}
	return out, nil
}

// covers reports whether any decision already bans ip, as itself or inside a range.
func covers(ds []Decision, ip netip.Addr) bool {
	for _, d := range ds {
		if p, err := ParseTarget(d.Value); err == nil && p.Contains(ip) {
			return true
		}
	}
	return false
}
