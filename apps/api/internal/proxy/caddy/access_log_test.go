package caddy

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
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
