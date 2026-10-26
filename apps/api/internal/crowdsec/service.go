package crowdsec

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/belune-hq/belune/internal/pkg/redact"
	"github.com/belune-hq/belune/internal/proxy"
	"github.com/belune-hq/belune/internal/store/generated"
)

const (
	// AllowlistTTL is how long a login keeps its address on the allowlist. Every
	// login starts the clock again; an admin who has not logged in for a month is
	// not holding a stale address that someone else may now be using.
	AllowlistTTL = 30 * 24 * time.Hour

	// healthTTL is how long a status read may reuse the last check. The reconciler
	// refreshes it every pass; this only stops a page that polls from turning each
	// poll into a docker exec.
	healthTTL = 5 * time.Second

	// loginTimeout bounds the background allowlisting that follows a login.
	loginTimeout = 30 * time.Second
)

// Store is the slice of the database Service uses.
type Store interface {
	GetCrowdSecConfig(ctx context.Context) (generated.CrowdsecConfig, error)
	SetCrowdSecEnabled(ctx context.Context, enabled bool) (generated.CrowdsecConfig, error)
	ReplaceCrowdSecBouncerKey(ctx context.Context, key []byte) (generated.CrowdsecConfig, error)
	SetCrowdSecAutoban(ctx context.Context, arg generated.SetCrowdSecAutobanParams) (generated.CrowdsecConfig, error)
	WAFAutobanCandidates(ctx context.Context, arg generated.WAFAutobanCandidatesParams) ([]generated.WAFAutobanCandidatesRow, error)
	ListCrowdSecUnbansSince(ctx context.Context, since pgtype.Timestamptz) ([]pgtype.Text, error)
}

var _ Store = (*generated.Queries)(nil)

// Cipher seals the bouncer key.
type Cipher interface {
	Encrypt(plaintext []byte) ([]byte, error)
	Decrypt(ciphertext []byte) ([]byte, error)
}

// Auditor records who did what. Satisfied by *service.AuditService.
type Auditor interface {
	Log(userID, tokenID, ipAddress, action, resourceType, resourceID string, details map[string]any)
}

// Policy is the rule for turning WAF detections into bans.
type Policy struct {
	Enabled         bool     `json:"enabled"`
	Threshold       int      `json:"threshold"`
	WindowMinutes   int      `json:"window_minutes"`
	DurationMinutes int      `json:"duration_minutes"`
	MaxActive       int      `json:"max_active"`
	Categories      []string `json:"categories"`
}

// Config is the stored configuration, without the key.
type Config struct {
	Enabled bool `json:"enabled"`
	// HasKey is whether a bouncer key has been generated. The key itself leaves
	// this package only to go to Caddy.
	HasKey  bool   `json:"has_key"`
	Autoban Policy `json:"autoban"`
}

// Health is what the last look at the local API found.
type Health struct {
	CheckedAt time.Time `json:"checked_at"`
	// Reachable is whether the local API answered.
	Reachable bool `json:"reachable"`
	// BouncerRegistered is whether Caddy's bouncer exists there.
	BouncerRegistered bool `json:"bouncer_registered"`
	// LastPull is when the bouncer last fetched the ban list, zero if never. A
	// bouncer that cannot authenticate stops pulling and Caddy keeps serving
	// without enforcing, with nothing visibly wrong; this is where that shows.
	LastPull time.Time `json:"last_pull,omitzero"`
	Error    string    `json:"error,omitempty"`
}

// Service is Belune's CrowdSec logic.
type Service struct {
	store  Store
	ctl    *Controller
	cipher Cipher
	audit  Auditor
	now    func() time.Time

	// regMu serialises bouncer registration. Two admins pressing Enable together
	// must not each register a bouncer and each store a different key.
	regMu sync.Mutex

	mu     sync.Mutex
	health Health

	autoban autobanRun
}

func NewService(store Store, ctl *Controller, cipher Cipher, audit Auditor) *Service {
	return &Service{store: store, ctl: ctl, cipher: cipher, audit: audit, now: time.Now}
}

// DefaultPolicy is the policy of an install that never touched it.
func DefaultPolicy() Policy {
	return Policy{
		Threshold:       10,
		WindowMinutes:   10,
		DurationMinutes: 60,
		MaxActive:       25,
		Categories:      []string{"attack-injection-java", "attack-injection-php", "attack-lfi", "attack-rce", "attack-rfi", "attack-sqli", "attack-xss"},
	}
}

func configFrom(row generated.CrowdsecConfig) Config {
	cats := row.AutobanCategories
	if cats == nil {
		cats = []string{}
	}
	return Config{
		Enabled: row.Enabled,
		HasKey:  len(row.BouncerKeyEncrypted) > 0,
		Autoban: Policy{
			Enabled:         row.AutobanEnabled,
			Threshold:       int(row.AutobanThreshold),
			WindowMinutes:   int(row.AutobanWindowMinutes),
			DurationMinutes: int(row.AutobanDurationMinutes),
			MaxActive:       int(row.AutobanMaxActive),
			Categories:      cats,
		},
	}
}

