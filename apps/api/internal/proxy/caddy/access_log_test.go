package caddy

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The access log file is world-readable and kept for a week. A named log with no
// `include` receives every logger in Caddy, so what this pins is not the shape of
// the config but the property it exists for: only access entries reach the file.
// The behaviour itself — that Caddy honours the include — was measured against a
// real Caddy; a mock cannot prove it, and this guards against the include (or the
// redaction beside it) being dropped.
func TestConfigureAccessLogs_ScopesTheFileToAccessEntries(t *testing.T) {
	var (
		mu     sync.Mutex
		writes = map[string]json.RawMessage{}
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		writes[r.Method+" "+r.URL.Path] = body
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	require.NoError(t, New(srv.URL).ConfigureAccessLogs(context.Background()))

	mu.Lock()
	defer mu.Unlock()

	var logging struct {
		Logs map[string]struct {
			Include []string `json:"include"`
			Exclude []string `json:"exclude"`
			Writer  struct {
				Mode string `json:"mode"`
			} `json:"writer"`
			Encoder struct {
				Format string                       `json:"format"`
				Fields map[string]map[string]string `json:"fields"`
			} `json:"encoder"`
		} `json:"logs"`
	}
	require.NoError(t, json.Unmarshal(writes["PUT /config/logging"], &logging))

	var server struct {
		DefaultLoggerName string `json:"default_logger_name"`
	}
	require.NoError(t, json.Unmarshal(writes["PUT /config/apps/http/servers/srv0/logs"], &server))
	require.NotEmpty(t, server.DefaultLoggerName)

	file, ok := logging.Logs[server.DefaultLoggerName]
	require.True(t, ok, "the server must point at the log that is configured")

	// Caddy names a server's access entries "http.log.access.<default_logger_name>".
	// An include that does not match that name would silently log nothing, and
	// request_logs would go quiet — so the exact name is part of the contract.
	assert.Equal(t, []string{"http.log.access." + server.DefaultLoggerName}, file.Include,
		"without an include the file is a catch-all for every Caddy logger")

	// The include keeps other loggers out; it must not replace the redaction of
	// the deploy-hook token, which is what protects the entries that remain.
	assert.Equal(t, "filter", file.Encoder.Format)
	assert.Contains(t, file.Encoder.Fields, "request>uri")
	assert.Equal(t, "0644", file.Writer.Mode, "the tailer reads this file as a different user")
}

// capturedLoggingConfig runs ConfigureAccessLogs against a stand-in admin API and
// returns the body it PUT to /config/logging.
func capturedLoggingConfig(t *testing.T) []byte {
	t.Helper()
	var (
		mu      sync.Mutex
		logging []byte
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if r.URL.Path == "/config/logging" {
			mu.Lock()
			logging = body
			mu.Unlock()
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	require.NoError(t, New(srv.URL).ConfigureAccessLogs(context.Background()))

	mu.Lock()
	defer mu.Unlock()
	return logging
}

// Caddy's filter encoder takes ONE filter per field, so the deploy-hook token and
// the `?token=` of the reset and invitation links must be redacted by one regexp.
// Splitting them, or adding a second filter for the field, silently drops one — and
// a test that only looked for `token=` would not notice the deploy hook reopening.
// This applies the regexp Belune actually sends, with Go's own regexp (the engine
// Caddy uses), to every shape that carries a secret and to the ones that must not
// be touched.
func TestConfigureAccessLogs_OneFilterRedactsEveryCredential(t *testing.T) {
	logging := capturedLoggingConfig(t)
	var cfg struct {
		Logs map[string]struct {
			Encoder struct {
				Fields map[string]struct {
					Filter string `json:"filter"`
					Regexp string `json:"regexp"`
					Value  string `json:"value"`
				} `json:"fields"`
			} `json:"encoder"`
		} `json:"logs"`
	}
	require.NoError(t, json.Unmarshal(logging, &cfg))
	fields := cfg.Logs["http.access.srv0"].Encoder.Fields

	cases := []struct{ name, in, want string }{
		{"deploy hook", "/api/webhooks/deploy/ABC123", "/api/webhooks/deploy/[REDACTED]"},
		{"deploy hook keeps its query", "/api/webhooks/deploy/ABC123?ref=main", "/api/webhooks/deploy/[REDACTED]?ref=main"},
		{"reset link", "/reset-password?token=XYZ789", "/reset-password?token=[REDACTED]"},
		{"invite link", "/accept-invite?token=XYZ789", "/accept-invite?token=[REDACTED]"},
		{"invitation API", "/api/auth/invitation?token=XYZ789", "/api/auth/invitation?token=[REDACTED]"},
		{"token after another parameter", "/x?a=1&token=XYZ789", "/x?a=1&token=[REDACTED]"},
		{"later parameters survive", "/x?token=XYZ789&b=2", "/x?token=[REDACTED]&b=2"},
		{"referer", "https://belune.example.com/reset-password?token=XYZ789", "https://belune.example.com/reset-password?token=[REDACTED]"},
		// What must not change: a different parameter that merely ends in "token".
		{"access_token is left alone", "/x?access_token=KEEP", "/x?access_token=KEEP"},
		{"a plain path is left alone", "/api/projects", "/api/projects"},
	}

	// The Referer carries the page's own URL on every sub-request, so it needs the
	// same redaction as the URI.
	for _, field := range []string{"request>uri", "request>headers>Referer"} {
		f, ok := fields[field]
		require.True(t, ok, "%s is not filtered", field)
		require.Equal(t, "regexp", f.Filter)
		re, err := regexp.Compile(f.Regexp)
		require.NoError(t, err, "Caddy would refuse this filter")
		for _, tc := range cases {
			assert.Equal(t, tc.want, re.ReplaceAllString(tc.in, f.Value), "%s: %s", field, tc.name)
		}
	}
}

// The access entries are copied to Caddy's default log (stderr) unless it excludes
// them, and nothing there filters the URI. Only the access logger may be excluded:
// the TLS status pipeline reads ACME failures from that same stream.
func TestConfigureAccessLogs_DefaultLogDropsOnlyAccessEntries(t *testing.T) {
	logging := capturedLoggingConfig(t)
	var cfg struct {
		Logs map[string]map[string]json.RawMessage `json:"logs"`
	}
	require.NoError(t, json.Unmarshal(logging, &cfg))

	def, ok := cfg.Logs["default"]
	require.True(t, ok, "without an explicit default log the access entries are copied to stderr")

	var exclude []string
	require.NoError(t, json.Unmarshal(def["exclude"], &exclude))
	assert.Equal(t, []string{"http.log.access.http.access.srv0"}, exclude,
		"excluding anything broader would hide the ACME/TLS lines the TLS status badges depend on")

	// Writer, encoder and level are deliberately left to Caddy's defaults: setting
	// them would change the format the line hook parses.
	assert.Len(t, def, 1, "the default log should carry only its exclude")
}
