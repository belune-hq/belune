package handler

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/belune-hq/belune/internal/config"
)

// A handler with nothing wired (no reconciler, runtime, probe or tailer) is the
// shape of an install whose Caddy Belune does not manage. It must answer, and
// every claim it makes must be "unknown" rather than a guess.
func TestGetProxyReconcilerStatus_NothingWiredClaimsNothing(t *testing.T) {
	h := &Handler{cfg: &config.Config{CaddyAdminURL: "http://x:2019", CaddyContainerName: "c", CaddyContainerSource: config.CaddySourceAssumed}}

	w := httptest.NewRecorder()
	h.GetProxyReconcilerStatus(w, httptest.NewRequest(http.MethodGet, "/api/maintenance/proxy", nil))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	body := w.Body.String()
	for _, want := range []string{
		`"admin_state":"unknown"`,
		`"state":"unknown"`, // container and access_log
		`"known":false`,
		`"source":"assumed"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body lacks %s:\n%s", want, body)
		}
	}
	for _, bad := range []string{`"state":"not_found"`, `"state":"unavailable"`, `"state":"healthy"`} {
		if strings.Contains(body, bad) {
			t.Errorf("body claims %s with nothing wired:\n%s", bad, body)
		}
	}
}
