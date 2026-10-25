package crowdsec

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"strconv"
	"strings"
	"time"
)

const (
	// BouncerName is the one bouncer Belune registers: Caddy's.
	BouncerName = "belune-caddy"

	// AllowlistName is the allowlist Belune owns. A separate list from anything an
	// operator made by hand with cscli, so pruning ours can never touch theirs.
	AllowlistName = "belune-admins"

	// decisionLimit is how many decisions one listing returns. cscli's own default
	// is 100, which would silently truncate a busy list.
	decisionLimit = 500
)

// ErrAllowlisted means CrowdSec refused to ban an address, or a range containing
// one, because an allowlist covers it. It is the safety working, not a failure.
var ErrAllowlisted = errors.New("the address is allowlisted")

// Decision is one ban in force.
type Decision struct {
	ID    int64  `json:"id"`
	Value string `json:"value"`
	// Scope is "ip" or "range".
	Scope string `json:"scope"`
	Type  string `json:"type"`
	// Origin is where CrowdSec says the decision came from: "cscli" for everything
	// Belune creates. The reason carries the finer attribution (see ReasonSource).
	Origin    string    `json:"origin"`
	Reason    string    `json:"reason"`
	CreatedAt time.Time `json:"created_at"`
	ExpiresAt time.Time `json:"expires_at"`
}

// Bouncer is a registered consumer of the ban list.
type Bouncer struct {
	Name string
	// LastPull is the last time it fetched the list; zero if it never has. This is
	// how Belune knows enforcement is live and not merely configured: a bouncer
	// that cannot authenticate stops pulling and Caddy fails open, quietly.
	LastPull time.Time
	Version  string
}

// AllowItem is one allowlisted address or range.
type AllowItem struct {
	Value       string    `json:"value"`
	Description string    `json:"description"`
	CreatedAt   time.Time `json:"created_at"`
	// ExpiresAt is zero for an entry that never expires.
	ExpiresAt time.Time `json:"expires_at,omitzero"`
}

// Controller issues cscli commands. It decides nothing: every guard that matters
// lives in Service, and Controller assumes it is handed validated values.
type Controller struct {
	run Runner
	now func() time.Time
}

func NewController(r Runner) *Controller {
	return &Controller{run: r, now: time.Now}
}

// Ping reports whether the local API answers.
func (c *Controller) Ping(ctx context.Context) error {
	_, err := c.run.Run(ctx, "lapi", "status")
	return err
}

// Decisions lists the bans in force.
func (c *Controller) Decisions(ctx context.Context) ([]Decision, error) {
	out, err := c.run.Run(ctx, "decisions", "list", "-o", "json", "--limit="+strconv.Itoa(decisionLimit))
	if err != nil {
		return nil, err
	}
	return parseDecisions(out, c.now())
}

// DecisionsFor lists the bans in force on exactly this address or range.
func (c *Controller) DecisionsFor(ctx context.Context, target netip.Prefix) ([]Decision, error) {
	out, err := c.run.Run(ctx, "decisions", "list", "-o", "json", "--limit="+strconv.Itoa(decisionLimit), targetFlag(target))
	if err != nil {
		return nil, err
	}
	return parseDecisions(out, c.now())
}

// Ban adds a ban. It never passes --bypass-allowlist: an allowlisted address is
// refused with ErrAllowlisted, and nothing in Belune can override that.
func (c *Controller) Ban(ctx context.Context, target netip.Prefix, duration time.Duration, reason string) error {
	_, err := c.run.Run(ctx, "decisions", "add",
		targetFlag(target),
		"--type=ban",
		"--duration="+formatMinutes(duration),
		"--reason="+reason,
	)
	return classify(err)
}

// Unban removes every decision on exactly this address or range, then checks it is
// gone. cscli exits 0 whether or not anything matched and reports the count only
// in a log line, so the post-condition is read back instead of parsed out of text.
func (c *Controller) Unban(ctx context.Context, target netip.Prefix) error {
	if _, err := c.run.Run(ctx, "decisions", "delete", targetFlag(target)); err != nil {
		return err
	}
	left, err := c.DecisionsFor(ctx, target)
	if err != nil {
		return err
	}
	if n := countExact(left, target); n > 0 {
		return fmt.Errorf("%d decision(s) on %s are still in force after the delete", n, target)
	}
	return nil
}

