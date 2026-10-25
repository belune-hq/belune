package crowdsec

import (
	"context"
	"errors"
	"net/netip"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The fixtures are cscli v1.8.1's own output, captured from a real local API: the
// shape this parser has to read is the one CrowdSec prints, not the one a test
// author would have invented.

func TestParseDecisions_ReadsRealCscliOutput(t *testing.T) {
	now := time.Date(2026, 10, 25, 11, 0, 0, 0, time.UTC)
	ds, err := parseDecisions(fixture(t, "decisions-v1.8.1.json"), now)
	require.NoError(t, err)
	require.Len(t, ds, 3)

	byValue := map[string]Decision{}
	for _, d := range ds {
		byValue[d.Value] = d
	}

	ip := byValue["203.0.113.9"]
	assert.Equal(t, "ip", ip.Scope)
	assert.Equal(t, "ban", ip.Type)
	assert.Equal(t, "cscli", ip.Origin)
	assert.Equal(t, "belune: scanner", ip.Reason)
	// Duration is the time REMAINING, so the expiry is measured from now.
	assert.WithinDuration(t, now.Add(90*time.Minute), ip.ExpiresAt, 2*time.Second)
	assert.False(t, ip.CreatedAt.IsZero())

	rng := byValue["198.51.100.0/24"]
	assert.Equal(t, "range", rng.Scope)
	assert.WithinDuration(t, now.Add(24*time.Hour), rng.ExpiresAt, 2*time.Second)

	v6 := byValue["2001:db8::1"]
	assert.Equal(t, "ip", v6.Scope)
	assert.WithinDuration(t, now.Add(7*24*time.Hour), v6.ExpiresAt, 2*time.Second)
}

func TestParseDecisions_EmptyAndUnusableInput(t *testing.T) {
	now := time.Now()
	for _, in := range []string{"", "null", "[]", " \n"} {
		ds, err := parseDecisions(in, now)
		require.NoError(t, err, in)
		assert.Empty(t, ds, in)
	}
	_, err := parseDecisions("<html>", now)
	assert.Error(t, err)

	// A simulated decision is not enforced, and an expired one is not a ban.
	ds, err := parseDecisions(`[{"created_at":"2026-10-25T10:00:00Z","decisions":[
		{"id":1,"duration":"1h","origin":"cscli","scenario":"a","scope":"Ip","type":"ban","value":"203.0.113.1","simulated":true},
		{"id":2,"duration":"-5m","origin":"cscli","scenario":"b","scope":"Ip","type":"ban","value":"203.0.113.2"},
		{"id":3,"duration":"garbage","origin":"cscli","scenario":"c","scope":"Ip","type":"ban","value":"203.0.113.3"}
	]}]`, now)
	require.NoError(t, err)
	assert.Empty(t, ds)
}

func TestController_Bouncer(t *testing.T) {
	r := newFakeRunner().on("bouncers list", fixture(t, "bouncers-v1.8.1.json"), nil)
	c := NewController(r)

	b, found, err := c.Bouncer(context.Background(), BouncerName)
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, "v0.14.1", b.Version)
	assert.False(t, b.LastPull.IsZero())

	_, found, err = c.Bouncer(context.Background(), "someone-else")
	require.NoError(t, err)
	assert.False(t, found)

	// A bouncer that never pulled has last_pull null: that must read as "never",
	// not as an error.
	r.on("bouncers list", `[{"name":"belune-caddy","last_pull":null}]`, nil)
	b, found, err = c.Bouncer(context.Background(), BouncerName)
	require.NoError(t, err)
	assert.True(t, found)
	assert.True(t, b.LastPull.IsZero())
}

