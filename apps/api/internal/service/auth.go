package service

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/redis/go-redis/v9"
	"golang.org/x/crypto/bcrypt"

	"github.com/belune-hq/belune/internal/store/generated"
)

var (
	ErrInvalidCredentials = errors.New("invalid email or password")
	ErrUserAlreadyExists  = errors.New("user already exists")
	ErrInvalidToken       = errors.New("invalid or expired token")
	ErrAccountLocked      = errors.New("account temporarily locked due to repeated failed login attempts")
)

// Lockout tunables. Five failures within fifteen minutes triggers a fifteen
// minute lock. Kept as package-level constants because they have no security
// reason to be operator-tunable in alpha.
const (
	lockoutThreshold = 5
	lockoutWindow    = 15 * time.Minute
	lockoutDuration  = 15 * time.Minute
)

// AuthService is the authentication and session lifecycle service. It owns
// access-token issuance, refresh-token rotation (DB-backed so it survives
// Redis restarts), login-lockout state, and forced-session revocation
// (used on role change and admin password reset).
type AuthService struct {
	queries       *generated.Queries
	jwtSecret     []byte
	accessExpiry  time.Duration
	refreshExpiry time.Duration
	rdb           *redis.Client
	// secondFactor is nil until wired (see SetSecondFactorVerifier). A user with
	// a factor enrolled cannot log in while it is nil — the login fails rather
	// than skipping the factor.
	secondFactor SecondFactorVerifier
}

// AuthClaims are the JWT body. UserID + Email + Role are duplicated from the
// users row at issue time so request handling does not need a per-request DB
// fetch — the trade-off is that role changes propagate via the explicit
// session-revocation path (RevokeUserSessions) rather than via every token.
//
// Tseq is the per-user token sequence at the moment this JWT was issued.
// RevokeUserSessions increments the sequence in Redis. ValidateToken rejects
// any JWT whose Tseq is strictly less than the current sequence — i.e. any
// token issued before the most recent revocation.
type AuthClaims struct {
	UserID string `json:"user_id"`
	Email  string `json:"email"`
	Role   string `json:"role"`
	Tseq   int64  `json:"tseq"`
	jwt.RegisteredClaims
}

// LoginResult is what the login + refresh endpoints return.
type LoginResult struct {
	Token        string         `json:"token"`
	RefreshToken string         `json:"refresh_token"`
	ExpiresIn    int            `json:"expires_in"`
	User         AuthUserResult `json:"user"`
}

// AuthResult is the legacy alias used by callers that only want the access token.
// Kept so existing handler code compiles without sweeping changes.
type AuthResult = LoginResult

type AuthUserResult struct {
	ID        pgtype.UUID `json:"id"`
	Email     string      `json:"email"`
	Role      string      `json:"role"`
	Username  string      `json:"username"`
	FirstName string      `json:"first_name"`
	LastName  string      `json:"last_name"`
}

// NewAuthService creates the auth service. accessExpiryHours / refreshExpiryHours
// fall back to safe defaults (1h access, 7d refresh) when zero or negative.
func NewAuthService(queries *generated.Queries, jwtSecret string, accessExpiryHours, refreshExpiryHours int, rdb *redis.Client) *AuthService {
	access := time.Duration(accessExpiryHours) * time.Hour
	if access <= 0 {
		access = time.Hour
	}
	refresh := time.Duration(refreshExpiryHours) * time.Hour
	if refresh <= 0 {
		refresh = 7 * 24 * time.Hour
	}
	return &AuthService{
		queries:       queries,
		jwtSecret:     []byte(jwtSecret),
		accessExpiry:  access,
		refreshExpiry: refresh,
		rdb:           rdb,
	}
}

func (s *AuthService) Register(ctx context.Context, email, password, role, username string) (generated.User, error) {
	_, err := s.queries.GetUserByEmail(ctx, email)
	if err == nil {
		return generated.User{}, ErrUserAlreadyExists
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return generated.User{}, fmt.Errorf("hash password: %w", err)
	}

	user, err := s.queries.CreateUser(ctx, generated.CreateUserParams{
		Email:        email,
		PasswordHash: string(hash),
		Role:         role,
		Username:     username,
	})
	if err != nil {
		return generated.User{}, fmt.Errorf("create user: %w", err)
	}

	return user, nil
}

