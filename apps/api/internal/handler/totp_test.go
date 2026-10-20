package handler_test

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/pquerna/otp"
	"github.com/pquerna/otp/totp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/belune-hq/belune/internal/store/generated"
	"github.com/belune-hq/belune/internal/testutil"
)

// codeAt generates the code an authenticator app would show at the given time,
// which is what lets these tests reason about the accepted window and about
// replay rather than only about "a correct code".
func codeAt(t *testing.T, secret string, at time.Time) string {
	t.Helper()
	code, err := totp.GenerateCodeCustom(secret, at, totp.ValidateOpts{
		Period:    30,
		Digits:    otp.DigitsSix,
		Algorithm: otp.AlgorithmSHA1,
	})
	require.NoError(t, err)
	return code
}

// enrollTOTP starts enrollment and returns the secret. It does NOT enable the
// factor — that is the distinction several of these tests are about.
func enrollTOTP(t *testing.T, token string) string {
	t.Helper()
	resp := env.DoRequest(t, "POST", "/api/auth/totp/enroll",
		map[string]string{"password": "password123"}, testutil.AuthHeader(token))
	require.Equal(t, http.StatusOK, resp.StatusCode)
	body := testutil.ReadJSON(t, resp)
	secret, ok := body["secret"].(string)
	require.True(t, ok, "enrollment must return a secret to scan")
	require.NotEmpty(t, body["uri"], "enrollment must return a provisioning URI")
	return secret
}

// enableTOTP takes a session all the way through enrollment and returns the
// secret, the recovery codes (only ever readable here), and the REPLACEMENT
// session token — enabling a factor revokes every session the user had,
// including the one that asked for it.
func enableTOTP(t *testing.T, token string) (secret string, codes []string, newToken string) {
	t.Helper()
	secret = enrollTOTP(t, token)
	resp := env.DoRequest(t, "POST", "/api/auth/totp/enroll/verify", map[string]string{
		"code": codeAt(t, secret, time.Now()),
	}, testutil.AuthHeader(token))
	require.Equal(t, http.StatusOK, resp.StatusCode)
	body := testutil.ReadJSON(t, resp)

	raw, ok := body["recovery_codes"].([]any)
	require.True(t, ok, "enabling must return recovery codes")
	codes = make([]string, 0, len(raw))
	for _, c := range raw {
		codes = append(codes, c.(string))
	}

	session, ok := body["session"].(map[string]any)
	require.True(t, ok, "enabling must hand back a session for the device that did it")
	newToken, _ = session["token"].(string)
	require.NotEmpty(t, newToken)
	return secret, codes, newToken
}

// loginCode returns a code from the NEXT step. Enrollment spends the current
// one, and the replay guard means a code is good exactly once — so a user who
// enables the factor and signs in within the same 30 seconds genuinely does
// have to wait for their app to roll over.
func loginCode(t *testing.T, secret string) string {
	t.Helper()
	return codeAt(t, secret, time.Now().Add(30*time.Second))
}