func (s *Service) row(ctx context.Context) (generated.CrowdsecConfig, error) {
	row, err := s.store.GetCrowdSecConfig(ctx)
	if errors.Is(err, pgx.ErrNoRows) {
		return generated.CrowdsecConfig{}, nil
	}
	return row, err
}

// Config reads the stored configuration. No row is the default: off.
func (s *Service) Config(ctx context.Context) (Config, error) {
	row, err := s.row(ctx)
	if err != nil {
		return Config{}, err
	}
	if row.ID == "" {
		return Config{Autoban: DefaultPolicy()}, nil
	}
	return configFrom(row), nil
}

// Decisions lists the bans in force.
func (s *Service) Decisions(ctx context.Context) ([]Decision, error) { return s.ctl.Decisions(ctx) }

// Allowlist lists Belune's allowlist.
func (s *Service) Allowlist(ctx context.Context) ([]AllowItem, error) { return s.ctl.Allowlist(ctx) }

// SetEnabled stores the switch. Turning it on first allowlists the address the
// admin is acting from, so the ban list the local API may already hold from an
// earlier run cannot catch the person who has just switched this on.
func (s *Service) SetEnabled(ctx context.Context, enabled bool, clientIP string) (Config, error) {
	if enabled {
		s.allowAddress(ctx, clientIP, "enabled CrowdSec")
	}
	row, err := s.store.SetCrowdSecEnabled(ctx, enabled)
	if err != nil {
		return Config{}, fmt.Errorf("saving the CrowdSec switch: %w", err)
	}
	return configFrom(row), nil
}

// allowAddress allowlists the address an admin is acting from, when it can be
// banned at all. Failure is logged and swallowed: it is a safety net, never a gate.
func (s *Service) allowAddress(ctx context.Context, clientIP, why string) {
	target, ok := bannableAddress(clientIP)
	if !ok {
		return
	}
	if err := s.ctl.Allow(ctx, target, CleanReason(why), AllowlistTTL); err != nil {
		// No CrowdSec container is the ordinary case of an install that has not had
		// its host update yet, not something to warn about on every login.
		if errors.Is(err, ErrUnavailable) {
			slog.Debug("crowdsec: not allowlisting the acting admin, CrowdSec is not running")
			return
		}
		slog.Warn("crowdsec: could not allowlist the acting admin", "error", redact.Error(err.Error()))
	}
}

// Protection says how an address stands against being banned, for the page that
// tells an admin about their own.
type Protection string

const (
	// ProtectionUnbannable is a private, loopback or reserved address: no ban can
	// name it, so it needs no allowlist entry and never gets one.
	ProtectionUnbannable Protection = "unbannable"
	// ProtectionAllowlisted is a public address that an unexpired allowlist entry
	// covers. It is safe for as long as the entry stands, and no longer.
	ProtectionAllowlisted Protection = "allowlisted"
	// ProtectionExposed is a public address nothing covers: it can be banned. The
	// allowlisting is a safety net that did not happen, or has expired.
	ProtectionExposed Protection = "exposed"
	// ProtectionUnknown is a public address whose standing could not be read,
	// because the local API did not answer. No claim is made either way.
	ProtectionUnknown Protection = "unknown"
)

// Protection reports whether clientIP can be banned, and if so whether it is
// allowlisted right now. It reads the allowlist, not the switch: the switch being
// on does not mean the address was added (the local API may have been down), and
// it being off does not mean it was not (an earlier run may have added it).
func (s *Service) Protection(ctx context.Context, clientIP string) Protection {
	target, ok := bannableAddress(clientIP)
	if !ok {
		// Either it cannot be banned, or it is not an address at all; only the first
		// is a claim that can be made.
		if ip, err := netip.ParseAddr(clientIP); err == nil && CheckBannable(netip.PrefixFrom(ip.Unmap(), ip.Unmap().BitLen())) != nil {
			return ProtectionUnbannable
		}
		return ProtectionUnknown
	}
	if !s.Look(ctx).Reachable {
		return ProtectionUnknown
	}
	items, err := s.ctl.Allowlist(ctx)
	if err != nil {
		return ProtectionUnknown
	}
	now := s.now()
	for _, it := range items {
		covers, err := ParseTarget(it.Value)
		if err != nil || !covers.Contains(target.Addr()) {
			continue
		}
		if it.ExpiresAt.IsZero() || it.ExpiresAt.After(now) {
			return ProtectionAllowlisted
		}
	}
	return ProtectionExposed
}

