package service

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base32"
	"encoding/base64"
	"errors"
	"fmt"
	"image/png"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pquerna/otp"
	"github.com/pquerna/otp/totp"

	"github.com/belune-hq/belune/internal/pkg/crypto"
	"github.com/belune-hq/belune/internal/store"
	"github.com/belune-hq/belune/internal/store/generated"
)

var (
	// ErrInvalidSecondFactor covers a wrong code and an unknown recovery code
	// alike: which one it was is not the caller's business.
	ErrInvalidSecondFactor = errors.New("invalid verification code")
	// ErrSecondFactorUsed is deliberately distinct. A TOTP code stays valid for
	// its whole window, so a user who submits the same one twice needs to be
	// told to wait for the next code rather than shown "invalid" for something
	// they entered correctly. It tells an attacker only that codes are
	// single-use, which is a property of TOTP, not a secret.
	ErrSecondFactorUsed = errors.New("that code has already been used")
	// ErrTOTPNotEnrolled is returned when there is no secret to verify against.
	ErrTOTPNotEnrolled = errors.New("no second factor is enrolled")
	// ErrUnsupportedMethod guards the method-agnostic verify path against a
	// client asking for a factor this build does not implement.
	ErrUnsupportedMethod = errors.New("unsupported verification method")
)

const (
	// The parameters every authenticator app assumes when scanning a QR code.
	// They are not configurable: an install that changed them would produce
	// codes the user's app cannot generate.
	totpPeriod    = 30
	totpDigits    = otp.DigitsSix
	totpAlgorithm = otp.AlgorithmSHA1

	// ±1 step, i.e. 90 seconds of tolerance in total. Every extra step widens
	// the set of codes a single guess can match, so this is as far as clock
	// skew is indulged.
	totpSkew = 1

	recoveryCodeCount = 10
	// 10 bytes = 80 bits, the same reasoning as an API token: enough that the
	// hash needs no work factor.
	recoveryCodeBytes = 10

	// Method names carried in the challenge and echoed back by the client.
	MethodTOTP         = "totp"
	MethodRecoveryCode = "recovery_code"
)

// TOTPService owns the second-factor secret: enrollment, verification, and the
// recovery codes that stand in when the authenticator is gone.
type TOTPService struct {
	db      *pgxpool.Pool
	queries *generated.Queries
	keyring *crypto.Keyring
}

func NewTOTPService(db *pgxpool.Pool, queries *generated.Queries, keyring *crypto.Keyring) *TOTPService {
	return &TOTPService{db: db, queries: queries, keyring: keyring}
}

// HasMFA reports whether a user has any second factor enabled. A helper rather
// than a scattered totp_enabled_at check so that adding a second kind of factor
// later is one edit, not a search.
func HasMFA(user generated.User) bool {
	return user.TotpEnabledAt.Valid
}

// Enrollment is what the user needs to add the account to their authenticator:
// a QR code to scan, and the same secret in a form they can type when a camera
// is not an option.
//
// The QR is rendered here, as a data URI, rather than in the browser. The
// alternative is shipping a QR library to every page load to draw a string the
// server already has, and the CSP already permits data: images.
type Enrollment struct {
	Secret string `json:"secret"`
	URI    string `json:"uri"`
	QRCode string `json:"qr_code"`
}

// Methods lists the ways this user can satisfy a second-factor challenge. The
// challenge carries this list so the client never has to know which factors
// exist — which is what lets another factor be added without a client change.
func (s *TOTPService) Methods(ctx context.Context, user generated.User) []string {
	if !HasMFA(user) {
		return []string{}
	}
	methods := []string{MethodTOTP}
	if n, err := s.queries.CountUnusedRecoveryCodes(ctx, user.ID); err == nil && n > 0 {
		methods = append(methods, MethodRecoveryCode)
	}
	return methods
}

// Enroll generates a secret and stores it WITHOUT enabling the factor. Nothing
// changes about how this user logs in until ConfirmEnrollment succeeds, which
// is the whole point: a secret that was mis-scanned must not be able to lock
// anyone out.
func (s *TOTPService) Enroll(ctx context.Context, user generated.User, issuer string) (*Enrollment, error) {
	key, err := totp.Generate(totp.GenerateOpts{
		Issuer:      issuer,
		AccountName: user.Email,
		Period:      totpPeriod,
		Digits:      totpDigits,
		Algorithm:   totpAlgorithm,
	})
	if err != nil {
		return nil, fmt.Errorf("generate totp secret: %w", err)
	}

	qr, err := qrDataURI(key)
	if err != nil {
		return nil, err
	}

	encrypted, err := s.keyring.Encrypt([]byte(key.Secret()))
	if err != nil {
		return nil, fmt.Errorf("encrypt totp secret: %w", err)
	}
	if err := s.queries.SetUserTOTPSecret(ctx, generated.SetUserTOTPSecretParams{
		ID:                  user.ID,
		TotpSecretEncrypted: encrypted,
	}); err != nil {
		return nil, fmt.Errorf("store totp secret: %w", err)
	}

	return &Enrollment{Secret: key.Secret(), URI: key.URL(), QRCode: qr}, nil
}

