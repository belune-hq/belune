package caddy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/belune-hq/belune/internal/proxy"
)

// "No certificates loaded" reaches us in three different shapes depending on how
// much of Caddy's TLS app exists. Development always had a tls app (local_certs
// creates one), so the 400 case only appeared on a production box — where it made
// SyncCertificates bail before the write, leaving uploaded certificates never
// pushed to the proxy at all.
func TestListPEMCerts_EmptyStates(t *testing.T) {
	tests := []struct {
		name   string
		status int
		body   string
	}{
		{
			// Stock Caddy with no TLS config at all: the path cannot be walked.
			name:   "no tls app — 400 invalid traversal path",
			status: http.StatusBadRequest,
			body:   `{"error":"invalid traversal path at: config/apps/tls/certificates"}`,
		},
		{
			// TLS app exists (an automation policy, say) but holds no certificates.
			name:   "tls app present, no certificates object",
			status: http.StatusOK,
			body:   `null`,
		},
		{
			name:   "certificates object present but empty",
			status: http.StatusOK,
			body:   `{"load_pem":[]}`,
		},
		{
			name:   "not found",
			status: http.StatusNotFound,
			body:   ``,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()

			certs, err := New(srv.URL).listPEMCerts(context.Background())
			require.NoError(t, err, "an empty certificate set is not an error")
			assert.Empty(t, certs)
		})
	}
}

// A genuine failure must still surface, or we would paper over a real outage.
func TestListPEMCerts_RealErrorStillFails(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"something is actually broken"}`))
	}))
	defer srv.Close()

	_, err := New(srv.URL).listPEMCerts(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "500")
}

// SyncCertificates must reach the write even when the proxy has no TLS app yet —
// the bug was that it returned early on the list, so nothing was ever pushed.
func TestSyncCertificates_WritesWhenNoTLSApp(t *testing.T) {
	var wrote bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"invalid traversal path at: config/apps/tls/certificates"}`))
			return
		}
		wrote = true
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	err := New(srv.URL).SyncCertificates(context.Background(), []proxy.HostCertificate{
		{Hostname: "app.example.com", CertPEM: "cert", KeyPEM: "key"},
	})
	require.NoError(t, err)
	assert.True(t, wrote, "certificate was never pushed to the proxy")
}