// loginChallenge performs the first login step and returns the challenge token.
// It also asserts the response carries no session, which is the property that
// makes this a second factor rather than a client-side suggestion.
func loginChallenge(t *testing.T, email, password string) (string, []string) {
	t.Helper()
	resp := env.DoRequest(t, "POST", "/api/auth/login", map[string]string{
		"email": email, "password": password,
	}, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	body := testutil.ReadJSON(t, resp)

	challenge, ok := body["challenge"].(string)
	require.True(t, ok, "a user with a second factor must get a challenge, not a session")
	require.Empty(t, body["token"], "the first step must not return an access token")
	require.Empty(t, resp.Cookies(), "the first step must not set a session cookie")

	var methods []string
	for _, m := range body["methods"].([]any) {
		methods = append(methods, m.(string))
	}
	return challenge, methods
}

// TestTOTPEnrollment_DoesNotEnableUntilVerified is the single most important
// property here: a secret the authenticator never actually stored must not be
// able to lock anyone out of their own install.
func TestTOTPEnrollment_DoesNotEnableUntilVerified(t *testing.T) {
	resetDB(t)
	token := env.SetupAdmin(t, "admin@test.com", "password123")

	secret := enrollTOTP(t, token)

	resp := env.DoRequest(t, "GET", "/api/auth/totp", nil, testutil.AuthHeader(token))
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, false, testutil.ReadJSON(t, resp)["enabled"],
		"storing a secret must not enable the factor")

	// A login at this point is still a plain login.
	resp = env.DoRequest(t, "POST", "/api/auth/login", map[string]string{
		"email": "admin@test.com", "password": "password123",
	}, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.NotEmpty(t, testutil.ReadJSON(t, resp)["token"], "an unverified enrollment must not gate login")

	// A wrong code leaves it off.
	resp = env.DoRequest(t, "POST", "/api/auth/totp/enroll/verify", map[string]string{
		"code": "000000",
	}, testutil.AuthHeader(token))
	require.Equal(t, http.StatusUnauthorized, resp.StatusCode)

	resp = env.DoRequest(t, "GET", "/api/auth/totp", nil, testutil.AuthHeader(token))
	assert.Equal(t, false, testutil.ReadJSON(t, resp)["enabled"])

	// The right code turns it on and hands over the recovery codes.
	resp = env.DoRequest(t, "POST", "/api/auth/totp/enroll/verify", map[string]string{
		"code": codeAt(t, secret, time.Now()),
	}, testutil.AuthHeader(token))
	require.Equal(t, http.StatusOK, resp.StatusCode)
	codes := testutil.ReadJSON(t, resp)["recovery_codes"].([]any)
	assert.Len(t, codes, 10)

	// And now the password alone is no longer a way in.
	loginChallenge(t, "admin@test.com", "password123")
}

// TestTOTPLogin_ChallengeAndCodeAreBothSingleUse covers the two replay windows:
// the challenge itself, and the code inside its 30-second validity.
func TestTOTPLogin_ChallengeAndCodeAreBothSingleUse(t *testing.T) {
	resetDB(t)
	token := env.SetupAdmin(t, "admin@test.com", "password123")
	secret, _, token := enableTOTP(t, token)

	challenge, methods := loginChallenge(t, "admin@test.com", "password123")
	assert.Contains(t, methods, "totp")

	code := loginCode(t, secret)
	resp := env.DoRequest(t, "POST", "/api/auth/login/verify", map[string]string{
		"challenge": challenge, "method": "totp", "code": code,
	}, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.NotEmpty(t, testutil.ReadJSON(t, resp)["token"], "a verified challenge must produce a session")

	// The challenge is spent.
	resp = env.DoRequest(t, "POST", "/api/auth/login/verify", map[string]string{
		"challenge": challenge, "method": "totp", "code": code,
	}, nil)
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode, "a challenge must not be reusable")

	// And so is the code, for the rest of its window: without the replay guard
	// a code seen once is good for another ~30 seconds.
	nextChallenge, _ := loginChallenge(t, "admin@test.com", "password123")
	resp = env.DoRequest(t, "POST", "/api/auth/login/verify", map[string]string{
		"challenge": nextChallenge, "method": "totp", "code": code,
	}, nil)
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode, "a code must not be accepted twice")
}

// TestTOTPVerify_AcceptsOneStepOfSkewAndNoMore pins the accepted window. Each
// extra step multiplies the number of codes a single guess can match, so the
// upper bound matters as much as the tolerance does.
func TestTOTPVerify_AcceptsOneStepOfSkewAndNoMore(t *testing.T) {
	resetDB(t)
	adminToken := env.SetupAdmin(t, "admin@test.com", "password123")

	cases := []struct {
		name   string
		offset time.Duration
		want   int
	}{
		{"one step behind", -30 * time.Second, http.StatusOK},
		{"one step ahead", 30 * time.Second, http.StatusOK},
		{"two steps behind", -70 * time.Second, http.StatusUnauthorized},
		{"two steps ahead", 70 * time.Second, http.StatusUnauthorized},
	}

	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// A fresh user per case: accepting a code advances the replay
			// marker, which would otherwise make later cases fail for the
			// wrong reason.
			email := "skew" + string(rune('a'+i)) + "@test.com"
			resp := env.DoRequest(t, "POST", "/api/users", map[string]string{
				"email": email, "password": "password123", "role": "member",
			}, testutil.AuthHeader(adminToken))
			require.Equal(t, http.StatusCreated, resp.StatusCode)

			userToken := env.LoginAs(t, email, "password123")
			secret := enrollTOTP(t, userToken)

			resp = env.DoRequest(t, "POST", "/api/auth/totp/enroll/verify", map[string]string{
				"code": codeAt(t, secret, time.Now().Add(tc.offset)),
			}, testutil.AuthHeader(userToken))
			assert.Equal(t, tc.want, resp.StatusCode)
		})
	}
}