func qrDataURI(key *otp.Key) (string, error) {
	img, err := key.Image(240, 240)
	if err != nil {
		return "", fmt.Errorf("render totp qr code: %w", err)
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return "", fmt.Errorf("encode totp qr code: %w", err)
	}
	return "data:image/png;base64," + base64.StdEncoding.EncodeToString(buf.Bytes()), nil
}

// ConfirmEnrollment enables the factor once the user has proved they can
// generate a code from the secret they just stored, and returns the recovery
// codes — the only time they are ever readable.
func (s *TOTPService) ConfirmEnrollment(ctx context.Context, user generated.User, code string) ([]string, error) {
	step, err := s.checkCode(user, code)
	if err != nil {
		return nil, err
	}

	// Enabling and issuing the codes are one write. Apart, a failure between
	// them leaves the factor on with no recovery codes at all, while the user is
	// told enrollment failed — they would then be asked for a code they were
	// never given a way back from.
	var codes []string
	err = store.WithTx(ctx, s.db, func(q *generated.Queries) error {
		// The UPDATE is conditional on a secret still being present, so a no-op
		// means the enrollment is gone, not that it succeeded.
		enabled, err := q.EnableUserTOTP(ctx, generated.EnableUserTOTPParams{
			ID:           user.ID,
			TotpLastStep: pgtype.Int8{Int64: step, Valid: true},
		})
		if err != nil {
			return fmt.Errorf("enable totp: %w", err)
		}
		if enabled == 0 {
			return ErrTOTPNotEnrolled
		}
		codes, err = replaceRecoveryCodes(ctx, q, user.ID)
		return err
	})
	if err != nil {
		return nil, err
	}
	return codes, nil
}

// Verify checks one factor of the named kind. The method is data, not a
// separate endpoint, so a new factor costs no change to the API or the client.
func (s *TOTPService) Verify(ctx context.Context, user generated.User, method, code string) error {
	switch method {
	case MethodTOTP:
		step, err := s.checkCode(user, code)
		if err != nil {
			return err
		}
		// Burn the step before the caller does anything with the result: a code
		// accepted twice inside its window is a replay, which is precisely what
		// this factor is supposed to prevent.
		//
		// The row count decides. Two requests carrying the same code both read
		// the same pre-update row and both pass checkCode, so it is this
		// conditional UPDATE that picks a winner — ignoring its result would let
		// them both through and leave the guard advisory.
		claimed, err := s.queries.SetUserTOTPLastStep(ctx, generated.SetUserTOTPLastStepParams{
			ID:           user.ID,
			TotpLastStep: pgtype.Int8{Int64: step, Valid: true},
		})
		if err != nil {
			return fmt.Errorf("record totp step: %w", err)
		}
		if claimed == 0 {
			return ErrSecondFactorUsed
		}
		return nil
	case MethodRecoveryCode:
		return s.consumeRecoveryCode(ctx, user.ID, code)
	default:
		return ErrUnsupportedMethod
	}
}

// Disable clears the secret and every recovery code with it. Leaving stale
// codes behind would let a code issued under an old secret satisfy a future
// enrollment the user has not consented to.
func (s *TOTPService) Disable(ctx context.Context, userID pgtype.UUID) error {
	if err := s.queries.DisableUserTOTP(ctx, userID); err != nil {
		return fmt.Errorf("disable totp: %w", err)
	}
	if err := s.queries.DeleteRecoveryCodes(ctx, userID); err != nil {
		return fmt.Errorf("delete recovery codes: %w", err)
	}
	return nil
}

// RegenerateRecoveryCodes issues a fresh set and invalidates the previous one,
// which is the only safe reading of "regenerate": a user who does this because
// they think the old list leaked must end up with the old list dead.
func (s *TOTPService) RegenerateRecoveryCodes(ctx context.Context, user generated.User) ([]string, error) {
	if !HasMFA(user) {
		return nil, ErrTOTPNotEnrolled
	}
	var codes []string
	err := store.WithTx(ctx, s.db, func(q *generated.Queries) error {
		var err error
		codes, err = replaceRecoveryCodes(ctx, q, user.ID)
		return err
	})
	if err != nil {
		return nil, err
	}
	return codes, nil
}

