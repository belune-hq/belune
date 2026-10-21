package proxy_test

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/bcrypt"

	"github.com/belune-hq/belune/internal/proxy"
)

// A recognisable value, so a leak anywhere (a stored config, an error, a log
// line) is a plain substring match.
const plaintext = "hunter2-VERY-SECRET-9f8e"

// realHash is a genuine bcrypt hash at the cheapest cost, which keeps these
// tests fast. It has the real shape, so it is what a pasted `caddy hash-password`
// would look like to the shape check.
func realHash(t *testing.T, password string) string {
	t.Helper()
	h, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.MinCost)
	require.NoError(t, err)
	return string(h)
}

func normalize(t *testing.T, raw string, stored ...string) (json.RawMessage, error) {
	t.Helper()
	var s json.RawMessage
	if len(stored) > 0 {
		s = json.RawMessage(stored[0])
	}
	return proxy.NormalizeFeatureConfig(proxy.FeatureBasicAuth, json.RawMessage(raw), s)
}

func mustNormalize(t *testing.T, raw string, stored ...string) json.RawMessage {
	t.Helper()
	out, err := normalize(t, raw, stored...)
	require.NoError(t, err)
	return out
}

func decode(t *testing.T, raw json.RawMessage) map[string]any {
	t.Helper()
	var m map[string]any
	require.NoError(t, json.Unmarshal(raw, &m))
	return m
}

func TestNormalize_PasswordIsHashedAndNeverStored(t *testing.T) {
	out := mustNormalize(t, `{"username":"alice","password":"`+plaintext+`"}`)
	stored := decode(t, out)

	// The stored shape: a hash and a username, and no password key under any name.
	assert.ElementsMatch(t, []string{"username", "hashed_password"}, keys(stored))
	assert.NotContains(t, string(out), plaintext, "the plaintext must not survive into what is stored")
	assert.Equal(t, "alice", stored["username"])

	hash, _ := stored["hashed_password"].(string)
	require.NoError(t, bcrypt.CompareHashAndPassword([]byte(hash), []byte(plaintext)),
		"the stored hash must verify against the password that was sent")
	assert.Error(t, bcrypt.CompareHashAndPassword([]byte(hash), []byte(plaintext+"x")))

	// 12 is basicAuthBcryptCost. Written as a literal on purpose: changing the
	// cost should have to touch a test, because it changes what an anonymous
	// client can make a route spend per guess.
	cost, err := bcrypt.Cost([]byte(hash))
	require.NoError(t, err)
	assert.Equal(t, 12, cost)

	// What is stored is what the route builder will accept.
	_, err = proxy.ParseFeatureConfig(proxy.FeatureBasicAuth, out)
	assert.NoError(t, err)
}

// ⛔ The worst bug here is not hashing: it is hashing a hash. The dashboard loads
// a stored config and re-saves it when anything else changes (the enable switch
// does), so a supplied hash has to come back exactly as it went in.
func TestNormalize_SuppliedHashIsNeverHashedAgain(t *testing.T) {
	hash := realHash(t, "whatever")
	in := `{"username":"alice","hashed_password":"` + hash + `"}`

	out := mustNormalize(t, in)

	assert.Equal(t, in, string(out), "byte-identical: same keys, same order, same hash")
	cost, err := bcrypt.Cost([]byte(decode(t, out)["hashed_password"].(string)))
	require.NoError(t, err)
	assert.Equal(t, bcrypt.MinCost, cost, "a re-hash would have come back at the work factor, not the original cost")

	// And it is stable under repetition: normalising what was stored changes nothing.
	assert.Equal(t, string(out), string(mustNormalize(t, string(out))))
	// Including when that is the hash this function produced itself.
	made := mustNormalize(t, `{"username":"alice","password":"`+plaintext+`"}`)
	assert.Equal(t, string(made), string(mustNormalize(t, string(made), string(made))))
}

// Both present: the password wins and is not an error. The edit flow always
// shows the stored hash, so changing a password means sending both.
func TestNormalize_PasswordWinsWhenBothAreSent(t *testing.T) {
	old := realHash(t, "the old password")
	out := mustNormalize(t, `{"username":"alice","password":"`+plaintext+`","hashed_password":"`+old+`"}`)

	stored := decode(t, out)
	hash := stored["hashed_password"].(string)
	assert.NotEqual(t, old, hash, "the supplied hash is discarded")
	assert.NotContains(t, string(out), old)
	assert.NoError(t, bcrypt.CompareHashAndPassword([]byte(hash), []byte(plaintext)))
	assert.Error(t, bcrypt.CompareHashAndPassword([]byte(hash), []byte("the old password")))
	assert.ElementsMatch(t, []string{"username", "hashed_password"}, keys(stored))
}

