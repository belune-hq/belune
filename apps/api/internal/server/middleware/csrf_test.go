package middleware_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/belune-hq/belune/internal/server/middleware"
)

func okHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
}

func TestCSRF_SafeMethodsBypass(t *testing.T) {
	h := middleware.CSRF()(okHandler())
	for _, method := range []string{http.MethodGet, http.MethodHead, http.MethodOptions} {
		req := httptest.NewRequest(method, "/api/anything", nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Errorf("%s should pass through, got %d", method, rec.Code)
		}
	}
}

func TestCSRF_BearerAuthBypass(t *testing.T) {
	h := middleware.CSRF()(okHandler())
	req := httptest.NewRequest(http.MethodPost, "/api/anything", strings.NewReader("{}"))
	req.Header.Set("Authorization", "Bearer some-jwt-token")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("Bearer auth should bypass CSRF, got %d", rec.Code)
	}
}

func TestCSRF_MissingCookie(t *testing.T) {
	h := middleware.CSRF()(okHandler())
	req := httptest.NewRequest(http.MethodPost, "/api/anything", strings.NewReader("{}"))
	req.Header.Set(middleware.CSRFHeaderName, "some-token")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("missing cookie should return 403, got %d", rec.Code)
	}
}

func TestCSRF_MissingHeader(t *testing.T) {
	h := middleware.CSRF()(okHandler())
	req := httptest.NewRequest(http.MethodPost, "/api/anything", strings.NewReader("{}"))
	req.AddCookie(&http.Cookie{Name: middleware.CSRFCookieName, Value: "token-value"})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("missing header should return 403, got %d", rec.Code)
	}
}

func TestCSRF_Mismatch(t *testing.T) {
	h := middleware.CSRF()(okHandler())
	req := httptest.NewRequest(http.MethodPost, "/api/anything", strings.NewReader("{}"))
	req.AddCookie(&http.Cookie{Name: middleware.CSRFCookieName, Value: "cookie-token"})
	req.Header.Set(middleware.CSRFHeaderName, "different-token")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("mismatched token should return 403, got %d", rec.Code)
	}
}

func TestCSRF_Valid(t *testing.T) {
	token, err := middleware.GenerateCSRFToken()
	if err != nil {
		t.Fatalf("generate token: %v", err)
	}
	h := middleware.CSRF()(okHandler())
	req := httptest.NewRequest(http.MethodPost, "/api/anything", strings.NewReader("{}"))
	req.AddCookie(&http.Cookie{Name: middleware.CSRFCookieName, Value: token})
	req.Header.Set(middleware.CSRFHeaderName, token)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("valid matched token should pass, got %d", rec.Code)
	}
}

func TestCSRF_EmptyCookieValue(t *testing.T) {
	h := middleware.CSRF()(okHandler())
	req := httptest.NewRequest(http.MethodPost, "/api/anything", strings.NewReader("{}"))
	req.AddCookie(&http.Cookie{Name: middleware.CSRFCookieName, Value: ""})
	req.Header.Set(middleware.CSRFHeaderName, "some-token")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("empty cookie value should return 403, got %d", rec.Code)
	}
}
