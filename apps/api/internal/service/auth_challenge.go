package service

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/belune-hq/belune/internal/store/generated"
)

var (
	// ErrInvalidChallenge covers unknown, expired, and already-spent challenges.
	ErrInvalidChallenge = errors.New("invalid or expired challenge")
	// ErrSecondFactorUnavailable is what a login attempt gets when the user has
	// a factor enrolled but nothing is wired up to verify it. It fails closed on
	// purpose: skipping the factor because the verifier is missing would turn a
	// wiring mistake into an authentication bypass.
	ErrSecondFactorUnavailable = errors.New("second factor verification is unavailable")
)

const (
	// Long enough to fetch a phone, short enough that a challenge left on a
	// shared screen is worthless by the time anyone returns to it.
	challengeTTL = 5 * time.Minute
	// Attempts allowed against one challenge before it dies. Six digits is a
	// million combinations, and account-level lockout still applies on top.
	challengeMaxAttempts = 5
)

// SecondFactorVerifier is the seam the login challenge talks to. TOTP is the
// only implementation today; the challenge names its methods as data so another
// factor can be added without a new endpoint or a client change.
type SecondFactorVerifier interface {
	Methods(ctx context.Context, user generated.User) []string
	Verify(ctx context.Context, user generated.User, method, code string) error
}

// LoginChallenge is returned when the password was right but is not, by itself,
// enough. It deliberately carries no session: returning real tokens and
// enforcing the second factor in the client is not a second factor.
type LoginChallenge struct {
	Challenge string   `json:"challenge"`
	Methods   []string `json:"methods"`
	ExpiresIn int      `json:"expires_in"`
}

// LoginOutcome is exactly one of a finished session or a challenge to complete.
// UserID is carried for the caller's audit trail and is never serialised: the
// challenge response must not name the account it belongs to.
type LoginOutcome struct {
	Session   *LoginResult
	Challenge *LoginChallenge
	UserID    pgtype.UUID
}

// SetSecondFactorVerifier wires the verifier after construction, which keeps
// AuthService and TOTPService from having to know about each other at build
// time.
func (s *AuthService) SetSecondFactorVerifier(v SecondFactorVerifier) {
	s.secondFactor = v
}

// issueChallenge mints a single-use token standing for "this password was
// correct". Redis holds only its hash, so a dump of the store yields nothing
// that can be presented as the challenge itself.
func (s *AuthService) issueChallenge(ctx context.Context, user generated.User) (*LoginChallenge, error) {
	if s.rdb == nil || s.secondFactor == nil {
		return nil, ErrSecondFactorUnavailable
	}

	// Ask before minting anything: a user with a factor enabled but no way to
	// present it should fail the login, not receive a challenge nothing can
	// answer.
	methods := s.secondFactor.Methods(ctx, user)
	if len(methods) == 0 {
		return nil, ErrSecondFactorUnavailable
	}

	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return nil, fmt.Errorf("generate challenge: %w", err)
	}
	token := base64.RawURLEncoding.EncodeToString(raw)

	key := challengeKey(token)
	if err := s.rdb.HSet(ctx, key, map[string]any{
		"user_id":  uuidString(user.ID),
		"attempts": 0,
	}).Err(); err != nil {
		return nil, fmt.Errorf("store challenge: %w", err)
	}
	if err := s.rdb.Expire(ctx, key, challengeTTL).Err(); err != nil {
		// Without a TTL the challenge would outlive its window, so drop it
		// rather than hand back one that never expires.
		s.rdb.Del(ctx, key)
		return nil, fmt.Errorf("set challenge expiry: %w", err)
	}

	return &LoginChallenge{
		Challenge: token,
		Methods:   methods,
		ExpiresIn: int(challengeTTL.Seconds()),
	}, nil
}

// ChallengeSubject returns the user a pending challenge belongs to without
// spending it, so the caller can audit an attempt against a real account.
func (s *AuthService) ChallengeSubject(ctx context.Context, challenge string) (generated.User, error) {
	if s.rdb == nil || challenge == "" {
		return generated.User{}, ErrInvalidChallenge
	}
	userID, err := s.rdb.HGet(ctx, challengeKey(challenge), "user_id").Result()
	if err != nil || userID == "" {
		return generated.User{}, ErrInvalidChallenge
	}
	return s.userByIDString(ctx, userID)
}

// CompleteLoginChallenge exchanges a challenge plus a factor for a real
// session. Failure costs an attempt; running out kills the challenge, so a
// stolen password cannot be paired with an unlimited guessing budget against
// six digits.
func (s *AuthService) CompleteLoginChallenge(ctx context.Context, challenge, method, code, userAgent, ip string) (*LoginResult, error) {
	if s.rdb == nil {
		return nil, ErrSecondFactorUnavailable
	}
	if s.secondFactor == nil {
		return nil, ErrSecondFactorUnavailable
	}

	key := challengeKey(challenge)
	fields, err := s.rdb.HGetAll(ctx, key).Result()
	if err != nil || len(fields) == 0 || fields["user_id"] == "" {
		return nil, ErrInvalidChallenge
	}

	user, err := s.userByIDString(ctx, fields["user_id"])
	if err != nil {
		return nil, ErrInvalidChallenge
	}

	// Spend the attempt before verifying. A verification that panics, times out
	// or is cancelled must still cost the attacker one of their five.
	attempts, err := s.rdb.HIncrBy(ctx, key, "attempts", 1).Result()
	if err != nil {
		return nil, fmt.Errorf("count challenge attempt: %w", err)
	}
	if attempts > challengeMaxAttempts {
		s.rdb.Del(ctx, key)
		return nil, ErrInvalidChallenge
	}

	if err := s.secondFactor.Verify(ctx, user, method, code); err != nil {
		if attempts >= challengeMaxAttempts {
			s.rdb.Del(ctx, key)
		}
		// The account-level lockout counts these too: five wrong codes are as
		// good a signal as five wrong passwords.
		if _, _, lockErr := s.RecordFailedLogin(ctx, strings.ToLower(user.Email)); lockErr != nil {
			slog.Warn("auth: failed to record second-factor failure", "error", lockErr)
		}
		return nil, err
	}

	// Single-use: the challenge dies the moment it produces a session.
	if err := s.rdb.Del(ctx, key).Err(); err != nil {
		slog.Warn("auth: failed to consume login challenge", "error", err)
	}
	s.ResetLoginAttempts(ctx, strings.ToLower(user.Email))

	return s.issueSession(ctx, user, userAgent, ip)
}

func (s *AuthService) userByIDString(ctx context.Context, id string) (generated.User, error) {
	var uid pgtype.UUID
	if err := uid.Scan(id); err != nil {
		return generated.User{}, ErrInvalidChallenge
	}
	user, err := s.queries.GetUserByID(ctx, uid)
	if err != nil {
		return generated.User{}, ErrInvalidChallenge
	}
	return user, nil
}

// challengeKey stores the hash rather than the token itself: Redis then holds
// nothing that could be replayed as a challenge if its contents leak.
func challengeKey(token string) string {
	sum := sha256.Sum256([]byte(token))
	return "login:challenge:" + hex.EncodeToString(sum[:])
}