// Login validates credentials and issues a new access + refresh token pair.
// The refresh row is persisted with a sha256(token_hash); plaintext is only
// returned to the caller and never stored. userAgent + ip are recorded on the
// row for future "list active sessions" UX (out of Phase 1 scope).
//
// Lockout is checked separately via CheckLockout before this is called so the
// handler can surface a Retry-After header without leaking that the user
// exists. RecordFailedLogin must be called by the handler on credential
// failure; we deliberately do not couple Login to that path here so the
// service stays composable.
func (s *AuthService) Login(ctx context.Context, email, password, userAgent, ip string) (*LoginOutcome, error) {
	user, err := s.queries.GetUserByEmail(ctx, email)
	if err != nil {
		return nil, ErrInvalidCredentials
	}

	if err := bcrypt.CompareHashAndPassword([]byte(user.PasswordHash), []byte(password)); err != nil {
		return nil, ErrInvalidCredentials
	}

	// A correct password is where this stops for anyone with a second factor.
	// The challenge carries no session, so there is nothing for a client to
	// mishandle into a bypass.
	if HasMFA(user) {
		challenge, err := s.issueChallenge(ctx, user)
		if err != nil {
			return nil, err
		}
		return &LoginOutcome{Challenge: challenge, UserID: user.ID}, nil
	}

	session, err := s.issueSession(ctx, user, userAgent, ip)
	if err != nil {
		return nil, err
	}
	return &LoginOutcome{Session: session, UserID: user.ID}, nil
}

// Refresh exchanges a refresh token (plaintext, as stored in the cookie)
// for a fresh access + refresh pair. The old refresh token is revoked
// regardless of outcome — this gives the caller automatic single-use rotation
// and limits replay windows if a refresh token has been exfiltrated.
func (s *AuthService) Refresh(ctx context.Context, refreshTokenPlain, userAgent, ip string) (*LoginResult, error) {
	if refreshTokenPlain == "" {
		return nil, ErrInvalidToken
	}
	hash := hashRefreshToken(refreshTokenPlain)

	row, err := s.queries.GetActiveRefreshToken(ctx, hash)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrInvalidToken
		}
		return nil, fmt.Errorf("lookup refresh token: %w", err)
	}

	// Revoke the old token immediately (single-use rotation).
	if err := s.queries.RevokeRefreshTokenByID(ctx, row.ID); err != nil {
		slog.Warn("auth: failed to revoke rotated refresh token", "error", err)
	}

	user, err := s.queries.GetUserByID(ctx, row.UserID)
	if err != nil {
		return nil, ErrInvalidToken
	}

	return s.issueSession(ctx, user, userAgent, ip)
}

// IssueSessionFor mints a session for a user who has already been authenticated
// by some other means — a completed second-factor challenge, or a re-auth that
// deliberately revoked every session and needs to hand this one back.
func (s *AuthService) IssueSessionFor(ctx context.Context, user generated.User, userAgent, ip string) (*LoginResult, error) {
	return s.issueSession(ctx, user, userAgent, ip)
}

// RevokeRefreshToken revokes a single refresh token by plaintext. Used by
// logout. Idempotent — a nonexistent or already-revoked token returns nil.
func (s *AuthService) RevokeRefreshToken(ctx context.Context, refreshTokenPlain string) error {
	if refreshTokenPlain == "" {
		return nil
	}
	return s.queries.RevokeRefreshTokenByHash(ctx, hashRefreshToken(refreshTokenPlain))
}

// RevokeUserSessions forces all of a user's sessions to terminate. Two effects:
//
//  1. All active refresh tokens are revoked in DB so they cannot mint new
//     access tokens.
//  2. The per-user token sequence in Redis (user:token_seq:{user_id}) is
//     incremented. ValidateToken rejects any JWT whose Tseq claim is below
//     the current value — so existing access tokens are rejected on next use.
//
// Sequence-based revocation is robust to wall-clock granularity (timestamps
// were ambiguous within a single second) and self-cleans via Redis TTL.
//
// Called on role changes and on admin-initiated password resets.
func (s *AuthService) RevokeUserSessions(ctx context.Context, userID pgtype.UUID) error {
	if err := s.queries.RevokeAllUserRefreshTokens(ctx, userID); err != nil {
		return fmt.Errorf("revoke refresh tokens: %w", err)
	}
	if s.rdb == nil {
		return nil
	}
	uid := uuidString(userID)
	if uid == "" {
		return nil
	}
	key := tokenSeqKey(uid)
	if _, err := s.rdb.Incr(ctx, key).Result(); err != nil {
		slog.Warn("auth: failed to bump token sequence", "user_id", uid, "error", err)
		return nil
	}
	// TTL covers the longest possible access-token lifetime — once every
	// outstanding token has expired naturally, the sequence can reset to 0.
	if err := s.rdb.Expire(ctx, key, s.accessExpiry).Err(); err != nil {
		slog.Warn("auth: failed to set token sequence TTL", "user_id", uid, "error", err)
	}
	return nil
}