// TestTOTPLogin_RepeatedBadCodesStopBeingGuessable: six digits is a million
// combinations, which is only safe because guessing is bounded. Both bounds
// live on the same failure counter, so this asserts the outcome rather than
// which one tripped first.
func TestTOTPLogin_RepeatedBadCodesStopBeingGuessable(t *testing.T) {
	resetDB(t)
	token := env.SetupAdmin(t, "admin@test.com", "password123")
	secret, _, token := enableTOTP(t, token)

	challenge, _ := loginChallenge(t, "admin@test.com", "password123")
	for range 5 {
		resp := env.DoRequest(t, "POST", "/api/auth/login/verify", map[string]string{
			"challenge": challenge, "method": "totp", "code": "000000",
		}, nil)
		assert.NotEqual(t, http.StatusOK, resp.StatusCode)
		resp.Body.Close()
	}

	// A genuinely correct code no longer helps: the budget is spent.
	resp := env.DoRequest(t, "POST", "/api/auth/login/verify", map[string]string{
		"challenge": challenge, "method": "totp", "code": loginCode(t, secret),
	}, nil)
	assert.NotEqual(t, http.StatusOK, resp.StatusCode,
		"a challenge must not survive an unlimited number of guesses")
	resp.Body.Close()
}

// TestRecoveryCode_WorksOnceAndOnlyOnce covers the lost-device path, and the
// used_at column that makes single-use enforceable rather than advisory.
func TestRecoveryCode_WorksOnceAndOnlyOnce(t *testing.T) {
	resetDB(t)
	token := env.SetupAdmin(t, "admin@test.com", "password123")
	_, codes, _ := enableTOTP(t, token)

	challenge, methods := loginChallenge(t, "admin@test.com", "password123")
	assert.Contains(t, methods, "recovery_code", "a user holding codes can present one")

	resp := env.DoRequest(t, "POST", "/api/auth/login/verify", map[string]string{
		"challenge": challenge, "method": "recovery_code", "code": codes[0],
	}, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.NotEmpty(t, testutil.ReadJSON(t, resp)["token"])

	challenge, _ = loginChallenge(t, "admin@test.com", "password123")
	resp = env.DoRequest(t, "POST", "/api/auth/login/verify", map[string]string{
		"challenge": challenge, "method": "recovery_code", "code": codes[0],
	}, nil)
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode, "a spent recovery code must not work again")

	// A different one from the same set still does.
	resp = env.DoRequest(t, "POST", "/api/auth/login/verify", map[string]string{
		"challenge": challenge, "method": "recovery_code", "code": codes[1],
	}, nil)
	assert.Equal(t, http.StatusOK, resp.StatusCode)
}

// TestRecoveryCodes_RegenerateKillsThePreviousSet: the usual reason to
// regenerate is believing the old list leaked, so the old list must die.
func TestRecoveryCodes_RegenerateKillsThePreviousSet(t *testing.T) {
	resetDB(t)
	token := env.SetupAdmin(t, "admin@test.com", "password123")
	secret, old, token := enableTOTP(t, token)

	resp := env.DoRequest(t, "POST", "/api/auth/totp/recovery-codes", map[string]string{
		"password": "password123", "code": loginCode(t, secret),
	}, testutil.AuthHeader(token))
	require.Equal(t, http.StatusOK, resp.StatusCode)
	fresh := testutil.ReadJSON(t, resp)["recovery_codes"].([]any)
	require.Len(t, fresh, 10)
	assert.NotEqual(t, old[0], fresh[0].(string))

	// The old set is gone even though it was never used.
	challenge, _ := loginChallenge(t, "admin@test.com", "password123")
	resp = env.DoRequest(t, "POST", "/api/auth/login/verify", map[string]string{
		"challenge": challenge, "method": "recovery_code", "code": old[0],
	}, nil)
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)

	resp = env.DoRequest(t, "POST", "/api/auth/login/verify", map[string]string{
		"challenge": challenge, "method": "recovery_code", "code": fresh[0].(string),
	}, nil)
	assert.Equal(t, http.StatusOK, resp.StatusCode)
}

