package crowdsec

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/belune-hq/belune/internal/proxy"
	"github.com/belune-hq/belune/internal/store/generated"
)

// memStore is Store over one in-memory row, with the same "no row until the first
// write" behaviour the table has.
type memStore struct {
	mu         sync.Mutex
	row        *generated.CrowdsecConfig
	candidates []generated.WAFAutobanCandidatesRow
	lastParams generated.WAFAutobanCandidatesParams
	unbans     []string
}

func (m *memStore) upsert(f func(*generated.CrowdsecConfig)) generated.CrowdsecConfig {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.row == nil {
		m.row = &generated.CrowdsecConfig{ID: "default", AutobanCategories: DefaultPolicy().Categories,
			AutobanThreshold: 10, AutobanWindowMinutes: 10, AutobanDurationMinutes: 60, AutobanMaxActive: 25}
	}
	f(m.row)
	return *m.row
}

func (m *memStore) GetCrowdSecConfig(context.Context) (generated.CrowdsecConfig, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.row == nil {
		return generated.CrowdsecConfig{}, pgx.ErrNoRows
	}
	return *m.row, nil
}

func (m *memStore) SetCrowdSecEnabled(_ context.Context, on bool) (generated.CrowdsecConfig, error) {
	return m.upsert(func(r *generated.CrowdsecConfig) { r.Enabled = on }), nil
}

func (m *memStore) ReplaceCrowdSecBouncerKey(_ context.Context, key []byte) (generated.CrowdsecConfig, error) {
	return m.upsert(func(r *generated.CrowdsecConfig) { r.BouncerKeyEncrypted = key }), nil
}

func (m *memStore) SetCrowdSecAutoban(_ context.Context, a generated.SetCrowdSecAutobanParams) (generated.CrowdsecConfig, error) {
	return m.upsert(func(r *generated.CrowdsecConfig) {
		r.AutobanEnabled, r.AutobanThreshold, r.AutobanWindowMinutes = a.AutobanEnabled, a.AutobanThreshold, a.AutobanWindowMinutes
		r.AutobanDurationMinutes, r.AutobanMaxActive, r.AutobanCategories = a.AutobanDurationMinutes, a.AutobanMaxActive, a.AutobanCategories
	}), nil
}

func (m *memStore) WAFAutobanCandidates(_ context.Context, a generated.WAFAutobanCandidatesParams) ([]generated.WAFAutobanCandidatesRow, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.lastParams = a
	return m.candidates, nil
}

func (m *memStore) ListCrowdSecUnbansSince(context.Context, pgtype.Timestamptz) ([]pgtype.Text, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]pgtype.Text, 0, len(m.unbans))
	for _, u := range m.unbans {
		out = append(out, pgtype.Text{String: u, Valid: true})
	}
	return out, nil
}

// xorCipher is not encryption; it makes ciphertext differ from plaintext so a test
// can tell a sealed value from a stored-in-the-clear one.
type xorCipher struct{}

func (xorCipher) Encrypt(p []byte) ([]byte, error) { return append([]byte("sealed:"), p...), nil }
func (xorCipher) Decrypt(c []byte) ([]byte, error) {
	if len(c) < 7 || string(c[:7]) != "sealed:" {
		return nil, errors.New("not sealed")
	}
	return c[7:], nil
}

type auditRow struct {
	userID, action, resourceType, resourceID string
	details                                  map[string]any
}

type recordingAudit struct {
	mu   sync.Mutex
	rows []auditRow
}

func (a *recordingAudit) Log(userID, _, _, action, resourceType, resourceID string, details map[string]any) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.rows = append(a.rows, auditRow{userID, action, resourceType, resourceID, details})
}

func (a *recordingAudit) actions() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	var out []string
	for _, r := range a.rows {
		out = append(out, r.action)
	}
	return out
}

type rig struct {
	svc   *Service
	store *memStore
	run   *fakeRunner
	audit *recordingAudit
	now   time.Time
}

func newRig(t *testing.T) *rig {
	t.Helper()
	r := &rig{store: &memStore{}, run: newFakeRunner(), audit: &recordingAudit{}, now: time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)}
	r.svc = NewService(r.store, NewController(r.run), xorCipher{}, r.audit)
	r.svc.now = func() time.Time { return r.now }
	// An empty, healthy local API by default.
	r.run.on("bouncers list", "[]", nil).
		on("decisions list", "[]", nil).
		on("allowlists list", "[]", nil).
		on("allowlists create", "", nil).
		on("allowlists add", "", nil).
		on("allowlists remove", "", nil).
		on("decisions add", "", nil)
	return r
}

func (r *rig) enable(t *testing.T) {
	t.Helper()
	_, err := r.svc.SetEnabled(context.Background(), true, "")
	require.NoError(t, err)
}