// currentTokenSeq returns the user's current token sequence, or 0 if the key
// does not exist (meaning the user has never had their sessions revoked).
func (s *AuthService) currentTokenSeq(ctx context.Context, userID string) int64 {
	if s.rdb == nil || userID == "" {
		return 0
	}
	val, err := s.rdb.Get(ctx, tokenSeqKey(userID)).Result()
	if err != nil {
		return 0
	}
	n, err := strconv.ParseInt(val, 10, 64)
	if err != nil {
		return 0
	}
	return n
}

// ValidateToken parses and verifies a JWT, then checks two revocation lists:
// the per-token JTI blacklist (set by Logout) and the per-user token sequence
// (bumped by RevokeUserSessions). Both are Redis-only — if Redis is
// unavailable validation fails open on the revocation checks, which is the
// documented trade-off vs adding a DB lookup to every authenticated request.
func (s *AuthService) ValidateToken(ctx context.Context, tokenString string) (*AuthClaims, error) {
	token, err := jwt.ParseWithClaims(tokenString, &AuthClaims{}, func(t *jwt.Token) (any, error) {
		if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("unexpected signing method: %v", t.Header["alg"])
		}
		return s.jwtSecret, nil
	})
	if err != nil {
		return nil, ErrInvalidToken
	}

	claims, ok := token.Claims.(*AuthClaims)
	if !ok || !token.Valid {
		return nil, ErrInvalidToken
	}

	if s.rdb == nil {
		return claims, nil
	}

	// Per-JTI blacklist (logout).
	if claims.ID != "" {
		exists, err := s.rdb.Exists(ctx, "jwt:blacklist:"+claims.ID).Result()
		if err == nil && exists > 0 {
			return nil, ErrInvalidToken
		}
	}

	// Per-user token sequence. A token whose embedded Tseq is strictly less
	// than the current value was issued before a RevokeUserSessions call and
	// must be rejected.
	if claims.UserID != "" {
		current := s.currentTokenSeq(ctx, claims.UserID)
		if claims.Tseq < current {
			return nil, ErrInvalidToken
		}
	}

	return claims, nil
}

// BlacklistToken adds a token's JTI to the Redis blacklist with a TTL covering
// its remaining lifetime.
func (s *AuthService) BlacklistToken(ctx context.Context, tokenString string) error {
	claims, err := s.ValidateToken(ctx, tokenString)
	if err != nil {
		return nil
	}
	if claims.ID == "" || s.rdb == nil {
		return nil
	}
	ttl := time.Until(claims.ExpiresAt.Time)
	if ttl <= 0 {
		return nil
	}
	return s.rdb.Set(ctx, "jwt:blacklist:"+claims.ID, "1", ttl).Err()
}

func (s *AuthService) IsSetupRequired(ctx context.Context) (bool, error) {
	count, err := s.queries.CountUsers(ctx)
	if err != nil {
		return false, fmt.Errorf("count users: %w", err)
	}
	return count == 0, nil
}

// CheckLockout returns (true, retryAfter) if the email is currently locked.
// retryAfter is the duration until the lock expires; never negative.
func (s *AuthService) CheckLockout(ctx context.Context, email string) (bool, time.Duration, error) {
	row, err := s.queries.GetLoginAttempt(ctx, email)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return false, 0, nil
		}
		return false, 0, fmt.Errorf("get login attempt: %w", err)
	}
	if !row.LockedUntil.Valid {
		return false, 0, nil
	}
	remaining := time.Until(row.LockedUntil.Time)
	if remaining <= 0 {
		return false, 0, nil
	}
	return true, remaining, nil
}