func TestNormalize_RefusesWhatCannotBeAGate(t *testing.T) {
	for name, raw := range map[string]string{
		"empty object":                   `{}`,
		"username only":                  `{"username":"alice"}`,
		"password only":                  `{"password":"x"}`,
		"empty username":                 `{"username":"","password":"x"}`,
		"empty password, no hash":        `{"username":"alice","password":""}`,
		"empty hash, no password":        `{"username":"alice","hashed_password":""}`,
		"both empty":                     `{"username":"alice","password":"","hashed_password":""}`,
		"the dashboard's untouched form": `{"username":"","password":""}`,
	} {
		t.Run(name, func(t *testing.T) {
			_, err := normalize(t, raw)
			require.Error(t, err)
			assert.Contains(t, err.Error(), "username and password are required")
			assert.False(t, errors.Is(err, proxy.ErrHashFailed), "a problem with the request is the caller's, not a hashing failure")
		})
	}

	_, err := proxy.NormalizeFeatureConfig(proxy.FeatureBasicAuth, nil, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "config required")
}

// An empty password must never be hashed into a gate that opens for "".
func TestNormalize_AnEmptyPasswordIsNotAPassword(t *testing.T) {
	hash := realHash(t, "kept")
	out := mustNormalize(t, `{"username":"alice","password":"","hashed_password":"`+hash+`"}`)
	assert.Equal(t, hash, decode(t, out)["hashed_password"], "an empty password leaves the supplied hash alone")
}

func TestNormalize_RejectsBadShapesWithoutEchoingThem(t *testing.T) {
	for name, raw := range map[string]string{
		"unknown key":     `{"username":"alice","passwd":"` + plaintext + `"}`,
		"password number": `{"username":"alice","password":123456789}`,
		"password object": `{"username":"alice","password":{"x":"` + plaintext + `"}}`,
		"not an object":   `"` + plaintext + `"`,
	} {
		t.Run(name, func(t *testing.T) {
			_, err := normalize(t, raw)
			require.Error(t, err)
			assert.NotContains(t, err.Error(), plaintext)
			assert.NotContains(t, err.Error(), "123456789")
		})
	}
}

func TestNormalize_PasswordLengthLimit(t *testing.T) {
	at72 := strings.Repeat("a", 72)
	out := mustNormalize(t, `{"username":"alice","password":"`+at72+`"}`)
	require.NoError(t, bcrypt.CompareHashAndPassword([]byte(decode(t, out)["hashed_password"].(string)), []byte(at72)))

	// bcrypt ignores everything past byte 72: refused, not silently truncated, or
	// two different long passwords sharing a prefix would both be accepted.
	for name, pw := range map[string]string{
		"73 ASCII bytes":                strings.Repeat("a", 73),
		"74 bytes of 2-byte characters": strings.Repeat("é", 37),
	} {
		t.Run(name, func(t *testing.T) {
			_, err := normalize(t, `{"username":"alice","password":"`+pw+`"}`)
			require.Error(t, err)
			assert.Contains(t, err.Error(), "at most 72 bytes")
			assert.NotContains(t, err.Error(), pw)
		})
	}
}

