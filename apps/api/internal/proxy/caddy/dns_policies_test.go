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

	"github.com/belune-hq/belune/internal/proxy"
)

// fakeAutomation is just enough of Caddy's admin API for the automation path: it
// holds the object (nil = the tls app is absent) and records writes. This proves
// what we SEND and when; it cannot prove Caddy accepts it. Only a live issuance
// does, and that is run by hand against a real provider.
type fakeAutomation struct {
	mu     sync.Mutex
	state  map[string]any
	writes []string
	reject int // status to answer a write with, 0 = accept
}

func (f *fakeAutomation) handler(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.URL.Path != automationPath {
		http.NotFound(w, r)
		return
	}
	switch r.Method {
	case http.MethodGet:
		if f.state == nil {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"invalid traversal path at: config/apps/tls/automation"}`))
			return
		}
		_ = json.NewEncoder(w).Encode(f.state)
	case http.MethodPut, http.MethodPatch:
		f.writes = append(f.writes, r.Method)
		if f.reject != 0 {
			w.WriteHeader(f.reject)
			return
		}
		if r.Method == http.MethodPut && f.state != nil {
			w.WriteHeader(http.StatusConflict)
			return
		}
		body, _ := io.ReadAll(r.Body)
		var next map[string]any
		_ = json.Unmarshal(body, &next)
		f.state = next
	}
}

func policyID(p any) string {
	m, _ := p.(map[string]any)
	id, _ := m["@id"].(string)
	return id
}

func cfPolicy(subjects ...string) proxy.DNSPolicy {
	return proxy.DNSPolicy{ProviderID: "p1", Provider: "cloudflare", Subjects: subjects, Credentials: map[string]string{"api_token": "tok"}}
}

func TestSyncDNSPolicies_CreatesWhereAbsent(t *testing.T) {
	f := &fakeAutomation{}
	srv := httptest.NewServer(http.HandlerFunc(f.handler))
	defer srv.Close()

	require.NoError(t, New(srv.URL).SyncDNSPolicies(context.Background(), []proxy.DNSPolicy{cfPolicy("*.example.com")}))

	require.NotNil(t, f.state, "the production case: no automation object existed")
	policies := f.state["policies"].([]any)
	require.Len(t, policies, 1)
	assert.Equal(t, proxy.PolicyIDPrefix+"p1", policyID(policies[0]))
	assert.Equal(t, []string{"PUT"}, f.writes)
}

// Development's local_certs policy lives in the same list; enabling DNS-01 must
// not remove it, and removing the last DNS domain must not either.
func TestSyncDNSPolicies_LeavesForeignPoliciesAlone(t *testing.T) {
	foreign := map[string]any{"issuers": []any{map[string]any{"module": "internal"}}}
	f := &fakeAutomation{state: map[string]any{"policies": []any{foreign}, "on_demand": map[string]any{"x": 1}}}
	srv := httptest.NewServer(http.HandlerFunc(f.handler))
	defer srv.Close()
	c := New(srv.URL)

	require.NoError(t, c.SyncDNSPolicies(context.Background(), []proxy.DNSPolicy{cfPolicy("a.example.com")}))
	policies := f.state["policies"].([]any)
	require.Len(t, policies, 2)
	assert.Equal(t, proxy.PolicyIDPrefix+"p1", policyID(policies[0]), "ours first, so a catch-all cannot shadow it")
	assert.Equal(t, foreign, policies[1])
	assert.Contains(t, f.state, "on_demand", "other automation members survive the rewrite")

	require.NoError(t, c.SyncDNSPolicies(context.Background(), nil))
	assert.Equal(t, []any{foreign}, f.state["policies"])
}

// Caddy reloads its whole TLS app on any write here and the reconciler calls this
// every 30s, so an unchanged set must not write.
func TestSyncDNSPolicies_NoWriteWhenUnchanged(t *testing.T) {
	f := &fakeAutomation{}
	srv := httptest.NewServer(http.HandlerFunc(f.handler))
	defer srv.Close()
	c := New(srv.URL)

	p := []proxy.DNSPolicy{cfPolicy("a.example.com", "b.example.com")}
	require.NoError(t, c.SyncDNSPolicies(context.Background(), p))
	n := len(f.writes)
	require.NoError(t, c.SyncDNSPolicies(context.Background(), p))
	assert.Len(t, f.writes, n)

	// A rotated token IS a change.
	p[0].Credentials = map[string]string{"api_token": "rotated"}
	require.NoError(t, c.SyncDNSPolicies(context.Background(), p))
	assert.Greater(t, len(f.writes), n)
}

func TestSyncDNSPolicies_NothingToDoCreatesNothing(t *testing.T) {
	f := &fakeAutomation{}
	srv := httptest.NewServer(http.HandlerFunc(f.handler))
	defer srv.Close()

	require.NoError(t, New(srv.URL).SyncDNSPolicies(context.Background(), nil))
	assert.Empty(t, f.writes)
	assert.Nil(t, f.state)
}

// doConfig only logs a 4xx. For this write that would hide the one failure that
// matters — Caddy refusing a policy leaves the domain on HTTP-01 for ever.
func TestSyncDNSPolicies_ReportsCaddyRefusal(t *testing.T) {
	f := &fakeAutomation{reject: http.StatusBadRequest}
	srv := httptest.NewServer(http.HandlerFunc(f.handler))
	defer srv.Close()

	err := New(srv.URL).SyncDNSPolicies(context.Background(), []proxy.DNSPolicy{cfPolicy("a.example.com")})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "HTTP 400")
}

// A wildcard route must not outrank an exact host it also matches, and its id must
// not carry the "*" into an admin-API path.
func TestWildcardRoutesSortAfterExactHosts(t *testing.T) {
	route := func(host, path string) json.RawMessage {
		m := map[string]any{"match": []any{map[string]any{"host": []string{host}, "path": []string{path, path + "/*"}}}}
		b, _ := json.Marshal(m)
		return b
	}
	routes := []json.RawMessage{
		route("*.example.com", "/api"),
		route("*.example.com", "/"),
		route("app.example.com", "/"),
	}
	sortRoutesBySpecificity(routes)

	var hosts []string
	for _, r := range routes {
		var p struct {
			Match []caddyMatch `json:"match"`
		}
		require.NoError(t, json.Unmarshal(r, &p))
		hosts = append(hosts, p.Match[0].Host[0]+pathFromMatcher(p.Match[0].Path))
	}
	assert.Equal(t, []string{"app.example.com/", "*.example.com/api", "*.example.com/"}, hosts)

	id := routeIDFor("*.example.com", "/")
	assert.NotContains(t, id, "*")
	assert.NotEqual(t, routeIDFor("wildcard.example.com", "/"), id)
}