func TestController_Allowlist(t *testing.T) {
	r := newFakeRunner().on("allowlists list", fixture(t, "allowlists-v1.8.1.json"), nil)
	items, err := NewController(r).Allowlist(context.Background())
	require.NoError(t, err)
	require.Len(t, items, 2)
	assert.Equal(t, "192.0.2.10", items[0].Value)
	assert.Equal(t, "login: admin@example.com", items[0].Description)
	assert.False(t, items[0].ExpiresAt.IsZero())

	// No list yet is an empty allowlist, not an error.
	r.on("allowlists list", "[]", nil)
	items, err = NewController(r).Allowlist(context.Background())
	require.NoError(t, err)
	assert.Empty(t, items)
}

func TestController_BanArguments(t *testing.T) {
	r := newFakeRunner().on("decisions add", "", nil)
	c := NewController(r)

	require.NoError(t, c.Ban(context.Background(), netip.MustParsePrefix("203.0.113.9/32"), 90*time.Minute, "belune: x --ip 1.1.1.1"))
	require.NoError(t, c.Ban(context.Background(), netip.MustParsePrefix("198.51.100.0/24"), 24*time.Hour, "belune: y"))

	calls := r.called("decisions add")
	require.Len(t, calls, 2)
	// Every value is attached to its flag with "=", so text that looks like another
	// flag stays inside its own argument.
	assert.Equal(t, []string{"decisions", "add", "--ip=203.0.113.9", "--type=ban", "--duration=90m", "--reason=belune: x --ip 1.1.1.1"}, calls[0])
	assert.Equal(t, []string{"decisions", "add", "--range=198.51.100.0/24", "--type=ban", "--duration=1440m", "--reason=belune: y"}, calls[1])
	for _, call := range calls {
		assert.NotContains(t, call, "--bypass-allowlist", "Belune must never override the allowlist")
	}
}

func TestController_BanOfAnAllowlistedAddressIsErrAllowlisted(t *testing.T) {
	r := newFakeRunner().on("decisions add", "", &CommandError{
		Command: "decisions add", ExitCode: 1,
		Stderr: "Error: cscli decisions add: 192.0.2.0/24 is allowlisted by item 192.0.2.10 from belune-admins (x), use --bypass-allowlist to add the decision anyway",
	})
	err := NewController(r).Ban(context.Background(), netip.MustParsePrefix("192.0.2.0/24"), time.Hour, "belune: x")
	assert.ErrorIs(t, err, ErrAllowlisted)

	// Any other refusal stays what it is.
	r.on("decisions add", "", &CommandError{Command: "decisions add", ExitCode: 1, Stderr: "boom"})
	err = NewController(r).Ban(context.Background(), netip.MustParsePrefix("203.0.113.9/32"), time.Hour, "belune: x")
	assert.NotErrorIs(t, err, ErrAllowlisted)
	var ce *CommandError
	assert.ErrorAs(t, err, &ce)
}

func TestController_UnbanChecksThePostCondition(t *testing.T) {
	target := netip.MustParsePrefix("203.0.113.9/32")
	listing := `[{"created_at":"2026-10-25T10:00:00Z","decisions":[{"id":1,"duration":"1h","origin":"cscli","scenario":"belune: x","scope":"Ip","type":"ban","value":"203.0.113.9"}]}]`

	t.Run("gone", func(t *testing.T) {
		r := newFakeRunner().on("decisions delete", "", nil).on("decisions list", "[]", nil)
		require.NoError(t, NewController(r).Unban(context.Background(), target))
		assert.Equal(t, []string{"decisions", "delete", "--ip=203.0.113.9"}, r.called("decisions delete")[0])
	})

	t.Run("still there", func(t *testing.T) {
		// cscli exits 0 whether or not it deleted anything; only the read-back knows.
		r := newFakeRunner().on("decisions delete", "", nil).on("decisions list", listing, nil)
		err := NewController(r).Unban(context.Background(), target)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "still in force")
	})

	t.Run("a containing range is a different ban", func(t *testing.T) {
		// `--ip` lists range decisions that contain the address too. Lifting the
		// address does not lift the range, and must not be reported as failing to.
		rangeListing := `[{"created_at":"2026-10-25T10:00:00Z","decisions":[{"id":2,"duration":"1h","origin":"cscli","scenario":"x","scope":"Range","type":"ban","value":"203.0.113.0/24"}]}]`
		r := newFakeRunner().on("decisions delete", "", nil).on("decisions list", rangeListing, nil)
		require.NoError(t, NewController(r).Unban(context.Background(), target))
	})

	t.Run("delete fails", func(t *testing.T) {
		boom := errors.New("boom")
		r := newFakeRunner().on("decisions delete", "", boom)
		assert.ErrorIs(t, NewController(r).Unban(context.Background(), target), boom)
	})
}