// TestTOTPDisable_NeedsPasswordAndCode: disabling the factor is the first thing
// someone with a stolen session would try.
func TestTOTPDisable_NeedsPasswordAndCode(t *testing.T) {
	resetDB(t)
	token := env.SetupAdmin(t, "admin@test.com", "password123")
	secret, _, token := enableTOTP(t, token)

	resp := env.DoRequest(t, "POST", "/api/auth/totp/disable", map[string]string{
		"password": "password123",
	}, testutil.AuthHeader(token))
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode, "a session alone must not disable the factor")

	resp = env.DoRequest(t, "POST", "/api/auth/totp/disable", map[string]string{
		"password": "wrong-password", "code": loginCode(t, secret),
	}, testutil.AuthHeader(token))
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)

	resp = env.DoRequest(t, "POST", "/api/auth/totp/disable", map[string]string{
		"password": "password123", "code": loginCode(t, secret),
	}, testutil.AuthHeader(token))
	require.Equal(t, http.StatusOK, resp.StatusCode)

	// Disabling clears the recovery codes with the secret: a stale code must not
	// survive to satisfy a future enrollment.
	ctx := context.Background()
	user, err := env.Queries.GetUserByEmail(ctx, "admin@test.com")
	require.NoError(t, err)
	assert.False(t, user.TotpEnabledAt.Valid)
	assert.Empty(t, user.TotpSecretEncrypted)

	remaining, err := env.Queries.CountUnusedRecoveryCodes(ctx, user.ID)
	require.NoError(t, err)
	assert.Zero(t, remaining)
}

// TestTOTPEnrollment_NeedsThePassword: a hijacked session must not be able to
// enrol its own authenticator. Enabling revokes every other session and hands
// this one the only factor, so a session-only enrolment locks the real owner
// out of their account with no recovery codes and no way back.
func TestTOTPEnrollment_NeedsThePassword(t *testing.T) {
	resetDB(t)
	token := env.SetupAdmin(t, "admin@test.com", "password123")

	resp := env.DoRequest(t, "POST", "/api/auth/totp/enroll", map[string]string{}, testutil.AuthHeader(token))
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode,
		"a session alone must not start enrolment")
	resp.Body.Close()

	resp = env.DoRequest(t, "POST", "/api/auth/totp/enroll",
		map[string]string{"password": "not-the-password"}, testutil.AuthHeader(token))
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
	resp.Body.Close()

	// And nothing was stored: a rejected attempt must not leave a secret behind.
	user, err := env.Queries.GetUserByEmail(context.Background(), "admin@test.com")
	require.NoError(t, err)
	assert.Empty(t, user.TotpSecretEncrypted)
}

// TestRegenerateRecoveryCodes_NeedsACode: ten fresh recovery codes are ten
// working second factors, so this endpoint hands out the very thing the factor
// exists to withhold. It has to cost what disabling costs.
func TestRegenerateRecoveryCodes_NeedsACode(t *testing.T) {
	resetDB(t)
	token := env.SetupAdmin(t, "admin@test.com", "password123")
	secret, _, token := enableTOTP(t, token)

	resp := env.DoRequest(t, "POST", "/api/auth/totp/recovery-codes",
		map[string]string{"password": "password123"}, testutil.AuthHeader(token))
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode,
		"the password alone must not mint new recovery codes")
	resp.Body.Close()

	resp = env.DoRequest(t, "POST", "/api/auth/totp/recovery-codes",
		map[string]string{"password": "password123", "code": loginCode(t, secret)},
		testutil.AuthHeader(token))
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	resp.Body.Close()
}

// TestTOTPLogin_PasswordDoesNotResetTheLockout: the account lockout is the only
// bound that survives an attacker who already has the password. Clearing it on a
// correct password let them reset the counter between code guesses simply by
// posting the first step again.
func TestTOTPLogin_PasswordDoesNotResetTheLockout(t *testing.T) {
	resetDB(t)
	token := env.SetupAdmin(t, "admin@test.com", "password123")
	enableTOTP(t, token)

	// Five wrong codes, each time starting a brand new challenge with a correct
	// password — the attack the reset made free.
	for range 5 {
		challenge, _ := loginChallenge(t, "admin@test.com", "password123")
		resp := env.DoRequest(t, "POST", "/api/auth/login/verify", map[string]string{
			"challenge": challenge, "method": "totp", "code": "000000",
		}, nil)
		resp.Body.Close()
	}

	resp := env.DoRequest(t, "POST", "/api/auth/login", map[string]string{
		"email": "admin@test.com", "password": "password123",
	}, nil)
	assert.Equal(t, http.StatusTooManyRequests, resp.StatusCode,
		"repeated second-factor failures must lock the account, not be forgiven by the password")
	resp.Body.Close()
}