func TestConfig_NoRowIsOffWithDefaultPolicy(t *testing.T) {
	r := newRig(t)
	cfg, err := r.svc.Config(context.Background())
	require.NoError(t, err)
	assert.False(t, cfg.Enabled)
	assert.False(t, cfg.HasKey)
	assert.Equal(t, DefaultPolicy(), cfg.Autoban)
	assert.False(t, cfg.Autoban.Enabled, "automatic bans are opt-in")
	assert.NotContains(t, cfg.Autoban.Categories, "attack-protocol")
}

func TestSetEnabled_AllowlistsTheAdminBeforeTheSwitchTurnsOn(t *testing.T) {
	r := newRig(t)
	var enabledWhenAllowed *bool
	r.run.onFunc("allowlists add", func([]string) (string, error) {
		row, _ := r.store.GetCrowdSecConfig(context.Background())
		v := row.Enabled
		enabledWhenAllowed = &v
		return "", nil
	})

	cfg, err := r.svc.SetEnabled(context.Background(), true, "203.0.113.50")
	require.NoError(t, err)
	assert.True(t, cfg.Enabled)

	require.NotNil(t, enabledWhenAllowed, "the admin's own address must be allowlisted")
	assert.False(t, *enabledWhenAllowed, "...and before enforcement can start, not after")
	assert.Equal(t, "203.0.113.50", r.run.called("allowlists add")[0][3])
}

func TestSetEnabled_DoesNotAllowlistWhatCannotBeBanned(t *testing.T) {
	for _, ip := range []string{"", "not-an-ip", "10.0.0.5", "127.0.0.1", "172.18.0.1"} {
		r := newRig(t)
		_, err := r.svc.SetEnabled(context.Background(), true, ip)
		require.NoError(t, err, ip)
		assert.Empty(t, r.run.called("allowlists"), ip)
	}
}

func TestSetEnabled_ASafetyNetFailureIsNotAFailure(t *testing.T) {
	r := newRig(t)
	r.run.on("allowlists list", "", ErrUnavailable)
	cfg, err := r.svc.SetEnabled(context.Background(), true, "203.0.113.50")
	require.NoError(t, err, "an absent CrowdSec must not stop the switch being saved")
	assert.True(t, cfg.Enabled)
}

func TestSetEnabled_DisablingTouchesNothing(t *testing.T) {
	r := newRig(t)
	_, err := r.svc.SetEnabled(context.Background(), false, "203.0.113.50")
	require.NoError(t, err)
	assert.Empty(t, r.run.calls)
}

func TestPrepare_OffMakesNoCalls(t *testing.T) {
	r := newRig(t)
	d, err := r.svc.Prepare(context.Background())
	require.NoError(t, err)
	assert.False(t, d.Enabled)
	assert.Empty(t, r.run.calls, "a feature that is off must not cost a docker exec per reconcile pass")
}

func TestPrepare_UnreachableIsNotReadyAndNotAnError(t *testing.T) {
	r := newRig(t)
	r.enable(t)
	r.run.on("bouncers list", "", ErrUnavailable)

	d, err := r.svc.Prepare(context.Background())
	require.NoError(t, err)
	assert.True(t, d.Enabled)
	assert.False(t, d.Ready)
	assert.Equal(t, proxy.ReasonCrowdSecUnavailable, d.Reason)
	assert.Empty(t, d.APIKey)
}

func TestPrepare_RegistersTheBouncerAndSealsItsKey(t *testing.T) {
	r := newRig(t)
	r.enable(t)
	r.run.on("bouncers add", "KEY-FROM-CROWDSEC", nil)

	d, err := r.svc.Prepare(context.Background())
	require.NoError(t, err)
	assert.True(t, d.Ready)
	assert.Equal(t, "KEY-FROM-CROWDSEC", d.APIKey)
	assert.Equal(t, APIURL, d.APIURL)

	row, _ := r.store.GetCrowdSecConfig(context.Background())
	assert.Equal(t, "sealed:KEY-FROM-CROWDSEC", string(row.BouncerKeyEncrypted), "the key is stored sealed")
}

func TestPrepare_KeepsAnExistingRegistration(t *testing.T) {
	r := newRig(t)
	r.enable(t)
	r.store.upsert(func(c *generated.CrowdsecConfig) { c.BouncerKeyEncrypted = []byte("sealed:OLD") })
	r.run.on("bouncers list", `[{"name":"belune-caddy","last_pull":null}]`, nil)

	d, err := r.svc.Prepare(context.Background())
	require.NoError(t, err)
	assert.True(t, d.Ready)
	assert.Equal(t, "OLD", d.APIKey)
	assert.Empty(t, r.run.called("bouncers add"))
	assert.Empty(t, r.run.called("bouncers delete"))
}

