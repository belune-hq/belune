package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/belune-hq/belune/internal/server/middleware"
)

// The authenticated surface's rate limiter had no test at all — testutil's
// server sets DisableRateLimiting, so every handler test runs with it switched
// off, and the only 429s anywhere in the suite come from the login handler's
// own lockout logic. That is how a limit sized for a script sat in front of an
// interactive dashboard through sixteen releases.
//
// These are pure in-memory tests: httprate's counter is local, so a few hundred
// requests through the middleware take microseconds and need no Docker.

// drive sends n requests as the given caller and returns the status of each.
func drive(t *testing.T, mw func(http.Handler) http.Handler, ctx context.Context, n int) []int {
	t.Helper()
	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	codes := make([]int, 0, n)
	for i := 0; i < n; i++ {
		req := httptest.NewRequest(http.MethodGet, "/api/projects", nil).WithContext(ctx)
		// Set, because httprate falls back to KeyByIP for an unauthenticated
		// request and httptest's RemoteAddr would otherwise be the key — which
		// would make every case here share one bucket and pass for the wrong
		// reason.
		req.RemoteAddr = "192.0.2.1:1234"
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		codes = append(codes, rec.Code)
	}
	return codes
}

func countOK(codes []int) int {
	n := 0
	for _, c := range codes {
		if c == http.StatusOK {
			n++
		}
	}
	return n
}

func sessionCtx(userID string) context.Context {
	return middleware.ContextWithUserID(context.Background(), userID)
}

// tokenCtx mirrors what Auth builds for a PAT: both ids are present, and
// rateLimitKey picks the token id because it is checked first.
func tokenCtx(userID, tokenID string) context.Context {
	return middleware.ContextWithTokenID(sessionCtx(userID), tokenID)
}

func TestSessionGetsTheSessionBudget(t *testing.T) {
	mw := rateLimitByCaller()
	codes := drive(t, mw, sessionCtx("user-1"), sessionRateLimit+5)

	assert.Equal(t, sessionRateLimit, countOK(codes),
		"a session should be allowed exactly sessionRateLimit requests in the window")
	assert.Equal(t, http.StatusTooManyRequests, codes[sessionRateLimit],
		"the request after the budget should be refused")
}

// The regression that matters: before the split, a dashboard tab spending
// ~33/min shared a 100/min budget with scripts, so three tabs exhausted it.
func TestSessionIsAllowedMoreThanTheTokenBudget(t *testing.T) {
	mw := rateLimitByCaller()
	codes := drive(t, mw, sessionCtx("user-1"), tokenRateLimit+50)

	assert.Equal(t, tokenRateLimit+50, countOK(codes),
		"a session must not be capped at the token budget — three dashboard tabs exceed it")
}

func TestTokenKeepsTheTokenBudget(t *testing.T) {
	mw := rateLimitByCaller()
	codes := drive(t, mw, tokenCtx("user-1", "token-1"), tokenRateLimit+5)

	assert.Equal(t, tokenRateLimit, countOK(codes))
	assert.Equal(t, http.StatusTooManyRequests, codes[tokenRateLimit])
}

// Separate counters, not just separate keys. rateLimitKey already returned
// distinct keys; what is new is that each kind is counted by its own limiter,
// and a shared counter would make the token's spend come out of the session's
// budget.
func TestATokenSpendDoesNotCostItsOwnerTheirSession(t *testing.T) {
	mw := rateLimitByCaller()

	exhausted := drive(t, mw, tokenCtx("user-1", "token-1"), tokenRateLimit+5)
	require.Equal(t, http.StatusTooManyRequests, exhausted[tokenRateLimit],
		"precondition: the token's own budget is spent")

	// Same user, now as a human. Their dashboard must be unaffected.
	codes := drive(t, mw, sessionCtx("user-1"), tokenRateLimit+50)
	assert.Equal(t, tokenRateLimit+50, countOK(codes),
		"the owner's session was charged for their token's requests")
}

func TestTwoTokensOfOneUserDoNotShareABucket(t *testing.T) {
	mw := rateLimitByCaller()

	first := drive(t, mw, tokenCtx("user-1", "token-1"), tokenRateLimit+5)
	require.Equal(t, http.StatusTooManyRequests, first[tokenRateLimit])

	second := drive(t, mw, tokenCtx("user-1", "token-2"), 10)
	assert.Equal(t, 10, countOK(second),
		"a runaway script on one token must not starve the same user's other tokens")
}

// httprate answers with http.Error by default — plain text "Too Many
// Requests", the one body in the whole API that is not {"error": …}. The SPA
// parses every failure as JSON, so a rate-limited dashboard showed a bare
// string with nothing in it to act on.
func TestTheRefusalIsTheAPIsOwnErrorShape(t *testing.T) {
	mw := rateLimitByCaller()
	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	ctx := tokenCtx("user-1", "token-1")
	var rec *httptest.ResponseRecorder
	for i := 0; i <= tokenRateLimit; i++ {
		req := httptest.NewRequest(http.MethodGet, "/api/projects", nil).WithContext(ctx)
		req.RemoteAddr = "192.0.2.1:1234"
		rec = httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
	}
	require.Equal(t, http.StatusTooManyRequests, rec.Code, "precondition: the budget is spent")

	assert.Equal(t, "application/json", rec.Header().Get("Content-Type"))
	assert.Equal(t, "60", rec.Header().Get("Retry-After"),
		"the client reads this header when the body carries no retry_after")

	var body struct {
		Error      string `json:"error"`
		RetryAfter int    `json:"retry_after"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body),
		"the refusal must be JSON: %q", rec.Body.String())
	assert.NotEmpty(t, body.Error, "the client renders this string to the operator")
	assert.Equal(t, 60, body.RetryAfter)
}
