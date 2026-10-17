package handler_test

import (
	"context"
	"fmt"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/weiliang79/belune/internal/testutil"
)

func TestAddDomain(t *testing.T) {
	resetDB(t)
	token := env.SetupAdmin(t, "admin@test.com", "password123")
	project := env.CreateProject(t, token, "Test Project", "test-project")
	projectID := extractID(project["id"])
	app := env.CreateApplication(t, token, projectID, map[string]any{
		"name": "Domain App", "type": "git", "build_type": "dockerfile", "source_repo": "https://github.com/test/repo",
	})
	appID := extractID(app["id"])

	resp := env.DoRequest(t, "POST", fmt.Sprintf("/api/projects/%s/applications/%s/domains", projectID, appID), map[string]any{
		"hostname":    "example.com",
		"ssl_enabled": true,
	}, testutil.AuthHeader(token))
	assert.Equal(t, http.StatusCreated, resp.StatusCode)
	result := testutil.ReadJSON(t, resp)
	assert.Equal(t, "example.com", result["hostname"])

	// Verify proxy AddRoute was called
	require.Len(t, env.Proxy.AddedRoutes, 1)
	assert.Equal(t, "example.com", env.Proxy.AddedRoutes[0].Hostname)
}

func TestRemoveDomain(t *testing.T) {
	resetDB(t)
	token := env.SetupAdmin(t, "admin@test.com", "password123")
	project := env.CreateProject(t, token, "Test Project", "test-project")
	projectID := extractID(project["id"])
	app := env.CreateApplication(t, token, projectID, map[string]any{
		"name": "Domain App", "type": "git", "build_type": "dockerfile", "source_repo": "https://github.com/test/repo",
	})
	appID := extractID(app["id"])

	// Add domain
	resp := env.DoRequest(t, "POST", fmt.Sprintf("/api/projects/%s/applications/%s/domains", projectID, appID), map[string]any{
		"hostname": "remove.example.com",
	}, testutil.AuthHeader(token))
	domain := testutil.ReadJSON(t, resp)
	domainID := extractID(domain["id"])

	// Remove domain
	resp = env.DoRequest(t, "DELETE", fmt.Sprintf("/api/projects/%s/applications/%s/domains/%s", projectID, appID, domainID), nil, testutil.AuthHeader(token))
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	resp.Body.Close()

	// Verify proxy RemoveRoute was called
	require.Len(t, env.Proxy.RemovedRoutes, 1)
	assert.Equal(t, "remove.example.com", env.Proxy.RemovedRoutes[0])
}

func TestAddDomainWithConfig(t *testing.T) {
	resetDB(t)
	token := env.SetupAdmin(t, "admin@test.com", "password123")
	project := env.CreateProject(t, token, "Test Project", "test-project")
	projectID := extractID(project["id"])
	app := env.CreateApplication(t, token, projectID, map[string]any{
		"name": "Domain App", "type": "git", "build_type": "dockerfile", "source_repo": "https://github.com/test/repo",
	})
	appID := extractID(app["id"])

	resp := env.DoRequest(t, "POST", fmt.Sprintf("/api/projects/%s/applications/%s/domains", projectID, appID), map[string]any{
		"hostname":       "config.example.com",
		"ssl_enabled":    true,
		"container_port": 3000,
		"force_https":    true,
		"ssl_mode":       "automatic",
	}, testutil.AuthHeader(token))
	assert.Equal(t, http.StatusCreated, resp.StatusCode)
	result := testutil.ReadJSON(t, resp)
	assert.Equal(t, "config.example.com", result["hostname"])
	assert.Equal(t, true, result["force_https"])
	assert.Equal(t, "automatic", result["ssl_mode"])

	// Verify proxy received the config
	require.Len(t, env.Proxy.AddedRoutes, 1)
	assert.Equal(t, true, env.Proxy.AddedRoutes[0].ForceHTTPS)
}