func TestPrepare_AnOrphanedBouncerIsReplaced(t *testing.T) {
	// Registered, no key stored: the process died between the two steps. The key
	// cannot be recovered, so the registration is redone.
	r := newRig(t)
	r.enable(t)
	r.run.on("bouncers list", `[{"name":"belune-caddy","last_pull":null}]`, nil).
		on("bouncers delete", "", nil).
		on("bouncers add", "NEW", nil)

	d, err := r.svc.Prepare(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "NEW", d.APIKey)
	assert.Len(t, r.run.called("bouncers delete"), 1)
	assert.Len(t, r.run.called("bouncers add"), 1)
}

func TestPrepare_AWipedVolumeGetsAFreshKey(t *testing.T) {
	// A key is stored but the local API has no such bouncer: its volume was lost.
	// The stored key is useless and is replaced, which the reconciler then pushes to
	// Caddy.
	r := newRig(t)
	r.enable(t)
	r.store.upsert(func(c *generated.CrowdsecConfig) { c.BouncerKeyEncrypted = []byte("sealed:STALE") })
	r.run.on("bouncers add", "FRESH", nil)

	d, err := r.svc.Prepare(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "FRESH", d.APIKey)
}

func TestPrepare_ConcurrentCallsRegisterOnce(t *testing.T) {
	r := newRig(t)
	r.enable(t)

	var registered atomic.Bool
	r.run.onFunc("bouncers list", func([]string) (string, error) {
		if registered.Load() {
			return `[{"name":"belune-caddy","last_pull":null}]`, nil
		}
		return "[]", nil
	}).onFunc("bouncers add", func([]string) (string, error) {
		time.Sleep(20 * time.Millisecond)
		registered.Store(true)
		return "ONLY-KEY", nil
	})
	// Without a cache hit saving the day: each caller must reach the lock.
	r.svc.now = time.Now

	var wg sync.WaitGroup
	keys := make(chan string, 8)
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			d, err := r.svc.Prepare(context.Background())
			if assert.NoError(t, err) {
				keys <- d.APIKey
			}
		}()
	}
	wg.Wait()
	close(keys)

	assert.Len(t, r.run.called("bouncers add"), 1, "two registrations would leave a key nobody stored")
	for k := range keys {
		assert.Equal(t, "ONLY-KEY", k)
	}
}

func TestRotateKey(t *testing.T) {
	r := newRig(t)
	r.store.upsert(func(c *generated.CrowdsecConfig) { c.BouncerKeyEncrypted = []byte("sealed:OLD") })
	r.run.on("bouncers delete", "", nil).on("bouncers add", "ROTATED", nil)

	require.NoError(t, r.svc.RotateKey(context.Background()))
	row, _ := r.store.GetCrowdSecConfig(context.Background())
	assert.Equal(t, "sealed:ROTATED", string(row.BouncerKeyEncrypted))
}

func TestBan_RefusesWhatItMust(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name, target, duration, clientIP string
		want                             any
	}{
		{"private", "10.0.0.5", "1h", "", &ErrNotBannable{}},
		{"loopback", "127.0.0.1", "1h", "", &ErrNotBannable{}},
		{"too wide", "203.0.0.0/8", "1h", "", &ErrNotBannable{}},
		{"yourself", "203.0.113.50", "1h", "203.0.113.50", &ErrNotBannable{}},
		{"your range", "203.0.113.0/24", "1h", "203.0.113.50", &ErrNotBannable{}},
		{"yourself, mapped", "203.0.113.50", "1h", "::ffff:203.0.113.50", &ErrNotBannable{}},
		{"garbage target", "garbage", "1h", "", &InvalidError{}},
		{"forever", "203.0.113.9", "999999h", "", &InvalidError{}},
		{"no duration", "203.0.113.9", "", "", &InvalidError{}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := newRig(t)
			_, err := r.svc.Ban(ctx, c.target, c.duration, "x", c.clientIP)
			require.Error(t, err)
			switch c.want.(type) {
			case *ErrNotBannable:
				var e *ErrNotBannable
				assert.ErrorAs(t, err, &e)
			case *InvalidError:
				var e *InvalidError
				assert.ErrorAs(t, err, &e)
			}
			assert.Empty(t, r.run.called("decisions add"), "a refused ban must never reach CrowdSec")
		})
	}
}

func TestBan_TagsTheReasonAndPassesTheLength(t *testing.T) {
	r := newRig(t)
	d, err := r.svc.Ban(context.Background(), "203.0.113.9", "2h", "  port\nscanner  ", "198.51.100.1")
	require.NoError(t, err)
	assert.Equal(t, "belune: port scanner", d.Reason)
	assert.Equal(t, r.now.Add(2*time.Hour), d.ExpiresAt)

	call := r.run.called("decisions add")[0]
	assert.Contains(t, call, "--reason=belune: port scanner")
	assert.Contains(t, call, "--duration=120m")

	d, err = r.svc.Ban(context.Background(), "203.0.113.10", "1h", "", "")
	require.NoError(t, err)
	assert.Equal(t, "belune: manual ban", d.Reason)
}

