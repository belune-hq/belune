package crowdsec

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/belune-hq/belune/internal/store/generated"
)

func candidate(ip string, requests int64, cats ...string) generated.WAFAutobanCandidatesRow {
	return generated.WAFAutobanCandidatesRow{
		ClientIp: netip.MustParseAddr(ip), Requests: requests, Categories: cats,
		LastSeen: pgtype.Timestamptz{Valid: true, Time: time.Now()},
	}
}

// decisionsJSON renders a decision listing the way cscli does.
func decisionsJSON(rows ...[3]string) string {
	out := "["
	for i, r := range rows {
		if i > 0 {
			out += ","
		}
		scope := "Ip"
		if len(r[0]) > 0 && contains(r[0], "/") {
			scope = "Range"
		}
		out += fmt.Sprintf(`{"created_at":"2026-10-25T10:00:00Z","decisions":[{"id":%d,"duration":"%s","origin":"cscli","scenario":%q,"scope":%q,"type":"ban","value":%q}]}`,
			i+1, r[2], r[1], scope, r[0])
	}
	return out + "]"
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

func autobanRig(t *testing.T, p Policy) *rig {
	t.Helper()
	r := newRig(t)
	r.enable(t)
	p.Enabled = true
	_, err := r.svc.SetAutoban(context.Background(), p)
	require.NoError(t, err)
	r.run.calls = nil
	return r
}

func policy() Policy {
	p := DefaultPolicy()
	p.Threshold, p.WindowMinutes, p.DurationMinutes, p.MaxActive = 10, 10, 60, 3
	return p
}

func TestEvaluateAutoban_OffDoesNothing(t *testing.T) {
	t.Run("feature off", func(t *testing.T) {
		r := newRig(t)
		_, err := r.svc.SetAutoban(context.Background(), func() Policy { p := policy(); p.Enabled = true; return p }())
		require.NoError(t, err)
		r.store.candidates = []generated.WAFAutobanCandidatesRow{candidate("203.0.113.9", 50, "attack-sqli")}
		require.NoError(t, r.svc.EvaluateAutoban(context.Background()))
		assert.Empty(t, r.run.calls)
	})
	t.Run("policy off", func(t *testing.T) {
		r := newRig(t)
		r.enable(t)
		r.store.candidates = []generated.WAFAutobanCandidatesRow{candidate("203.0.113.9", 50, "attack-sqli")}
		require.NoError(t, r.svc.EvaluateAutoban(context.Background()))
		assert.Empty(t, r.run.called("decisions"), "automatic bans are opt-in: a WAF detection never becomes a ban on its own")
	})
}

func TestEvaluateAutoban_BansAndExplainsWhy(t *testing.T) {
	r := autobanRig(t, policy())
	r.store.candidates = []generated.WAFAutobanCandidatesRow{
		candidate("203.0.113.9", 14, "attack-sqli", "attack-xss"),
	}

	require.NoError(t, r.svc.EvaluateAutoban(context.Background()))

	adds := r.run.called("decisions add")
	require.Len(t, adds, 1)
	assert.Equal(t, []string{"decisions", "add", "--ip=203.0.113.9", "--type=ban", "--duration=60m",
		"--reason=belune-waf: attack-sqli, attack-xss: 14 requests in 10m"}, adds[0])

	// The query is asked the policy's question, not a default one.
	q := r.store.lastParams
	assert.EqualValues(t, 10, q.Threshold)
	assert.Equal(t, policy().Categories, q.Categories)
	assert.WithinDuration(t, r.now.Add(-10*time.Minute), q.Since.Time, time.Second)

	assert.Equal(t, []string{"crowdsec_autoban"}, r.audit.actions())
	assert.Equal(t, "203.0.113.9", r.audit.rows[0].resourceID)
	assert.Empty(t, r.audit.rows[0].userID, "attributed to the system, not to a person")

	st := r.svc.Autoban()
	assert.EqualValues(t, 1, st.Banned)
	assert.Equal(t, 1, st.Active)
	assert.Empty(t, st.LastError)
}

func TestEvaluateAutoban_LeavesAloneWhatItShould(t *testing.T) {
	r := autobanRig(t, policy())
	r.run.on("decisions list", decisionsJSON(
		[3]string{"203.0.113.20", "belune: manual", "1h"}, // already banned by hand
		[3]string{"198.51.100.0/24", "crowdsec/x", "1h"},  // inside a banned range
	), nil)
	r.run.onFunc("decisions add", func(args []string) (string, error) {
		if args[2] == "--ip=203.0.113.40" {
			return "", &CommandError{Command: "decisions add", ExitCode: 1, Stderr: "203.0.113.40 is allowlisted by item"}
		}
		return "", nil
	})
	r.store.unbans = []string{"203.0.113.50"}
	r.store.candidates = []generated.WAFAutobanCandidatesRow{
		candidate("203.0.113.20", 99, "attack-sqli"), // already banned
		candidate("198.51.100.7", 99, "attack-sqli"), // covered by a range ban
		candidate("10.0.0.5", 99, "attack-sqli"),     // private: cannot be a visitor
		candidate("127.0.0.1", 99, "attack-sqli"),    // loopback
		candidate("203.0.113.40", 99, "attack-sqli"), // allowlisted: CrowdSec refuses
		candidate("203.0.113.50", 99, "attack-sqli"), // an admin lifted this ban lately
		candidate("203.0.113.60", 12, "attack-sqli"), // the only one that qualifies
	}

	require.NoError(t, r.svc.EvaluateAutoban(context.Background()))

	var banned []string
	for _, c := range r.run.called("decisions add") {
		banned = append(banned, c[2])
	}
	assert.Equal(t, []string{"--ip=203.0.113.40", "--ip=203.0.113.60"}, banned,
		"203.0.113.40 is attempted (CrowdSec owns the allowlist) and refused; nothing else is")

	st := r.svc.Autoban()
	assert.EqualValues(t, 1, st.Banned)
	assert.Equal(t, 4, st.Skipped, "private, loopback, allowlisted and lifted")
	assert.Equal(t, []string{"crowdsec_autoban"}, r.audit.actions(), "only a real ban is audited")
}

func TestEvaluateAutoban_StopsAtTheCapAndSaysSo(t *testing.T) {
	r := autobanRig(t, policy()) // cap of 3
	r.run.on("decisions list", decisionsJSON(
		[3]string{"203.0.113.1", "belune-waf: attack-xss: 11 requests in 10m", "1h"},
		[3]string{"203.0.113.2", "belune-waf: attack-xss: 11 requests in 10m", "1h"},
		// By hand, so it does not count against the cap of automatic bans.
		[3]string{"203.0.113.3", "belune: manual", "1h"},
	), nil)
	r.store.candidates = []generated.WAFAutobanCandidatesRow{
		candidate("203.0.113.10", 90, "attack-sqli"),
		candidate("203.0.113.11", 80, "attack-sqli"),
		candidate("203.0.113.12", 70, "attack-sqli"),
	}

	require.NoError(t, r.svc.EvaluateAutoban(context.Background()))

	adds := r.run.called("decisions add")
	require.Len(t, adds, 1, "two automatic bans were already in force; the cap of three leaves room for one")
	assert.Equal(t, "--ip=203.0.113.10", adds[0][2], "worst offender first")

	st := r.svc.Autoban()
	assert.True(t, st.Capped)
	assert.Equal(t, 2, st.Skipped)
	assert.Equal(t, 3, st.Active)

	// And the next pass, with the cap still full, adds nothing.
	r.run.calls = nil
	r.run.on("decisions list", decisionsJSON(
		[3]string{"203.0.113.1", "belune-waf: x", "1h"},
		[3]string{"203.0.113.2", "belune-waf: x", "1h"},
		[3]string{"203.0.113.10", "belune-waf: x", "1h"},
	), nil)
	require.NoError(t, r.svc.EvaluateAutoban(context.Background()))
	assert.Empty(t, r.run.called("decisions add"))
}

func TestEvaluateAutoban_AFailureIsReportedAndStopsTheRun(t *testing.T) {
	r := autobanRig(t, policy())
	r.run.on("decisions add", "", errors.New("database is locked"))
	r.store.candidates = []generated.WAFAutobanCandidatesRow{
		candidate("203.0.113.10", 90, "attack-sqli"),
		candidate("203.0.113.11", 80, "attack-sqli"),
	}

	err := r.svc.EvaluateAutoban(context.Background())
	require.Error(t, err)
	assert.Len(t, r.run.called("decisions add"), 1, "a broken local API is not hammered once per candidate")
	assert.NotEmpty(t, r.svc.Autoban().LastError)

	// CrowdSec unreachable altogether.
	r.run.on("decisions list", "", ErrUnavailable)
	require.Error(t, r.svc.EvaluateAutoban(context.Background()))
}

func TestPolicyValidate_TheCategoryListIsClosed(t *testing.T) {
	for _, c := range BanCategories {
		p := policy()
		p.Categories = []string{c}
		assert.NoError(t, p.Validate(), c)
	}
	// The reason this list exists: 920420 fired on benign text/plain POSTs, and its
	// severity is CRITICAL like a real attack's.
	for _, c := range []string{"attack-protocol", "attack-reputation-scanner", "attack-generic", "attack-multipart-header", "attack-disclosure", "attack-fixation", ""} {
		p := policy()
		p.Categories = []string{c}
		assert.Error(t, p.Validate(), "%q must never be ban-eligible", c)
	}
}