// The optional shape check, on the way in. It cannot say a hash is the right
// one; it catches what makes a gate that never authenticates anyone.
func TestNormalize_SuppliedHashMustBeBcryptShaped(t *testing.T) {
	good := realHash(t, "x")
	require.True(t, strings.HasPrefix(good, "$2a$04$"))

	for _, prefix := range []string{"$2a$", "$2b$", "$2y$"} {
		t.Run("accepts "+prefix, func(t *testing.T) {
			h := prefix + good[4:]
			out := mustNormalize(t, `{"username":"alice","hashed_password":"`+h+`"}`)
			assert.Equal(t, h, decode(t, out)["hashed_password"])
		})
	}

	for name, bad := range map[string]string{
		"the plaintext itself":     plaintext,
		"a short plaintext":        "hunter2",
		"a truncated copy":         good[:len(good)-10],
		"one character short":      good[:len(good)-1],
		"one character long":       good + "a",
		"surrounding whitespace":   " " + good,
		"a trailing newline":       good + "\n",
		"another scheme":           "$1$" + good[4:],
		"an unknown bcrypt prefix": "$2x$" + good[4:],
		"no cost":                  "$2a$" + good[7:],
		"a non-base64 character":   good[:len(good)-1] + "!",
		"base64 of a real hash":    "JDJhJDA0JGFiY2RlZmdoaWprbG1ub3BxcnN0dXZ3eHl6MDEyMzQ1Njc4OWFiY2RlZmdo",
	} {
		t.Run("rejects "+name, func(t *testing.T) {
			// Encoded, not concatenated: a raw newline in a JSON string would fail
			// the JSON parser and never reach the shape check.
			body, err := json.Marshal(map[string]string{"username": "alice", "hashed_password": bad})
			require.NoError(t, err)
			_, err = normalize(t, string(body))
			require.Error(t, err)
			assert.Contains(t, err.Error(), "bcrypt hash")
			assert.Contains(t, err.Error(), "$2a$", "say what shape is expected")
			// The value is, most likely, a password pasted into the wrong field.
			assert.NotContains(t, err.Error(), strings.TrimSpace(bad), "never echo what was sent")
		})
	}
}

// ⛔ The shape check must never make an existing row un-re-savable. The enable
// switch re-sends the stored config; failing it because of what the row holds
// would block the operator from turning a gate OFF.
func TestNormalize_AHashEqualToTheStoredOneIsNotRejudged(t *testing.T) {
	legacy := `{"username":"alice","hashed_password":"x"}`

	out := mustNormalize(t, legacy, legacy)
	assert.Equal(t, "x", decode(t, out)["hashed_password"])

	// A different value is judged as a new one, whatever is stored.
	_, err := normalize(t, `{"username":"alice","hashed_password":"y"}`, legacy)
	assert.Error(t, err)

	// A stored row that does not decode grants no exemption.
	for _, junk := range []string{``, `null`, `[]`, `not json`, `{"hashed_password":5}`} {
		_, err := normalize(t, `{"username":"alice","hashed_password":"x"}`, junk)
		assert.Error(t, err, "stored=%q", junk)
	}
}

// Only basic_auth is rewritten. Everything else is stored exactly as sent, as it
// always was: a redirect with no status_code must not suddenly gain a 301.
func TestNormalize_OtherFeatureTypesAreReturnedByteIdentical(t *testing.T) {
	for _, ft := range []string{proxy.FeatureHeaders, proxy.FeatureIPAllowlist, proxy.FeatureRedirect, proxy.FeatureRateLimit} {
		for _, raw := range []string{
			`{"from":"/a","to":"/b"}`,
			"{ \"ranges\" :\n [\"203.0.113.0/24\"] }",
			`{"password":"` + plaintext + `"}`,
			`not even json`,
		} {
			out, err := proxy.NormalizeFeatureConfig(ft, json.RawMessage(raw), nil)
			require.NoError(t, err, "validation is ParseFeatureConfig's job, not this function's")
			assert.Equal(t, raw, string(out), "%s must be stored exactly as sent", ft)
		}
	}
}

// ⛔ The reader is unchanged, on purpose. The route builder re-parses every
// stored row through ParseFeatureConfig, and a row it stops recognising is
// SKIPPED, so a stricter parser would serve a live route with no authentication.
// The shape check therefore lives in NormalizeFeatureConfig, on the way in.
func TestParseFeatureConfig_BasicAuthReaderIsStillLenient(t *testing.T) {
	for _, hash := range []string{"x", "$2a$10$abcdef", "$2a$10$xyz", "not-a-hash-at-all"} {
		_, err := proxy.ParseFeatureConfig(proxy.FeatureBasicAuth, json.RawMessage(`{"username":"alice","hashed_password":"`+hash+`"}`))
		assert.NoError(t, err, "a stored %q must still build a route", hash)
	}

	// And it still requires a hash: the stored form is not the input form.
	_, err := proxy.ParseFeatureConfig(proxy.FeatureBasicAuth, json.RawMessage(`{"username":"alice","password":"x"}`))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "username and hashed_password are required")
}

func keys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