// countExact counts decisions whose value is exactly target. cscli's --ip filter
// also returns range decisions that contain the address, which are a different
// ban and are not what an unban of the address removes.
func countExact(ds []Decision, target netip.Prefix) int {
	n := 0
	for _, d := range ds {
		if d.Value == targetValue(target) {
			n++
		}
	}
	return n
}

// Bouncer returns the registered bouncer called name, if there is one.
func (c *Controller) Bouncer(ctx context.Context, name string) (Bouncer, bool, error) {
	out, err := c.run.Run(ctx, "bouncers", "list", "-o", "json")
	if err != nil {
		return Bouncer{}, false, err
	}
	var rows []struct {
		Name     string  `json:"name"`
		LastPull *string `json:"last_pull"`
		Version  string  `json:"version"`
	}
	if err := decodeJSON(out, &rows); err != nil {
		return Bouncer{}, false, fmt.Errorf("reading bouncers: %w", err)
	}
	for _, r := range rows {
		if r.Name != name {
			continue
		}
		b := Bouncer{Name: r.Name, Version: r.Version}
		if r.LastPull != nil {
			if t, err := time.Parse(time.RFC3339Nano, *r.LastPull); err == nil {
				b.LastPull = t
			}
		}
		return b, true, nil
	}
	return Bouncer{}, false, nil
}

// RegisterBouncer registers name and returns the key CrowdSec generated for it.
// The key is generated on the CrowdSec side and printed on stdout, so it is never
// an argument on a command line, where a process listing on the host could read it.
func (c *Controller) RegisterBouncer(ctx context.Context, name string) (string, error) {
	out, err := c.run.Run(ctx, "bouncers", "add", name, "-o", "raw")
	if err != nil {
		return "", err
	}
	key := strings.TrimSpace(out)
	if key == "" {
		return "", errors.New("CrowdSec registered the bouncer but printed no key")
	}
	return key, nil
}

// DeleteBouncer removes a bouncer. One that is already gone is not an error.
func (c *Controller) DeleteBouncer(ctx context.Context, name string) error {
	_, err := c.run.Run(ctx, "bouncers", "delete", name)
	var ce *CommandError
	if errors.As(err, &ce) && strings.Contains(ce.Stderr, "not found") {
		return nil
	}
	return err
}

// Allowlist returns the items on Belune's allowlist, creating the list if it does
// not exist yet.
func (c *Controller) Allowlist(ctx context.Context) ([]AllowItem, error) {
	items, exists, err := c.readAllowlist(ctx)
	if err != nil {
		return nil, err
	}
	if !exists {
		return nil, nil
	}
	return items, nil
}

func (c *Controller) readAllowlist(ctx context.Context) ([]AllowItem, bool, error) {
	out, err := c.run.Run(ctx, "allowlists", "list", "-o", "json")
	if err != nil {
		return nil, false, err
	}
	var lists []struct {
		Name  string `json:"name"`
		Items []struct {
			Value       string `json:"value"`
			Description string `json:"description"`
			CreatedAt   string `json:"created_at"`
			Expiration  string `json:"expiration"`
		} `json:"items"`
	}
	if err := decodeJSON(out, &lists); err != nil {
		return nil, false, fmt.Errorf("reading allowlists: %w", err)
	}
	for _, l := range lists {
		if l.Name != AllowlistName {
			continue
		}
		items := make([]AllowItem, 0, len(l.Items))
		for _, i := range l.Items {
			it := AllowItem{Value: i.Value, Description: i.Description}
			it.CreatedAt, _ = time.Parse(time.RFC3339Nano, i.CreatedAt)
			if t, err := time.Parse(time.RFC3339Nano, i.Expiration); err == nil && t.Year() < 9000 {
				it.ExpiresAt = t
			}
			items = append(items, it)
		}
		return items, true, nil
	}
	return nil, false, nil
}