// TestTOTPDisable_AcceptsARecoveryCode: the authenticator being gone is the
// most likely reason someone is turning the factor off, so the way out must not
// require the very thing they lost. Recovery codes are a method on the same
// endpoint rather than a separate flow.
func TestTOTPDisable_AcceptsARecoveryCode(t *testing.T) {
	resetDB(t)
	token := env.SetupAdmin(t, "admin@test.com", "password123")
	_, codes, token := enableTOTP(t, token)

	resp := env.DoRequest(t, "POST", "/api/auth/totp/disable", map[string]string{
		"password": "password123", "method": "recovery_code", "code": codes[0],
	}, testutil.AuthHeader(token))
	require.Equal(t, http.StatusOK, resp.StatusCode,
		"a recovery code must be able to turn the factor off")
	resp.Body.Close()

	user, err := env.Queries.GetUserByEmail(context.Background(), "admin@test.com")
	require.NoError(t, err)
	assert.False(t, user.TotpEnabledAt.Valid)
	assert.Empty(t, user.TotpSecretEncrypted)

	// And the password alone gets them back in.
	resp = env.DoRequest(t, "POST", "/api/auth/login", map[string]string{
		"email": "admin@test.com", "password": "password123",
	}, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.NotEmpty(t, testutil.ReadJSON(t, resp)["token"])
}

// TestAdminResetTOTP_ClearsTheFactorAndIsAudited covers the lost-device case
// recovery codes did not: an admin can already do nearly anything, so the
// control here is visibility, not permission.
func TestAdminResetTOTP_ClearsTheFactorAndIsAudited(t *testing.T) {
	resetDB(t)
	adminToken := env.SetupAdmin(t, "admin@test.com", "password123")

	resp := env.DoRequest(t, "POST", "/api/users", map[string]string{
		"email": "locked-out@test.com", "password": "password123", "role": "member",
	}, testutil.AuthHeader(adminToken))
	require.Equal(t, http.StatusCreated, resp.StatusCode)
	memberID := extractID(testutil.ReadJSON(t, resp)["id"])

	memberToken := env.LoginAs(t, "locked-out@test.com", "password123")
	enableTOTP(t, memberToken)

	// Steps up with the admin's OWN current password before clearing someone
	// else's factor.
	resp = env.DoRequest(t, "POST", "/api/users/"+memberID+"/totp/reset", map[string]string{
		"current_password": "password123",
	}, testutil.AuthHeader(adminToken))
	require.Equal(t, http.StatusOK, resp.StatusCode)

	// The password alone gets them back in.
	resp = env.DoRequest(t, "POST", "/api/auth/login", map[string]string{
		"email": "locked-out@test.com", "password": "password123",
	}, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.NotEmpty(t, testutil.ReadJSON(t, resp)["token"])

	user, err := env.Queries.GetUserByEmail(context.Background(), "locked-out@test.com")
	require.NoError(t, err)
	assert.False(t, user.TotpEnabledAt.Valid)
	assert.Empty(t, user.TotpSecretEncrypted, "the reset must clear the secret, not just the flag")
}

// TestAdminResetTOTP_RequiresCallersOwnPassword pins the same step-up fix as
// TestResetUserPassword_RequiresCallersOwnPassword: a hijacked or borrowed
// admin session must not be able to strip another admin's second factor with
// nothing re-checked.
func TestAdminResetTOTP_RequiresCallersOwnPassword(t *testing.T) {
	resetDB(t)
	adminToken := env.SetupAdmin(t, "admin@test.com", "password123")

	resp := env.DoRequest(t, "POST", "/api/users", map[string]string{
		"email": "locked-out@test.com", "password": "password123", "role": "member",
	}, testutil.AuthHeader(adminToken))
	require.Equal(t, http.StatusCreated, resp.StatusCode)
	memberID := extractID(testutil.ReadJSON(t, resp)["id"])

	memberToken := env.LoginAs(t, "locked-out@test.com", "password123")
	enableTOTP(t, memberToken)

	resp = env.DoRequest(t, "POST", "/api/users/"+memberID+"/totp/reset", map[string]string{
		"current_password": "wrong-password",
	}, testutil.AuthHeader(adminToken))
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
	resp.Body.Close()

	user, err := env.Queries.GetUserByEmail(context.Background(), "locked-out@test.com")
	require.NoError(t, err)
	assert.True(t, user.TotpEnabledAt.Valid, "the target's factor must be untouched")
}

// TestAdminResetTOTP_RequiresCallerMFAWhenEnrolled pins a fix for a gap an
// Opus review found: the caller's password alone was not enough when the
// CALLER themself has a second factor enrolled — DisableTOTP,
// RegenerateRecoveryCodes, and CreateHostShellSession already require a
// fresh code too in that case, and stripping someone ELSE's factor should
// not need less proof than disabling your own.
func TestAdminResetTOTP_RequiresCallerMFAWhenEnrolled(t *testing.T) {
	resetDB(t)
	adminToken := env.SetupAdmin(t, "admin@test.com", "password123")
	adminSecret, _, adminToken := enableTOTP(t, adminToken)

	resp := env.DoRequest(t, "POST", "/api/users", map[string]string{
		"email": "locked-out@test.com", "password": "password123", "role": "member",
	}, testutil.AuthHeader(adminToken))
	require.Equal(t, http.StatusCreated, resp.StatusCode)
	memberID := extractID(testutil.ReadJSON(t, resp)["id"])

	memberToken := env.LoginAs(t, "locked-out@test.com", "password123")
	enableTOTP(t, memberToken)

	// Correct password, no code from the (MFA-enrolled) caller: rejected.
	resp = env.DoRequest(t, "POST", "/api/users/"+memberID+"/totp/reset", map[string]string{
		"current_password": "password123",
	}, testutil.AuthHeader(adminToken))
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode, "an MFA-enrolled caller must also supply a code")
	resp.Body.Close()

	target, err := env.Queries.GetUserByEmail(context.Background(), "locked-out@test.com")
	require.NoError(t, err)
	assert.True(t, target.TotpEnabledAt.Valid, "the target's factor must be untouched")

	// Correct password AND a valid code from the caller: succeeds. loginCode,
	// not codeAt(time.Now()) — enrolling just above already spent this
	// window's code, so the identical value here would 401 as a replay.
	resp = env.DoRequest(t, "POST", "/api/users/"+memberID+"/totp/reset", map[string]string{
		"current_password": "password123",
		"code":             loginCode(t, adminSecret),
	}, testutil.AuthHeader(adminToken))
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	resp.Body.Close()
}

// TestHostShell_RequiresTheSecondFactorWhenEnrolled: the host shell is the
// highest-privilege action in the product, and its step-up re-auth defends
// against a hijacked session but not a stolen password.
func TestHostShell_RequiresTheSecondFactorWhenEnrolled(t *testing.T) {
	resetDB(t)
	ctx := context.Background()
	token := env.SetupAdmin(t, "admin@test.com", "password123")
	secret, _, token := enableTOTP(t, token)

	_, err := env.Queries.UpsertSetting(ctx, generated.UpsertSettingParams{
		Key: "host_shell_enabled", Value: "true",
	})
	require.NoError(t, err)
	// TruncateAll does not clear settings, so leaving this on would silently
	// arm the host shell for every test that runs after this one.
	t.Cleanup(func() {
		_, _ = env.Queries.UpsertSetting(context.Background(), generated.UpsertSettingParams{
			Key: "host_shell_enabled", Value: "false",
		})
	})

	resp := env.DoRequest(t, "POST", "/api/maintenance/host-shell", map[string]string{
		"password": "password123",
	}, testutil.AuthHeader(token))
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode,
		"password-only step-up must not open a host root shell for a 2FA user")
	assert.Contains(t, testutil.ReadJSON(t, resp)["error"], "verification code")

	// With the code the gate passes — it then fails further along, on the mocked
	// runtime, which is exactly how far this test is meant to reach.
	resp = env.DoRequest(t, "POST", "/api/maintenance/host-shell", map[string]string{
		"password": "password123", "code": loginCode(t, secret),
	}, testutil.AuthHeader(token))
	assert.NotEqual(t, http.StatusUnauthorized, resp.StatusCode,
		"a correct code must get past the step-up gate")
	resp.Body.Close()
}
