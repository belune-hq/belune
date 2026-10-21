package proxy

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"

	"golang.org/x/crypto/bcrypt"
)

// basicAuthBcryptCost is the work factor a basic_auth password is hashed at. It
// is chosen, not inherited: user passwords use bcrypt.DefaultCost without a
// stated reason, and Caddy's own `hash-password` uses 14.
//
// Caddy verifies this hash when requests arrive, and a wrong guess costs it the
// same as a right one, so the cost is also the CPU an anonymous client can make
// a route spend per attempt. 14 (about 1.1s on a 4-core machine) makes that a
// real lever against whatever sits behind the gate; 10 (about 70ms) is the
// floor; 12 (about 270ms) hashes a password in a fraction of a second here and
// keeps each guess expensive.
const basicAuthBcryptCost = 12

// bcryptMaxPasswordBytes is bcrypt's input limit: it ignores everything past the
// 72nd byte, so a longer password would be silently truncated and a different
// long password with the same first 72 bytes would also be accepted. Refused
// rather than truncated.
const bcryptMaxPasswordBytes = 72

// bcryptShape is a bcrypt hash in Modular Crypt Format: $2a$, $2b$ or $2y$, a
// two-digit cost, then 53 characters of salt and digest in bcrypt's base64
// alphabet. It cannot say a hash is the right one, but it catches what produces
// a gate that never authenticates anyone: a pasted plaintext, a truncated copy.
var bcryptShape = regexp.MustCompile(`^\$2[aby]\$[0-9]{2}\$[./A-Za-z0-9]{53}$`)

// ErrHashFailed marks a failure to hash, which is the server's fault and not the
// caller's: every other NormalizeFeatureConfig error is a problem with what was
// sent and belongs in a 400.
var ErrHashFailed = errors.New("hashing failed")

// NormalizeFeatureConfig turns the config a client SENT into the config that is
// STORED, and the stored form is what must be persisted.
//
// Callers must write the returned bytes and never the request's: ParseFeatureConfig
// validates and discards its result, so a handler that stores what it received
// would put a plaintext basic_auth password in the database, return it from
// every later read, and hand it to the route builder.
//
// Only basic_auth is rewritten, and every other type comes back byte-identical.
// For basic_auth a client may send a password, which is hashed, or a
// hashed_password, which is kept exactly as given and never hashed again (the
// dashboard loads a stored config and re-saves it when anything else changes, so
// hashing a hash would break a working gate on an unrelated edit). If both are
// sent the password wins: the edit flow always shows the stored hash, so
// changing a password means sending both, and refusing that would break the
// feature exactly when it is needed.
//
// stored is the config currently saved for this domain and type, or nil. A hash
// equal to it is kept as it is without being judged: re-saving an existing row,
// as the enable switch does, must never start failing because of what it holds.
// Any other hash must be bcrypt-shaped. That check is made here, on the way in,
// and deliberately NOT in ParseFeatureConfig: the route builder re-parses every
// stored row, and a stricter parser would drop a live gate it no longer
// recognises, which serves the route without authentication.
//
// No error repeats the value it was sent: a hashed_password that fails the shape
// check is most likely a plaintext password pasted into the wrong field.
func NormalizeFeatureConfig(featureType string, raw, stored json.RawMessage) (json.RawMessage, error) {
	if featureType != FeatureBasicAuth {
		return raw, nil
	}
	if len(raw) == 0 {
		return nil, fmt.Errorf("%s: config required", FeatureBasicAuth)
	}

	var in BasicAuthConfig
	if err := strictUnmarshal(raw, &in); err != nil {
		return nil, fmt.Errorf("%s: %w", FeatureBasicAuth, err)
	}
	if in.Username == "" || (in.Password == "" && in.HashedPassword == "") {
		return nil, fmt.Errorf("%s: username and password are required (or hashed_password instead of password, for a bcrypt hash you already have)", FeatureBasicAuth)
	}

	out := BasicAuthConfig{Username: in.Username}
	switch {
	case in.Password != "":
		if len(in.Password) > bcryptMaxPasswordBytes {
			return nil, fmt.Errorf("%s: password must be at most %d bytes (bcrypt ignores anything longer)", FeatureBasicAuth, bcryptMaxPasswordBytes)
		}
		hash, err := bcrypt.GenerateFromPassword([]byte(in.Password), basicAuthBcryptCost)
		if err != nil {
			return nil, fmt.Errorf("%s: %w: %w", FeatureBasicAuth, ErrHashFailed, err)
		}
		out.HashedPassword = string(hash)
	case in.HashedPassword == storedHashedPassword(stored) || bcryptShape.MatchString(in.HashedPassword):
		out.HashedPassword = in.HashedPassword
	default:
		return nil, fmt.Errorf("%s: hashed_password must be a bcrypt hash (it looks like $2a$12$ followed by 53 characters). Send password instead and Belune will hash it, or make one with `caddy hash-password`", FeatureBasicAuth)
	}

	normalized, err := json.Marshal(out)
	if err != nil {
		return nil, fmt.Errorf("%s: %w: %w", FeatureBasicAuth, ErrHashFailed, err)
	}
	return normalized, nil
}

// storedHashedPassword reads the hash out of a saved basic_auth config. It is
// lenient on purpose: a stored row is not re-judged here, so anything that does
// not decode simply grants no exemption.
func storedHashedPassword(stored json.RawMessage) string {
	if len(stored) == 0 {
		return ""
	}
	var c BasicAuthConfig
	if err := json.Unmarshal(stored, &c); err != nil {
		return ""
	}
	return c.HashedPassword
}