// Allow puts target on the allowlist until ttl from now. Adding it again does not
// extend it (cscli warns "already in allowlist" and keeps the old expiry), so an
// existing entry is removed first: a login must refresh the clock, not leave the
// original one running.
//
// Adding to the allowlist also deletes any ban already in force on that address.
// That is the property the whole feature leans on: an admin who was banned and
// then logs in from the same address is un-banned by the login itself.
func (c *Controller) Allow(ctx context.Context, target netip.Prefix, description string, ttl time.Duration) error {
	_, exists, err := c.readAllowlist(ctx)
	if err != nil {
		return err
	}
	if !exists {
		if _, err := c.run.Run(ctx, "allowlists", "create", AllowlistName, "-d", "Addresses Belune must never ban"); err != nil {
			// A concurrent create by another login is fine; anything else is not.
			var ce *CommandError
			if !errors.As(err, &ce) || !strings.Contains(ce.Stderr, "already exists") {
				return err
			}
		}
	} else if err := c.Unallow(ctx, target); err != nil {
		return err
	}
	_, err = c.run.Run(ctx, "allowlists", "add", AllowlistName, targetValue(target),
		"-d", description, "-e", formatMinutes(ttl))
	return err
}

// Unallow removes target from the allowlist. One that is not there is not an error.
func (c *Controller) Unallow(ctx context.Context, target netip.Prefix) error {
	_, err := c.run.Run(ctx, "allowlists", "remove", AllowlistName, targetValue(target))
	var ce *CommandError
	if errors.As(err, &ce) && (strings.Contains(ce.Stderr, "not found") || strings.Contains(ce.Stderr, "not in")) {
		return nil
	}
	return err
}

// classify turns cscli's refusal to ban an allowlisted address into ErrAllowlisted.
func classify(err error) error {
	var ce *CommandError
	if errors.As(err, &ce) && strings.Contains(ce.Stderr, "is allowlisted by") {
		return ErrAllowlisted
	}
	return err
}

// targetFlag is the cscli flag that names target: --ip for a single address, --range
// for a prefix.
func targetFlag(t netip.Prefix) string {
	if t.IsSingleIP() {
		return "--ip=" + t.Addr().String()
	}
	return "--range=" + t.String()
}

// targetValue is the value as CrowdSec stores and prints it.
func targetValue(t netip.Prefix) string {
	if t.IsSingleIP() {
		return t.Addr().String()
	}
	return t.String()
}

// formatMinutes renders d in whole minutes ("90m"), the one form that is
// unambiguous to every cscli version: it accepts "d" suffixes too, but a plain Go
// duration cannot be misread.
func formatMinutes(d time.Duration) string {
	return strconv.FormatInt(int64(d/time.Minute), 10) + "m"
}

func decodeJSON(out string, v any) error {
	out = strings.TrimSpace(out)
	// cscli prints "null" for an empty list in some versions and "[]" in others.
	if out == "" || out == "null" {
		return nil
	}
	dec := json.NewDecoder(bytes.NewReader([]byte(out)))
	return dec.Decode(v)
}

// parseDecisions flattens cscli's alert-shaped listing (an alert holds the
// decisions it caused) into one row per decision.
func parseDecisions(out string, now time.Time) ([]Decision, error) {
	var alerts []struct {
		CreatedAt string `json:"created_at"`
		Decisions []struct {
			ID        int64  `json:"id"`
			Duration  string `json:"duration"`
			Origin    string `json:"origin"`
			Scenario  string `json:"scenario"`
			Scope     string `json:"scope"`
			Type      string `json:"type"`
			Value     string `json:"value"`
			Simulated bool   `json:"simulated"`
		} `json:"decisions"`
	}
	if err := decodeJSON(out, &alerts); err != nil {
		return nil, fmt.Errorf("reading decisions: %w", err)
	}
	ds := make([]Decision, 0, len(alerts))
	for _, a := range alerts {
		created, _ := time.Parse(time.RFC3339Nano, a.CreatedAt)
		for _, d := range a.Decisions {
			if d.Simulated {
				continue
			}
			// Duration is the time REMAINING, as a Go duration ("59m59s").
			remaining, err := time.ParseDuration(d.Duration)
			if err != nil || remaining <= 0 {
				continue
			}
			ds = append(ds, Decision{
				ID:        d.ID,
				Value:     d.Value,
				Scope:     strings.ToLower(d.Scope),
				Type:      d.Type,
				Origin:    d.Origin,
				Reason:    d.Scenario,
				CreatedAt: created,
				ExpiresAt: now.Add(remaining),
			})
		}
	}
	return ds, nil
}