func TestBan_AnAllowlistedAddressIsRefusedByCrowdSec(t *testing.T) {
	r := newRig(t)
	r.run.on("decisions add", "", &CommandError{Command: "decisions add", ExitCode: 1, Stderr: "203.0.113.9 is allowlisted by item"})
	_, err := r.svc.Ban(context.Background(), "203.0.113.9", "1h", "", "")
	assert.ErrorIs(t, err, ErrAllowlisted)
}

func TestNoteAdminLogin(t *testing.T) {
	waitFor := func(t *testing.T, r *rig, want int) {
		t.Helper()
		require.Eventually(t, func() bool { return len(r.run.called("allowlists add")) == want }, 2*time.Second, 5*time.Millisecond)
	}

	t.Run("allowlists the address when on", func(t *testing.T) {
		r := newRig(t)
		r.enable(t)
		r.svc.NoteAdminLogin("admin@example.com", "203.0.113.50")
		waitFor(t, r, 1)
		call := r.run.called("allowlists add")[0]
		assert.Equal(t, "203.0.113.50", call[3])
		assert.Contains(t, call, "login: admin@example.com")
		assert.Contains(t, call, "43200m", "30 days, restarted by every login")
	})

	t.Run("does nothing when off", func(t *testing.T) {
		r := newRig(t)
		r.svc.NoteAdminLogin("admin@example.com", "203.0.113.50")
		time.Sleep(50 * time.Millisecond)
		assert.Empty(t, r.run.calls)
	})

	t.Run("does nothing for an address that cannot be banned", func(t *testing.T) {
		r := newRig(t)
		r.enable(t)
		r.run.calls = nil
		r.svc.NoteAdminLogin("admin@example.com", "192.168.1.20")
		time.Sleep(50 * time.Millisecond)
		assert.Empty(t, r.run.calls)
	})

	t.Run("returns at once and survives CrowdSec being down", func(t *testing.T) {
		r := newRig(t)
		r.enable(t)
		release := make(chan struct{})
		r.run.onFunc("allowlists list", func([]string) (string, error) { <-release; return "", ErrUnavailable })

		returned := make(chan struct{})
		go func() { r.svc.NoteAdminLogin("a@x", "203.0.113.50"); close(returned) }()
		select {
		case <-returned:
		case <-time.After(time.Second):
			t.Fatal("NoteAdminLogin blocked on CrowdSec: a login would hang behind it")
		}
		close(release)
	})
}

func TestSetAutoban_ValidatesAndNormalises(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	good := Policy{Enabled: true, Threshold: 20, WindowMinutes: 15, DurationMinutes: 120, MaxActive: 10,
		Categories: []string{"attack-xss", "attack-sqli", "attack-xss"}}

	cfg, err := r.svc.SetAutoban(ctx, good)
	require.NoError(t, err)
	assert.Equal(t, []string{"attack-sqli", "attack-xss"}, cfg.Autoban.Categories, "sorted and de-duplicated")
	assert.True(t, cfg.Autoban.Enabled)

	bad := map[string]func(p *Policy){
		"threshold below the floor": func(p *Policy) { p.Threshold = 2 },
		"threshold above":           func(p *Policy) { p.Threshold = 1001 },
		"window zero":               func(p *Policy) { p.WindowMinutes = 0 },
		"window over a day":         func(p *Policy) { p.WindowMinutes = 1441 },
		"ban under five minutes":    func(p *Policy) { p.DurationMinutes = 4 },
		"ban over a week":           func(p *Policy) { p.DurationMinutes = 10081 },
		"cap zero":                  func(p *Policy) { p.MaxActive = 0 },
		"cap over 500":              func(p *Policy) { p.MaxActive = 501 },
		"protocol category":         func(p *Policy) { p.Categories = []string{"attack-protocol"} },
		"scanner category":          func(p *Policy) { p.Categories = []string{"attack-reputation-scanner"} },
		"made-up category":          func(p *Policy) { p.Categories = []string{"attack-xss", "nonsense"} },
		"on with no category":       func(p *Policy) { p.Categories = nil },
	}
	for name, mutate := range bad {
		t.Run(name, func(t *testing.T) {
			p := good
			p.Categories = append([]string(nil), good.Categories...)
			mutate(&p)
			_, err := r.svc.SetAutoban(ctx, p)
			var invalid *InvalidError
			assert.ErrorAs(t, err, &invalid)
		})
	}

	// Off with no categories is a legitimate way to switch it off.
	off := good
	off.Enabled, off.Categories = false, nil
	_, err = r.svc.SetAutoban(ctx, off)
	assert.NoError(t, err)
}
