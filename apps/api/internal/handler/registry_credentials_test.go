package handler_test

import (
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/belune-hq/belune/internal/service"
	"github.com/belune-hq/belune/internal/testutil"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/stretchr/testify/require"
)

func TestRegistryCredentialsCRUDAndSecretHandling(t *testing.T) {
	resetDB(t)
	token := env.SetupAdmin(t, "registry-admin@test.com", "password123")
	password := "registry-password-never-return"
	created := env.DoRequest(t, "POST", "/api/settings/registries", map[string]any{"registry_host": " GHCR.IO ", "username": "registry-user", "password": password}, testutil.AuthHeader(token))
	require.Equal(t, http.StatusCreated, created.StatusCode)
	row := testutil.ReadJSON(t, created)
	id := extractID(row["id"])
	require.Equal(t, "ghcr.io", row["registry_host"])
	require.Equal(t, true, row["password_set"])
	require.NotContains(t, fmt.Sprint(row), password)
	require.NotContains(t, row, "password_encrypted")
	require.NotContains(t, row, "password")
	var encrypted []byte
	require.NoError(t, env.Pool.QueryRow(context.Background(), `SELECT password_encrypted FROM registry_credentials WHERE id=$1`, id).Scan(&encrypted))
	require.NotContains(t, string(encrypted), password)
	var uuid pgtype.UUID
	require.NoError(t, uuid.Scan(id))
	svc := service.NewRegistryCredentialService(env.Queries, env.Config.Keyring)
	auth, err := svc.Resolve(context.Background(), "ghcr.io/team/private:latest")
	require.NoError(t, err)
	require.Equal(t, password, auth.Password)
	anonymous, err := svc.Resolve(context.Background(), "myuser/private")
	require.NoError(t, err)
	require.Nil(t, anonymous, "Docker Hub namespaces must not select GHCR credentials")
	updated := env.DoRequest(t, "PUT", "/api/settings/registries/"+id, map[string]any{"username": "new-user"}, testutil.AuthHeader(token))
	require.Equal(t, http.StatusOK, updated.StatusCode)
	_ = updated.Body.Close()
	current, err := env.Queries.GetRegistryCredential(context.Background(), uuid)
	require.NoError(t, err)
	require.Equal(t, encrypted, current.PasswordEncrypted)
	require.Equal(t, "ghcr.io", current.RegistryHost)
	rotated := env.DoRequest(t, "PUT", "/api/settings/registries/"+id, map[string]any{"password": "rotated-password"}, testutil.AuthHeader(token))
	require.Equal(t, http.StatusOK, rotated.StatusCode)
	_ = rotated.Body.Close()
	auth, err = svc.Resolve(context.Background(), "ghcr.io/team/private")
	require.NoError(t, err)
	require.Equal(t, "rotated-password", auth.Password)
	require.Equal(t, "new-user", auth.Username)
	list := env.DoRequest(t, "GET", "/api/settings/registries", nil, testutil.AuthHeader(token))
	require.Equal(t, http.StatusOK, list.StatusCode)
	rows := testutil.ReadJSONArray(t, list)
	require.Len(t, rows, 1)
	require.NotContains(t, fmt.Sprint(rows), "rotated-password")
	duplicate := env.DoRequest(t, "POST", "/api/settings/registries", map[string]any{"registry_host": "ghcr.io", "username": "u", "password": "p"}, testutil.AuthHeader(token))
	require.Equal(t, http.StatusConflict, duplicate.StatusCode)
	_ = duplicate.Body.Close()
	for _, input := range []map[string]any{
		{"registry_host": "https://ghcr.io", "username": "u", "password": "p"},
		{"registry_host": "ghcr.io/path", "username": "u", "password": "p"},
		{"registry_host": "user:pass@ghcr.io", "username": "u", "password": "p"},
		{"registry_host": "myuser", "username": "u", "password": "p"},
		{"registry_host": "docker.io", "username": "", "password": "p"},
		{"registry_host": "docker.io", "username": "u"},
	} {
		res := env.DoRequest(t, "POST", "/api/settings/registries", input, testutil.AuthHeader(token))
		require.Equal(t, http.StatusBadRequest, res.StatusCode)
		_ = res.Body.Close()
	}
	removed := env.DoRequest(t, "DELETE", "/api/settings/registries/"+id, nil, testutil.AuthHeader(token))
	require.Equal(t, http.StatusOK, removed.StatusCode)
	_ = removed.Body.Close()
	auth, err = svc.Resolve(context.Background(), "ghcr.io/team/private")
	require.NoError(t, err)
	require.Nil(t, auth)
}

func TestRegistryCredentialsAdminOnly(t *testing.T) {
	resetDB(t)
	admin := env.SetupAdmin(t, "registry-role-admin@test.com", "password123")
	_, member := createMember(t, admin, "registry-member@test.com")
	for _, method := range []string{"GET", "POST", "PUT", "DELETE"} {
		path := "/api/settings/registries"
		if method == "PUT" || method == "DELETE" {
			path += "/00000000-0000-0000-0000-000000000001"
		}
		res := env.DoRequest(t, method, path, map[string]any{}, testutil.AuthHeader(member))
		require.Equal(t, http.StatusForbidden, res.StatusCode)
		_ = res.Body.Close()
	}
}

func TestRegistryCredentialsRequireSession(t *testing.T) {
	resetDB(t)
	admin := env.SetupAdmin(t, "registry-session-admin@test.com", "password123")
	token := mintScoped(t, admin, service.AllScopes)
	for _, method := range []string{"GET", "POST", "PUT", "DELETE"} {
		path := "/api/settings/registries"
		if method == "PUT" || method == "DELETE" {
			path += "/00000000-0000-0000-0000-000000000001"
		}
		res := env.DoRequest(t, method, path, nil, testutil.AuthHeader(token))
		require.Equal(t, http.StatusForbidden, res.StatusCode)
		body := testutil.ReadJSON(t, res)
		require.Equal(t, "this action requires a session, not a personal access token", body["error"])
	}
}

func TestRegistryCredentialsAuditDoesNotContainPasswords(t *testing.T) {
	resetDB(t)
	token := env.SetupAdmin(t, "registry-audit-admin@test.com", "password123")
	url := auditedServerURL(t)
	password := "registry-audit-secret-value"
	response := postRaw(t, url+"/api/settings/registries", token, map[string]any{"registry_host": "ghcr.io", "username": "audit-user", "password": password})
	require.Equal(t, http.StatusCreated, response.StatusCode)
	_ = testutil.ReadJSON(t, response)
	var details string
	require.Eventually(t, func() bool {
		return env.Pool.QueryRow(context.Background(), `SELECT details::text FROM audit_logs WHERE action='create_registry_credential'`).Scan(&details) == nil
	}, 3*time.Second, 25*time.Millisecond)
	require.NotContains(t, details, password)
	require.Contains(t, details, "ghcr.io")
	require.Contains(t, details, `"rotated": true`)
}