// RemainingRecoveryCodes reports how many are still unspent, so the UI can warn
// before the user runs out.
func (s *TOTPService) RemainingRecoveryCodes(ctx context.Context, userID pgtype.UUID) (int64, error) {
	return s.queries.CountUnusedRecoveryCodes(ctx, userID)
}

// checkCode validates a code against the user's stored secret and returns the
// time step it matched, so the caller can record it as spent.
func (s *TOTPService) checkCode(user generated.User, code string) (int64, error) {
	code = strings.TrimSpace(code)
	if code == "" {
		return 0, ErrInvalidSecondFactor
	}
	if len(user.TotpSecretEncrypted) == 0 {
		return 0, ErrTOTPNotEnrolled
	}

	secret, err := s.keyring.Decrypt(user.TotpSecretEncrypted)
	if err != nil {
		return 0, fmt.Errorf("decrypt totp secret: %w", err)
	}

	// Walk the accepted window explicitly rather than calling Validate, which
	// reports only yes/no: the step that matched is what makes replay
	// detectable, and it is not recoverable afterwards.
	now := time.Now().Unix()
	current := now / totpPeriod
	for delta := int64(-totpSkew); delta <= totpSkew; delta++ {
		step := current + delta
		expected, err := totp.GenerateCodeCustom(string(secret), time.Unix(step*totpPeriod, 0), totp.ValidateOpts{
			Period:    totpPeriod,
			Skew:      0,
			Digits:    totpDigits,
			Algorithm: totpAlgorithm,
		})
		if err != nil {
			return 0, fmt.Errorf("generate comparison code: %w", err)
		}
		if subtle.ConstantTimeCompare([]byte(expected), []byte(code)) != 1 {
			continue
		}
		if user.TotpLastStep.Valid && step <= user.TotpLastStep.Int64 {
			return 0, ErrSecondFactorUsed
		}
		return step, nil
	}
	return 0, ErrInvalidSecondFactor
}

func replaceRecoveryCodes(ctx context.Context, q *generated.Queries, userID pgtype.UUID) ([]string, error) {
	if err := q.DeleteRecoveryCodes(ctx, userID); err != nil {
		return nil, fmt.Errorf("clear recovery codes: %w", err)
	}

	codes := make([]string, 0, recoveryCodeCount)
	for range recoveryCodeCount {
		code, err := generateRecoveryCode()
		if err != nil {
			return nil, err
		}
		hash := hashRecoveryCode(code)
		if err := q.InsertRecoveryCode(ctx, generated.InsertRecoveryCodeParams{
			UserID:   userID,
			CodeHash: hash[:],
		}); err != nil {
			return nil, fmt.Errorf("store recovery code: %w", err)
		}
		codes = append(codes, code)
	}
	return codes, nil
}

func (s *TOTPService) consumeRecoveryCode(ctx context.Context, userID pgtype.UUID, code string) error {
	hash := hashRecoveryCode(code)
	_, err := s.queries.ConsumeRecoveryCode(ctx, generated.ConsumeRecoveryCodeParams{
		UserID:   userID,
		CodeHash: hash[:],
	})
	if errors.Is(err, pgx.ErrNoRows) {
		// No unused code with that hash — wrong code, or one already spent.
		// Both are the same answer to the caller.
		return ErrInvalidSecondFactor
	}
	if err != nil {
		// A dropped connection is not a wrong code. Reporting it as one costs
		// the user an attempt and a lockout strike for the database's mistake.
		return fmt.Errorf("consume recovery code: %w", err)
	}
	return nil
}

// recoveryCodeAlphabet is base32 without padding: unambiguous when read off a
// printout and typeable without a shifted key.
var recoveryCodeAlphabet = base32.StdEncoding.WithPadding(base32.NoPadding)

func generateRecoveryCode() (string, error) {
	buf := make([]byte, recoveryCodeBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generate recovery code: %w", err)
	}
	raw := recoveryCodeAlphabet.EncodeToString(buf)
	// Grouped for transcription; normaliseRecoveryCode strips the dashes again.
	return raw[:4] + "-" + raw[4:8] + "-" + raw[8:12] + "-" + raw[12:], nil
}

// hashRecoveryCode normalises before hashing so a user may type the code with
// or without its dashes, in either case.
func hashRecoveryCode(code string) [32]byte {
	normalised := strings.ToUpper(strings.ReplaceAll(strings.TrimSpace(code), "-", ""))
	return sha256.Sum256([]byte(normalised))
}
