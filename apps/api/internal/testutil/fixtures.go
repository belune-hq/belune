package testutil

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/stretchr/testify/require"

	"github.com/belune-hq/belune/internal/store/generated"
)

// LocalServerID returns the id of the local server row seeded by the migrations.
// Tests that insert a project directly through the queries need it, since
// projects.server_id is NOT NULL and the id is generated rather than fixed.
func LocalServerID(t *testing.T, ctx context.Context, queries *generated.Queries) pgtype.UUID {
	t.Helper()

	server, err := queries.GetLocalServer(ctx)
	require.NoError(t, err)
	return server.ID
}

// SetupAdmin creates the first admin user via POST /api/auth/setup and returns the auth token.
func (e *TestEnv) SetupAdmin(t *testing.T, email, password string) string {
	t.Helper()

	resp := e.DoRequest(t, "POST", "/api/auth/setup", map[string]string{
		"email":    email,
		"password": password,
	}, nil)
	defer resp.Body.Close()
	require.Equal(t, http.StatusCreated, resp.StatusCode, "setup admin should return 201")

	// Now login to get a token
	return e.LoginAs(t, email, password)
}

// LoginAs logs in with the given credentials and returns the JWT token.
func (e *TestEnv) LoginAs(t *testing.T, email, password string) string {
	t.Helper()

	resp := e.DoRequest(t, "POST", "/api/auth/login", map[string]string{
		"email":    email,
		"password": password,
	}, nil)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode, "login should return 200")

	var result map[string]any
	err := json.NewDecoder(resp.Body).Decode(&result)
	require.NoError(t, err)

	token, ok := result["token"].(string)
	require.True(t, ok, "login response should contain token")
	return token
}

// CreateProject creates a project via POST /api/projects and returns the response body.
func (e *TestEnv) CreateProject(t *testing.T, token, name, slug string) map[string]any {
	t.Helper()

	resp := e.DoRequest(t, "POST", "/api/projects", map[string]string{
		"name": name,
		"slug": slug,
	}, AuthHeader(token))
	defer resp.Body.Close()
	require.Equal(t, http.StatusCreated, resp.StatusCode, "create project should return 201")

	var result map[string]any
	err := json.NewDecoder(resp.Body).Decode(&result)
	require.NoError(t, err)
	return result
}

// CreateApplication creates an application via POST /api/projects/{projectId}/applications.
func (e *TestEnv) CreateApplication(t *testing.T, token, projectID string, body map[string]any) map[string]any {
	t.Helper()

	resp := e.DoRequest(t, "POST", fmt.Sprintf("/api/projects/%s/applications", projectID), body, AuthHeader(token))
	defer resp.Body.Close()
	require.Equal(t, http.StatusCreated, resp.StatusCode, "create application should return 201")

	var result map[string]any
	err := json.NewDecoder(resp.Body).Decode(&result)
	require.NoError(t, err)
	return result
}

// DoRequest performs an HTTP request against the test server.
func (e *TestEnv) DoRequest(t *testing.T, method, path string, body any, headers map[string]string) *http.Response {
	t.Helper()

	var bodyReader io.Reader
	if body != nil {
		jsonBody, err := json.Marshal(body)
		require.NoError(t, err)
		bodyReader = bytes.NewReader(jsonBody)
	}

	req, err := http.NewRequest(method, e.Server.URL+path, bodyReader)
	require.NoError(t, err)

	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	for k, v := range headers {
		req.Header.Set(k, v)
	}

	resp, err := e.Server.Client().Do(req)
	require.NoError(t, err)
	return resp
}

// ReadJSON reads and decodes a JSON response body.
func ReadJSON(t *testing.T, resp *http.Response) map[string]any {
	t.Helper()
	defer resp.Body.Close()
	var result map[string]any
	err := json.NewDecoder(resp.Body).Decode(&result)
	require.NoError(t, err)
	return result
}

// ReadJSONArray reads and decodes a JSON array response body.
func ReadJSONArray(t *testing.T, resp *http.Response) []any {
	t.Helper()
	defer resp.Body.Close()
	var result []any
	err := json.NewDecoder(resp.Body).Decode(&result)
	require.NoError(t, err)
	return result
}

// AuthHeader returns an Authorization header map.
func AuthHeader(token string) map[string]string {
	return map[string]string{
		"Authorization": "Bearer " + token,
	}
}