// bannableAddress parses an address and says whether a ban could name it. The one
// definition, shared by what allowlists an admin and what tells them about it, so
// the two cannot come to disagree.
func bannableAddress(clientIP string) (netip.Prefix, bool) {
	ip, err := netip.ParseAddr(clientIP)
	if err != nil {
		return netip.Prefix{}, false
	}
	ip = ip.Unmap()
	target := netip.PrefixFrom(ip, ip.BitLen())
	if CheckBannable(target) != nil {
		return netip.Prefix{}, false
	}
	return target, true
}

// NoteAdminLogin allowlists the address an admin has just logged in from. It
// runs in the background and cannot fail the login: a CrowdSec that is down,
// absent or slow must never be a reason someone cannot sign in.
//
// This is what makes bans safe to use. A ban that catches the admin's own address
// is undone by the admin's next login, because adding an address to the allowlist
// also deletes any ban already in force on it.
func (s *Service) NoteAdminLogin(email, ip string) {
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), loginTimeout)
		defer cancel()
		cfg, err := s.Config(ctx)
		if err != nil || !cfg.Enabled {
			return
		}
		s.allowAddress(ctx, ip, "login: "+email)
	}()
}

// Ban bans an address or range for duration, on behalf of who.
func (s *Service) Ban(ctx context.Context, rawTarget, rawDuration, rawReason, clientIP string) (Decision, error) {
	target, err := ParseTarget(rawTarget)
	if err != nil {
		return Decision{}, &InvalidError{err}
	}
	duration, err := ParseDuration(rawDuration)
	if err != nil {
		return Decision{}, &InvalidError{err}
	}
	if err := CheckBannable(target); err != nil {
		return Decision{}, err
	}
	// Whoever is asking is never banned by their own hand. They are allowlisted
	// at login, but a session that predates the switch has no entry yet.
	if ip, err := netip.ParseAddr(clientIP); err == nil && target.Contains(ip.Unmap()) {
		return Decision{}, &ErrNotBannable{Reason: "that would ban the address you are connected from"}
	}

	reason := ReasonPrefixManual + "manual ban"
	if r := CleanReason(rawReason); r != "" {
		reason = ReasonPrefixManual + r
	}
	if err := s.ctl.Ban(ctx, target, duration, reason); err != nil {
		return Decision{}, err
	}

	return Decision{Value: targetValue(target), Reason: reason, ExpiresAt: s.now().Add(duration)}, nil
}

// Unban lifts every ban on exactly this address or range.
func (s *Service) Unban(ctx context.Context, rawTarget string) error {
	target, err := ParseTarget(rawTarget)
	if err != nil {
		return &InvalidError{err}
	}
	return s.ctl.Unban(ctx, target)
}

// AllowTarget adds an address or range to the allowlist by hand, for as long as
// duration (an office or VPN address, say).
func (s *Service) AllowTarget(ctx context.Context, rawTarget, rawDuration, rawNote string) error {
	target, err := ParseTarget(rawTarget)
	if err != nil {
		return &InvalidError{err}
	}
	duration, err := ParseDuration(rawDuration)
	if err != nil {
		return &InvalidError{err}
	}
	note := "added by hand"
	if n := CleanReason(rawNote); n != "" {
		note = n
	}
	return s.ctl.Allow(ctx, target, note, duration)
}

// UnallowTarget removes an entry from the allowlist.
func (s *Service) UnallowTarget(ctx context.Context, rawTarget string) error {
	target, err := ParseTarget(rawTarget)
	if err != nil {
		return &InvalidError{err}
	}
	return s.ctl.Unallow(ctx, target)
}

// RotateKey replaces the bouncer key: for a bouncer that has stopped pulling, most
// often because its entry was changed by hand in the local API.
func (s *Service) RotateKey(ctx context.Context) error {
	s.regMu.Lock()
	defer s.regMu.Unlock()
	if err := s.ctl.DeleteBouncer(ctx, BouncerName); err != nil {
		return err
	}
	return s.register(ctx)
}

// register creates the bouncer and stores the key CrowdSec generated for it. The
// caller holds regMu.
//
// If the process dies between the two, the bouncer exists with no stored key. The
// next Prepare sees exactly that and starts over, so the failure heals itself and
// never leaves a key registered that nothing can produce.
func (s *Service) register(ctx context.Context) error {
	key, err := s.ctl.RegisterBouncer(ctx, BouncerName)
	if err != nil {
		return fmt.Errorf("registering the bouncer: %w", err)
	}
	sealed, err := s.cipher.Encrypt([]byte(key))
	if err != nil {
		return fmt.Errorf("sealing the bouncer key: %w", err)
	}
	if _, err := s.store.ReplaceCrowdSecBouncerKey(ctx, sealed); err != nil {
		return fmt.Errorf("storing the bouncer key: %w", err)
	}
	return nil
}

