package service

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/belune-hq/belune/internal/store"
	"github.com/belune-hq/belune/internal/store/generated"
)

var ErrInvalidAPIToken = errors.New("invalid or expired token")

// TokenPrefix marks a value as a personal access token rather than a session
// JWT — checked before any DB lookup, so a wrong-shaped Authorization header
// never reaches the hash comparison. It also doubles as a secret-scanner
// signature, same reasoning as GitHub's own token prefixes.
const TokenPrefix = "belune_pat_"

// lastUsedCoarsen bounds how often a validated token's last_used_at is
// written. A dashboard held open or a 15s Prometheus scrape would otherwise
// turn every single request into a write; the field only needs to answer "is
// this token still in use", not record exact request timestamps.
const lastUsedCoarsen = 5 * time.Minute

// HasTokenPrefix reports whether s looks like a personal access token.
func HasTokenPrefix(s string) bool {
	return strings.HasPrefix(s, TokenPrefix)
}

// AllScopes is the full, valid scope set — both the canonical list a create
// request's chosen scopes are validated against, and what PR3 minted every
// token with before this picker existed (PR3 shipped no scope picker
// deliberately, so no UI ever promised a restriction enforcement didn't back
// yet; this PR adds the picker and enforcement together).
var AllScopes = []string{"read", "write", "deploy", "metrics"}

// TokenService owns personal access tokens: generation, hashing, and the
// authentication lookup the auth middleware calls on every Bearer request
// whose value has the PAT prefix.
type TokenService struct {
	db      *pgxpool.Pool
	queries *generated.Queries
}

func NewTokenService(db *pgxpool.Pool, queries *generated.Queries) *TokenService {
	return &TokenService{db: db, queries: queries}
}

// AuthenticatedToken is what a valid PAT resolves to — enough for the auth
// middleware to populate the request context exactly like a JWT session.
type AuthenticatedToken struct {
	TokenID       pgtype.UUID
	UserID        pgtype.UUID
	EffectiveRole string
	Scopes        []string
	// Pinned is the token's own recorded pin state, not derived from
	// len(ProjectIDs) == 0 — a pinned token can legitimately have zero
	// reachable projects (every one it was pinned to got deleted, or the
	// owner lost access to it), and that must reach NOTHING, not everything.
	// See migration 000067 for why this can't be inferred from row count.
	Pinned bool
	// ProjectIDs is the token's pinned project set as strings, empty (never
	// nil) when Pinned is false or when it is true but nothing is left
	// reachable — callers branch on Pinned, never on this slice's length.
	ProjectIDs []string
}

// GenerateToken produces a new plaintext token and its SHA-256 hash. The
// plaintext is returned to the caller exactly once by whoever calls this
// (PR3's create endpoint) — only the hash is ever persisted.
func GenerateToken() (plain string, hash []byte, err error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", nil, err
	}
	plain = TokenPrefix + base64.RawURLEncoding.EncodeToString(b)
	sum := sha256.Sum256([]byte(plain))
	return plain, sum[:], nil
}

func hashToken(plain string) []byte {
	sum := sha256.Sum256([]byte(plain))
	return sum[:]
}

// minRole returns the more restrictive of two roles. There are only two
// roles, so this is "both admin, or member" rather than a general ordering —
// it exists to make the shrink-only intent readable at the call site, not to
// generalise past a two-role model that isn't there.
func minRole(a, b string) string {
	if a == "admin" && b == "admin" {
		return "admin"
	}
	return "member"
}

// Authenticate looks up a plaintext PAT by its hash, rejects an unknown or
// expired one, and computes the effective role — min(role_at_issue, the
// owner's role right now). Demotion restricts immediately; promotion does
// NOT retroactively elevate a token minted under lower privilege.
//
// Also coarsens last_used_at (see lastUsedCoarsen): the touch is best-effort
// and its failure must never fail authentication.
func (s *TokenService) Authenticate(ctx context.Context, plain string) (*AuthenticatedToken, error) {
	row, err := s.queries.GetAPITokenByHash(ctx, hashToken(plain))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrInvalidAPIToken
		}
		return nil, err
	}

	if row.ExpiresAt.Valid && row.ExpiresAt.Time.Before(time.Now()) {
		return nil, ErrInvalidAPIToken
	}

	now := time.Now()
	if err := s.queries.UpdateAPITokenLastUsed(ctx, generated.UpdateAPITokenLastUsedParams{
		ID:         row.ID,
		LastUsedAt: pgtype.Timestamptz{Time: now, Valid: true},
		Threshold:  pgtype.Timestamptz{Time: now.Add(-lastUsedCoarsen), Valid: true},
	}); err != nil {
		slog.Warn("token: failed to update last_used_at", "token_id", uuidString(row.ID), "error", err)
	}

	return &AuthenticatedToken{
		TokenID:       row.ID,
		UserID:        row.UserID,
		EffectiveRole: minRole(row.RoleAtIssue, row.UserRole),
		Scopes:        row.Scopes,
		Pinned:        row.Pinned,
		ProjectIDs:    uuidsToStrings(row.ProjectIds),
	}, nil
}