func TestController_RegisterBouncerReadsTheKeyFromStdout(t *testing.T) {
	r := newFakeRunner().on("bouncers add", "M9m5uZu+ksfYPyn9fB6Hnv6g43cNks6TYin2B1UYuQw", nil)
	key, err := NewController(r).RegisterBouncer(context.Background(), BouncerName)
	require.NoError(t, err)
	assert.Equal(t, "M9m5uZu+ksfYPyn9fB6Hnv6g43cNks6TYin2B1UYuQw", key)

	call := r.called("bouncers add")[0]
	// The key is CrowdSec's to generate and print; it is never an argument, where a
	// process listing on the host could read it.
	assert.Equal(t, []string{"bouncers", "add", BouncerName, "-o", "raw"}, call)

	r.on("bouncers add", "  \n", nil)
	_, err = NewController(r).RegisterBouncer(context.Background(), BouncerName)
	assert.Error(t, err, "a registration that prints no key is not a registration")
}

func TestController_AllowRefreshesAnExistingEntry(t *testing.T) {
	target := netip.MustParsePrefix("192.0.2.10/32")

	t.Run("existing list: remove then add", func(t *testing.T) {
		r := newFakeRunner().
			on("allowlists list", fixture(t, "allowlists-v1.8.1.json"), nil).
			on("allowlists remove", "", nil).
			on("allowlists add", "", nil)
		require.NoError(t, NewController(r).Allow(context.Background(), target, "login: a@x", 30*24*time.Hour))

		// cscli keeps the OLD expiry when a value is added twice, so the entry is
		// removed first: a login has to restart the clock.
		var order []string
		for _, c := range r.calls {
			if c[0] == "allowlists" && c[1] != "list" {
				order = append(order, c[1])
			}
		}
		assert.Equal(t, []string{"remove", "add"}, order)
		assert.Equal(t, []string{"allowlists", "add", AllowlistName, "192.0.2.10", "-d", "login: a@x", "-e", "43200m"}, r.called("allowlists add")[0])
		assert.Empty(t, r.called("allowlists create"))
	})

	t.Run("missing list is created first", func(t *testing.T) {
		r := newFakeRunner().
			on("allowlists list", "[]", nil).
			on("allowlists create", "", nil).
			on("allowlists add", "", nil)
		require.NoError(t, NewController(r).Allow(context.Background(), target, "x", time.Hour))
		assert.Len(t, r.called("allowlists create"), 1)
		assert.Empty(t, r.called("allowlists remove"))
	})

	t.Run("a concurrent create is not a failure", func(t *testing.T) {
		r := newFakeRunner().
			on("allowlists list", "[]", nil).
			on("allowlists create", "", &CommandError{Command: "allowlists create", ExitCode: 1, Stderr: "allowlist 'belune-admins' already exists"}).
			on("allowlists add", "", nil)
		require.NoError(t, NewController(r).Allow(context.Background(), target, "x", time.Hour))
	})
}

func TestController_DeleteBouncerToleratesAbsence(t *testing.T) {
	r := newFakeRunner().on("bouncers delete", "", &CommandError{Command: "bouncers delete", ExitCode: 1, Stderr: "ent: bouncer not found"})
	assert.NoError(t, NewController(r).DeleteBouncer(context.Background(), BouncerName))
	r.on("bouncers delete", "", &CommandError{Command: "bouncers delete", ExitCode: 1, Stderr: "database is locked"})
	assert.Error(t, NewController(r).DeleteBouncer(context.Background(), BouncerName))
}