// RecordFailedLogin increments the per-email failed-login counter and locks
// the account if the threshold is reached within the rolling window. Returns
// (locked, retryAfter, err) — locked=true means this call triggered the lock.
func (s *AuthService) RecordFailedLogin(ctx context.Context, email string) (bool, time.Duration, error) {
	windowStart := pgtype.Timestamptz{Time: time.Now().Add(-lockoutWindow), Valid: true}

	row, err := s.queries.IncrementFailedLogin(ctx, generated.IncrementFailedLoginParams{
		Email:         email,
		FirstFailedAt: windowStart,
	})
	if err != nil {
		return false, 0, fmt.Errorf("increment failed login: %w", err)
	}

	if row.FailedCount < lockoutThreshold {
		return false, 0, nil
	}

	lockedUntil := time.Now().Add(lockoutDuration)
	if err := s.queries.LockAccount(ctx, generated.LockAccountParams{
		Email:       email,
		LockedUntil: pgtype.Timestamptz{Time: lockedUntil, Valid: true},
	}); err != nil {
		return false, 0, fmt.Errorf("lock account: %w", err)
	}
	return true, time.Until(lockedUntil), nil
}

// ResetLoginAttempts clears the lockout state for an email (called on
// successful login).
func (s *AuthService) ResetLoginAttempts(ctx context.Context, email string) {
	if err := s.queries.ResetLoginAttempts(ctx, email); err != nil {
		slog.Warn("auth: failed to reset login attempts", "email", email, "error", err)
	}
}

// AccessExpiry returns the configured access-token lifetime; used by the
// handler when setting the cookie MaxAge.
func (s *AuthService) AccessExpiry() time.Duration { return s.accessExpiry }

// RefreshExpiry returns the configured refresh-token lifetime; used by the
// handler when setting the refresh cookie MaxAge.
func (s *AuthService) RefreshExpiry() time.Duration { return s.refreshExpiry }

// LoginUser issues a JWT + refresh token pair for a user whose identity has
// already been established by the caller (e.g. accept-invitation flow where
// the password was just set). Skips credential verification.
func (s *AuthService) LoginUser(ctx context.Context, user generated.User, userAgent, ip string) (*LoginResult, error) {
	return s.issueSession(ctx, user, userAgent, ip)
}

// issueSession generates a JWT + refresh token pair, persists the refresh
// token, and returns the bundle. Shared by Login and Refresh.
func (s *AuthService) issueSession(ctx context.Context, user generated.User, userAgent, ip string) (*LoginResult, error) {
	tseq := s.currentTokenSeq(ctx, uuidString(user.ID))
	access, err := s.generateAccessToken(user, tseq)
	if err != nil {
		return nil, fmt.Errorf("generate access token: %w", err)
	}

	refreshPlain, err := generateRefreshToken()
	if err != nil {
		return nil, fmt.Errorf("generate refresh token: %w", err)
	}

	if _, err := s.queries.CreateRefreshToken(ctx, generated.CreateRefreshTokenParams{
		UserID:    user.ID,
		TokenHash: hashRefreshToken(refreshPlain),
		ExpiresAt: pgtype.Timestamptz{Time: time.Now().Add(s.refreshExpiry), Valid: true},
		UserAgent: userAgent,
		Ip:        ip,
	}); err != nil {
		return nil, fmt.Errorf("persist refresh token: %w", err)
	}

	return &LoginResult{
		Token:        access,
		RefreshToken: refreshPlain,
		ExpiresIn:    int(s.accessExpiry.Seconds()),
		User: AuthUserResult{
			ID:        user.ID,
			Email:     user.Email,
			Role:      user.Role,
			Username:  user.Username,
			FirstName: user.FirstName,
			LastName:  user.LastName,
		},
	}, nil
}

func (s *AuthService) generateAccessToken(user generated.User, tseq int64) (string, error) {
	userIDStr := uuidString(user.ID)

	claims := AuthClaims{
		UserID: userIDStr,
		Email:  user.Email,
		Role:   user.Role,
		Tseq:   tseq,
		RegisteredClaims: jwt.RegisteredClaims{
			ID:        uuid.New().String(),
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(s.accessExpiry)),
			IssuedAt:  jwt.NewNumericDate(time.Now()),
			Subject:   userIDStr,
		},
	}

	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	return token.SignedString(s.jwtSecret)
}

// generateRefreshToken produces a 32-byte URL-safe random string. The plaintext
// is only ever returned to the client; what we persist is sha256(plaintext).
func generateRefreshToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func hashRefreshToken(plain string) []byte {
	sum := sha256.Sum256([]byte(plain))
	return sum[:]
}

func tokenSeqKey(userID string) string {
	return "user:token_seq:" + userID
}

func uuidString(id pgtype.UUID) string {
	if !id.Valid {
		return ""
	}
	b := id.Bytes
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