func TestUpdateDomain(t *testing.T) {
	resetDB(t)
	token := env.SetupAdmin(t, "admin@test.com", "password123")
	project := env.CreateProject(t, token, "Test Project", "test-project")
	projectID := extractID(project["id"])
	app := env.CreateApplication(t, token, projectID, map[string]any{
		"name": "Domain App", "type": "git", "build_type": "dockerfile", "source_repo": "https://github.com/test/repo",
	})
	appID := extractID(app["id"])

	// Add domain
	resp := env.DoRequest(t, "POST", fmt.Sprintf("/api/projects/%s/applications/%s/domains", projectID, appID), map[string]any{
		"hostname": "update.example.com",
	}, testutil.AuthHeader(token))
	domain := testutil.ReadJSON(t, resp)
	domainID := extractID(domain["id"])

	// Update domain
	resp = env.DoRequest(t, "PUT", fmt.Sprintf("/api/projects/%s/applications/%s/domains/%s", projectID, appID, domainID), map[string]any{
		"hostname":    "update.example.com",
		"force_https": true,
		"ssl_mode":    "off",
	}, testutil.AuthHeader(token))
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	updated := testutil.ReadJSON(t, resp)
	assert.Equal(t, "off", updated["ssl_mode"])
}

func TestAddDomain_InvalidSSLMode(t *testing.T) {
	resetDB(t)
	token := env.SetupAdmin(t, "admin@test.com", "password123")
	project := env.CreateProject(t, token, "Test Project", "test-project")
	projectID := extractID(project["id"])
	app := env.CreateApplication(t, token, projectID, map[string]any{
		"name": "Domain App", "type": "git", "build_type": "dockerfile", "source_repo": "https://github.com/test/repo",
	})
	appID := extractID(app["id"])

	resp := env.DoRequest(t, "POST", fmt.Sprintf("/api/projects/%s/applications/%s/domains", projectID, appID), map[string]any{
		"hostname": "bad.example.com",
		"ssl_mode": "invalid_mode",
	}, testutil.AuthHeader(token))
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
	resp.Body.Close()
}

func TestListDomains(t *testing.T) {
	resetDB(t)
	token := env.SetupAdmin(t, "admin@test.com", "password123")
	project := env.CreateProject(t, token, "Test Project", "test-project")
	projectID := extractID(project["id"])
	app := env.CreateApplication(t, token, projectID, map[string]any{
		"name": "Domain App", "type": "git", "build_type": "dockerfile", "source_repo": "https://github.com/test/repo",
	})
	appID := extractID(app["id"])

	// Add two domains
	env.DoRequest(t, "POST", fmt.Sprintf("/api/projects/%s/applications/%s/domains", projectID, appID), map[string]any{
		"hostname": "one.example.com",
	}, testutil.AuthHeader(token)).Body.Close()
	env.DoRequest(t, "POST", fmt.Sprintf("/api/projects/%s/applications/%s/domains", projectID, appID), map[string]any{
		"hostname": "two.example.com",
	}, testutil.AuthHeader(token)).Body.Close()

	resp := env.DoRequest(t, "GET", fmt.Sprintf("/api/projects/%s/applications/%s/domains", projectID, appID), nil, testutil.AuthHeader(token))
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	domains := testutil.ReadJSONArray(t, resp)
	assert.Len(t, domains, 2)
}

// A domain's port override is range-checked like the application's. Before,
// 99999 was persisted and surfaced as a 502 at request time instead of a 400.
func TestDomainContainerPort_RangeAndZero(t *testing.T) {
	resetDB(t)
	token := env.SetupAdmin(t, "admin@test.com", "password123")
	project := env.CreateProject(t, token, "Test Project", "test-project")
	projectID := extractID(project["id"])
	app := env.CreateApplication(t, token, projectID, map[string]any{
		"name": "Domain App", "type": "git", "build_type": "dockerfile", "source_repo": "https://github.com/test/repo",
	})
	appID := extractID(app["id"])
	base := fmt.Sprintf("/api/projects/%s/applications/%s/domains", projectID, appID)

	for _, bad := range []int{99999, 65536, -1} {
		resp := env.DoRequest(t, "POST", base, map[string]any{
			"hostname": "bad.example.com", "container_port": bad,
		}, testutil.AuthHeader(token))
		assert.Equal(t, http.StatusBadRequest, resp.StatusCode, "create with %d", bad)
		resp.Body.Close()
	}
	var n int
	require.NoError(t, env.Pool.QueryRow(context.Background(), `SELECT count(*) FROM domains`).Scan(&n))
	assert.Zero(t, n, "a refused create persisted a domain")

	// 0 is "no override": accepted, and stored as NULL rather than a Valid 0.
	resp := env.DoRequest(t, "POST", base, map[string]any{
		"hostname": "zero.example.com", "container_port": 0,
	}, testutil.AuthHeader(token))
	require.Equal(t, http.StatusCreated, resp.StatusCode)
	domainID := extractID(testutil.ReadJSON(t, resp)["id"])
	var port *int32
	require.NoError(t, env.Pool.QueryRow(context.Background(),
		`SELECT container_port FROM domains WHERE id = $1`, domainID).Scan(&port))
	assert.Nil(t, port)

	// Update: out of range is refused and leaves the stored port alone.
	url := base + "/" + domainID
	resp = env.DoRequest(t, "PUT", url, map[string]any{
		"hostname": "zero.example.com", "container_port": 3001,
	}, testutil.AuthHeader(token))
	require.Equal(t, http.StatusOK, resp.StatusCode)
	resp.Body.Close()
	resp = env.DoRequest(t, "PUT", url, map[string]any{
		"hostname": "zero.example.com", "container_port": 70000,
	}, testutil.AuthHeader(token))
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
	resp.Body.Close()
	require.NoError(t, env.Pool.QueryRow(context.Background(),
		`SELECT container_port FROM domains WHERE id = $1`, domainID).Scan(&port))
	require.NotNil(t, port)
	assert.EqualValues(t, 3001, *port)
}