// Prepare looks at the local API and makes sure Caddy's bouncer is registered,
// and returns what the proxy should carry. It is the reconciler's only entry into
// CrowdSec, called every pass while the feature is switched on.
func (s *Service) Prepare(ctx context.Context) (proxy.CrowdSecDesire, error) {
	row, err := s.row(ctx)
	if err != nil {
		return proxy.CrowdSecDesire{}, err
	}
	if !row.Enabled {
		return proxy.CrowdSecDesire{}, nil
	}
	desire := proxy.CrowdSecDesire{Enabled: true, APIURL: APIURL}

	h := s.Look(ctx)
	if !h.Reachable {
		desire.Reason = proxy.ReasonCrowdSecUnavailable
		return desire, nil
	}

	if !h.BouncerRegistered || len(row.BouncerKeyEncrypted) == 0 {
		s.regMu.Lock()
		// Read again under the lock: another caller may have just done this.
		row, err = s.row(ctx)
		if err == nil {
			err = s.ensureRegistered(ctx, row)
		}
		if err == nil {
			row, err = s.row(ctx)
		}
		s.regMu.Unlock()
		if err != nil {
			desire.Reason = proxy.ReasonCrowdSecUnavailable
			return desire, fmt.Errorf("registering the CrowdSec bouncer: %w", err)
		}
		s.refresh(ctx)
	}

	key, err := s.cipher.Decrypt(row.BouncerKeyEncrypted)
	if err != nil {
		return desire, fmt.Errorf("opening the bouncer key: %w", err)
	}
	desire.Ready, desire.APIKey = true, string(key)
	return desire, nil
}

// ensureRegistered brings the registration into line with the stored key. regMu is
// held.
func (s *Service) ensureRegistered(ctx context.Context, row generated.CrowdsecConfig) error {
	_, found, err := s.ctl.Bouncer(ctx, BouncerName)
	if err != nil {
		return err
	}
	hasKey := len(row.BouncerKeyEncrypted) > 0
	if found && hasKey {
		return nil
	}
	if found {
		// Registered, but we hold no key for it: an orphan from an interrupted
		// registration. Its key cannot be recovered, so it is replaced.
		if err := s.ctl.DeleteBouncer(ctx, BouncerName); err != nil {
			return err
		}
	}
	return s.register(ctx)
}

// Look checks the local API, reusing an answer less than healthTTL old.
func (s *Service) Look(ctx context.Context) Health {
	s.mu.Lock()
	if h := s.health; !h.CheckedAt.IsZero() && s.now().Sub(h.CheckedAt) < healthTTL {
		s.mu.Unlock()
		return h
	}
	s.mu.Unlock()
	return s.refresh(ctx)
}

// Refresh checks the local API now.
func (s *Service) Refresh(ctx context.Context) Health { return s.refresh(ctx) }

func (s *Service) refresh(ctx context.Context) Health {
	h := Health{CheckedAt: s.now()}
	b, found, err := s.ctl.Bouncer(ctx, BouncerName)
	switch {
	case err != nil:
		h.Error = redact.Error(err.Error())
	default:
		h.Reachable = true
		h.BouncerRegistered = found
		h.LastPull = b.LastPull
	}
	s.mu.Lock()
	s.health = h
	s.mu.Unlock()
	return h
}

// Health returns the last check without making a new one.
func (s *Service) Health() Health {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.health
}

// SetAutoban stores the policy for turning WAF detections into bans.
func (s *Service) SetAutoban(ctx context.Context, p Policy) (Config, error) {
	if err := p.Validate(); err != nil {
		return Config{}, &InvalidError{err}
	}
	row, err := s.store.SetCrowdSecAutoban(ctx, generated.SetCrowdSecAutobanParams{
		AutobanEnabled:         p.Enabled,
		AutobanThreshold:       int32(p.Threshold),
		AutobanWindowMinutes:   int32(p.WindowMinutes),
		AutobanDurationMinutes: int32(p.DurationMinutes),
		AutobanMaxActive:       int32(p.MaxActive),
		AutobanCategories:      p.Categories,
	})
	if err != nil {
		return Config{}, fmt.Errorf("saving the auto-ban policy: %w", err)
	}
	return configFrom(row), nil
}

// recordSystem audits something Belune did on its own. What an admin does is
// audited by the handler that took the request, like every other mutation.
func (s *Service) recordSystem(action, resourceID string, details map[string]any) {
	if s.audit == nil {
		return
	}
	s.audit.Log("", "", "", action, "crowdsec", resourceID, details)
}

// InvalidError is a request the caller got wrong, as opposed to CrowdSec failing.
type InvalidError struct{ Err error }

func (e *InvalidError) Error() string { return e.Err.Error() }
func (e *InvalidError) Unwrap() error { return e.Err }
