package handler_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/belune-hq/belune/internal/testutil"
)

// The address an audit row carries comes from the proxy's header and from nothing
// else a caller can write. These go through the real router and the real audit
// writer, so what they pin is the column, not a helper.
func TestAudit_AttributesTheClientCaddyReportedAndNeverAForgedHeader(t *testing.T) {
	g := newGateEnv(t)
	base := newAuditedMCP(t)

	// put sets a route feature with the given headers and returns the address the
	// audit row for it recorded.
	put := func(headers map[string]string) string {
		t.Helper()
		var before int
		count := `SELECT count(*) FROM audit_logs WHERE action = 'upsert_route_feature'`
		require.NoError(t, env.Pool.QueryRow(context.Background(), count).Scan(&before))

		b, err := json.Marshal(map[string]any{
			"feature_type": "basic_auth", "enabled": true,
			"config": map[string]any{"username": "alice", "password": gatePassword},
		})
		require.NoError(t, err)
		req, err := http.NewRequest("PUT", base+g.featuresURL, bytes.NewReader(b))
		require.NoError(t, err)
		req.Header.Set("Content-Type", "application/json")
		for k, v := range testutil.AuthHeader(g.token) {
			req.Header.Set(k, v)
		}
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		require.Equal(t, http.StatusOK, resp.StatusCode, readBody(t, resp))

		var ip string
		require.Eventually(t, func() bool {
			var n int
			if env.Pool.QueryRow(context.Background(), count).Scan(&n) != nil || n <= before {
				return false
			}
			return env.Pool.QueryRow(context.Background(),
				`SELECT COALESCE(ip_address, '') FROM audit_logs WHERE action = 'upsert_route_feature' ORDER BY created_at DESC LIMIT 1`).Scan(&ip) == nil
		}, 3*time.Second, 10*time.Millisecond, "the audit row never arrived")
		return ip
	}

	forged := map[string]string{
		"X-Forwarded-For": "198.51.100.1, 198.51.100.2",
		"X-Real-IP":       "198.51.100.3",
		"True-Client-IP":  "198.51.100.4",
	}

	t.Run("the proxy's header is the client", func(t *testing.T) {
		h := map[string]string{"X-Belune-Client-IP": "203.0.113.77"}
		for k, v := range forged {
			h[k] = v
		}
		assert.Equal(t, "203.0.113.77", put(h))
	})

	t.Run("without it, nothing a caller sent is the client", func(t *testing.T) {
		got := put(forged)
		assert.Equal(t, "127.0.0.1", got, "the TCP peer, which is truthful about itself")
	})

	t.Run("a value that is not an address is not stored at all", func(t *testing.T) {
		// 200 bytes would have been refused by VARCHAR(45) and the row lost.
		long := bytes.Repeat([]byte("9"), 200)
		got := put(map[string]string{"X-Belune-Client-IP": string(long)})
		assert.Equal(t, "127.0.0.1", got, "an invalid header is ignored and the row still exists")
	})
}
