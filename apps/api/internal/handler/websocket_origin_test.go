package handler

import (
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/belune-hq/belune/internal/config"
)

// The dashboard's hostname is set at runtime from the UI, so a static allowlist
// can never contain it. Same-origin must be derived from the request instead —
// otherwise the panel's own WebSocket is rejected by its own server, which is
// exactly what happened on the first real deployment.
func TestOriginAllowed(t *testing.T) {
	h := &Handler{cfg: &config.Config{
		CORSOrigins: []string{"http://localhost:5173"},
	}}

	tests := []struct {
		name   string
		host   string
		origin string
		want   bool
	}{
		{"same-origin over http (bare IP bootstrap)", "64.110.101.24", "http://64.110.101.24", true},
		{"same-origin over https (after a domain is set)", "belune.example.com", "https://belune.example.com", true},
		{"same-origin with a port", "localhost:8080", "http://localhost:8080", true},
		{"configured cross-origin (the Vite dev server)", "localhost:8080", "http://localhost:5173", true},
		{"host casing is irrelevant", "Belune.Example.com", "https://belune.example.com", true},

		// The attack this check exists to stop: a page on another site opening a
		// socket to us with the victim's cookies.
		{"cross-site origin is rejected", "belune.example.com", "https://evil.example.com", false},
		{"lookalike host is rejected", "belune.example.com", "https://belune.example.com.evil.com", false},
		{"unconfigured origin is rejected", "localhost:8080", "http://localhost:9999", false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest("GET", "/api/ws", nil)
			r.Host = tc.host
			assert.Equal(t, tc.want, h.originAllowed(r, tc.origin))
		})
	}
}