// An omitting PUT must carry the stored port override forward. UpdateDomain
// writes container_port unconditionally, so before the fallback a request that
// only meant to change the TLS mode cleared the override and the route silently
// started pointing at the application's own port. The same wipe ssl_mode had.
func TestUpdateDomain_OmittedContainerPortIsPreserved(t *testing.T) {
	resetDB(t)
	token := env.SetupAdmin(t, "admin@test.com", "password123")
	project := env.CreateProject(t, token, "Test Project", "test-project")
	projectID := extractID(project["id"])
	app := env.CreateApplication(t, token, projectID, map[string]any{
		"name": "Domain App", "type": "git", "build_type": "dockerfile", "source_repo": "https://github.com/test/repo",
	})
	appID := extractID(app["id"])
	base := fmt.Sprintf("/api/projects/%s/applications/%s/domains", projectID, appID)

	resp := env.DoRequest(t, "POST", base, map[string]any{
		"hostname": "port.example.com", "container_port": 3000,
	}, testutil.AuthHeader(token))
	require.Equal(t, http.StatusCreated, resp.StatusCode)
	domainID := extractID(testutil.ReadJSON(t, resp)["id"])
	url := base + "/" + domainID

	storedPort := func(t *testing.T) *int32 {
		t.Helper()
		var port *int32
		require.NoError(t, env.Pool.QueryRow(context.Background(),
			`SELECT container_port FROM domains WHERE id = $1`, domainID).Scan(&port))
		return port
	}

	// A PUT about something else entirely, with container_port absent.
	resp = env.DoRequest(t, "PUT", url, map[string]any{
		"hostname": "port.example.com", "ssl_mode": "off",
	}, testutil.AuthHeader(token))
	require.Equal(t, http.StatusOK, resp.StatusCode)
	resp.Body.Close()
	port := storedPort(t)
	require.NotNil(t, port, "an omitting PUT cleared the port override")
	assert.EqualValues(t, 3000, *port)

	// The response body must agree with the database; a stale body here is how
	// the dashboard would re-send the wipe on the next save.
	resp = env.DoRequest(t, "GET", url, nil, testutil.AuthHeader(token))
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.EqualValues(t, 3000, testutil.ReadJSON(t, resp)["container_port"])

	// ⚠️ An explicit null reads as absent and therefore KEEPS the value: it
	// decodes into a nil *int32, which no amount of handler code can tell
	// apart from a field that was never sent. Pinned rather than merely
	// tolerated, because the dashboard used to send null to clear a port and
	// would have silently stopped being able to.
	resp = env.DoRequest(t, "PUT", url, map[string]any{
		"hostname": "port.example.com", "container_port": nil,
	}, testutil.AuthHeader(token))
	require.Equal(t, http.StatusOK, resp.StatusCode)
	resp.Body.Close()
	port = storedPort(t)
	require.NotNil(t, port, "an explicit null cleared the port override")
	assert.EqualValues(t, 3000, *port)

	// An explicit 0 clears it, and is the only way to: absent and zero must
	// stay distinguishable, since zero is how a caller removes an override.
	resp = env.DoRequest(t, "PUT", url, map[string]any{
		"hostname": "port.example.com", "container_port": 0,
	}, testutil.AuthHeader(token))
	require.Equal(t, http.StatusOK, resp.StatusCode)
	resp.Body.Close()
	assert.Nil(t, storedPort(t), "an explicit 0 no longer clears the override")
}