// uuidsToStrings converts a slice of parsed ids to their string form. The
// query result it is always called on is already COALESCE'd to a non-nil
// array in SQL, so the output is always non-nil too — matching
// AuthenticatedToken.ProjectIDs' own "never nil" contract.
func uuidsToStrings(ids []pgtype.UUID) []string {
	out := make([]string, len(ids))
	for i, id := range ids {
		out[i] = uuidToString(id)
	}
	return out
}

// CreateTokenParams collects what Create persists. Validation (name
// non-empty, expiry one of the offered choices, scopes non-empty and each one
// a member of AllScopes, and — for ProjectIDs — that the caller can actually
// reach every project named) is the handler's job — this is the point past
// which a token unconditionally gets minted with exactly the scopes and pins
// given.
type CreateTokenParams struct {
	UserID      pgtype.UUID
	Name        string
	RoleAtIssue string
	ExpiresAt   pgtype.Timestamptz
	Scopes      []string
	// Pinned narrows the token's reach to exactly ProjectIDs — false means
	// unpinned (every project the owner can reach, evaluated at use time),
	// the default a create request with an empty or omitted project list
	// gets. The handler decides this from len(requested ids) > 0, never a
	// caller-supplied flag: "pinned to zero projects" is a state a token can
	// only reach LATER (every pin deleted out from under it), not one a
	// create request can ask for directly.
	Pinned bool
	// ProjectIDs pins the token to these projects; empty when Pinned is
	// false. Already validated and parsed by the handler (reachable, exists,
	// deduplicated) — Create persists exactly what it's given.
	ProjectIDs []pgtype.UUID
}

// CreatedToken carries the plaintext alongside the stored row. Plain exists
// only for this one return trip — Create never logs or persists it, and
// nothing downstream can recover it once the caller's response is sent.
// ProjectIDs rides alongside rather than on the embedded row because a
// token's pins live in api_token_projects, not on api_tokens itself.
type CreatedToken struct {
	generated.ApiToken
	Plain      string
	ProjectIDs []string
}

// Create mints a new personal access token with exactly p.Scopes, pinned to
// p.ProjectIDs if p.Pinned. The token row and its pin rows are written in one
// transaction: a partial failure that persisted the token without its pins
// would produce a pinned=false-looking token that actually reaches every
// project its owner can — the same escalation migration 000067's pinned
// column exists to prevent, just reached through a crash instead of a
// dangling foreign key.
func (s *TokenService) Create(ctx context.Context, p CreateTokenParams) (*CreatedToken, error) {
	plain, hash, err := GenerateToken()
	if err != nil {
		return nil, fmt.Errorf("generating token: %w", err)
	}

	var row generated.ApiToken
	err = store.WithTx(ctx, s.db, func(q *generated.Queries) error {
		var err error
		row, err = q.CreateAPIToken(ctx, generated.CreateAPITokenParams{
			UserID:      p.UserID,
			Name:        p.Name,
			TokenHash:   hash,
			Scopes:      p.Scopes,
			Pinned:      p.Pinned,
			RoleAtIssue: p.RoleAtIssue,
			ExpiresAt:   p.ExpiresAt,
		})
		if err != nil {
			return err
		}
		if len(p.ProjectIDs) == 0 {
			return nil
		}
		return q.CreateAPITokenProjectPins(ctx, generated.CreateAPITokenProjectPinsParams{
			TokenID:    row.ID,
			ProjectIds: p.ProjectIDs,
		})
	})
	if err != nil {
		return nil, fmt.Errorf("creating token: %w", err)
	}
	projectIDs := make([]string, len(p.ProjectIDs))
	for i, id := range p.ProjectIDs {
		projectIDs[i] = uuidToString(id)
	}
	return &CreatedToken{ApiToken: row, Plain: plain, ProjectIDs: projectIDs}, nil
}
