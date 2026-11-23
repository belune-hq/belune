package handler_test

import (
	"context"
	"fmt"
	"net/http"
	"testing"

	"github.com/belune-hq/belune/internal/service"
	"github.com/belune-hq/belune/internal/testutil"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/stretchr/testify/require"
)

func TestApplicationRegistryCredentialLifecycleAndIsolation(t *testing.T) {
	resetDB(t)
	admin := env.SetupAdmin(t, "admin@test.com", "password123")
	_, member := createMember(t, admin, "registry-owner@test.com")
	_, other := createMember(t, admin, "other@test.com")
	project := env.CreateProject(t, member, "Private", "private")
	pid := extractID(project["id"])
	app := env.CreateApplication(t, member, pid, map[string]any{"name": "Private app", "type": "image", "build_type": "image", "source_image": "ghcr.io/team/private:one"})
	id := extractID(app["id"])
	path := fmt.Sprintf("/api/projects/%s/applications/%s", pid, id)
	var uuid pgtype.UUID
	require.NoError(t, uuid.Scan(id))
	password := "app-password-never-return"
	res := env.DoRequest(t, "PUT", path, map[string]any{"registry_username": "app-user", "registry_password": password}, testutil.AuthHeader(member))
	require.Equal(t, 200, res.StatusCode)
	body := testutil.ReadJSON(t, res)
	require.Equal(t, true, body["has_registry_credentials"])
	require.Equal(t, "app-user", body["registry_username"])
	require.NotContains(t, fmt.Sprint(body), password)
	require.NotContains(t, body, "registry_password_encrypted")
	require.NotContains(t, body, "registry_password")
	stored, err := env.Queries.GetApplication(context.Background(), uuid)
	require.NoError(t, err)
	require.NotContains(t, string(stored.RegistryPasswordEncrypted), password)
	require.True(t, stored.SourceChangedAt.Valid, "changing a pull credential requires a deploy")
	original := append([]byte(nil), stored.RegistryPasswordEncrypted...)
	res = env.DoRequest(t, "PUT", path, map[string]any{"name": "Rename only"}, testutil.AuthHeader(member))
	require.Equal(t, 200, res.StatusCode)
	_ = res.Body.Close()
	stored, err = env.Queries.GetApplication(context.Background(), uuid)
	require.NoError(t, err)
	require.Equal(t, original, stored.RegistryPasswordEncrypted)
	require.Equal(t, "app-user", stored.RegistryUsername.String)
	// Global credentials remain a fallback, never an override for this application.
	res = env.DoRequest(t, "POST", "/api/settings/registries", map[string]string{"registry_host": "ghcr.io", "username": "global-user", "password": "global-password"}, testutil.AuthHeader(admin))
	require.Equal(t, 201, res.StatusCode)
	_ = res.Body.Close()
	resolver := service.NewRegistryCredentialService(env.Queries, env.Config.Keyring)
	auth, err := resolver.Resolve(context.Background(), uuid, "ghcr.io/team/private:two")
	require.NoError(t, err)
	require.Equal(t, password, auth.Password)
	require.Equal(t, "app-user", auth.Username)
	_, err = resolver.Resolve(context.Background(), uuid, "other.example/team/private")
	require.Error(t, err, "do not send an app credential to another registry during a concurrent source edit")
	res = env.DoRequest(t, "PUT", path, map[string]any{"source_image": "other.example/team/private"}, testutil.AuthHeader(member))
	require.Equal(t, 400, res.StatusCode)
	_ = res.Body.Close()
	res = env.DoRequest(t, "PUT", path, map[string]any{"registry_password": "stolen"}, testutil.AuthHeader(other))
	require.Equal(t, 403, res.StatusCode)
	_ = res.Body.Close()
	res = env.DoRequest(t, "PUT", path, map[string]any{"registry_password": "rotated"}, testutil.AuthHeader(member))
	require.Equal(t, 200, res.StatusCode)
	_ = res.Body.Close()
	auth, err = resolver.Resolve(context.Background(), uuid, "ghcr.io/team/private")
	require.NoError(t, err)
	require.Equal(t, "rotated", auth.Password)
	// Image preview children carry the same scoped credential even though the UI
	// normally creates previews for git branches.
	svc := service.NewApplicationService(env.Pool, env.Queries, nil, env.Config.Keyring, "", nil)
	stored, err = env.Queries.GetApplication(context.Background(), uuid)
	require.NoError(t, err)
	preview, created, err := svc.FindOrCreatePreview(context.Background(), stored, "private", "private-app", "preview-branch", "preview-branch")
	require.NoError(t, err)
	require.True(t, created)
	require.Equal(t, stored.RegistryUsername, preview.RegistryUsername)
	require.Equal(t, stored.RegistryPasswordEncrypted, preview.RegistryPasswordEncrypted)
	auth, err = resolver.Resolve(context.Background(), preview.ID, "ghcr.io/team/private")
	require.NoError(t, err)
	require.Equal(t, "rotated", auth.Password)
	// Remove the preview before changing the parent source (the existing source
	// switch guard protects children).
	require.NoError(t, env.Queries.DeleteApplication(context.Background(), preview.ID))
	res = env.DoRequest(t, "PUT", path, map[string]any{"clear_registry_credentials": true}, testutil.AuthHeader(member))
	require.Equal(t, 200, res.StatusCode)
	_ = res.Body.Close()
	auth, err = resolver.Resolve(context.Background(), uuid, "ghcr.io/team/private")
	require.NoError(t, err)
	require.Equal(t, "global-user", auth.Username)
	res = env.DoRequest(t, "GET", path, nil, testutil.AuthHeader(member))
	require.Equal(t, 200, res.StatusCode)
	body = testutil.ReadJSON(t, res)
	require.Equal(t, "install_wide", body["registry_credential_source"])
	res = env.DoRequest(t, "PUT", path, map[string]any{"registry_username": "app-user", "registry_password": password}, testutil.AuthHeader(member))
	require.Equal(t, 200, res.StatusCode)
	_ = res.Body.Close()
	res = env.DoRequest(t, "POST", path+"/change-source", map[string]any{"type": "git", "source_repo": "https://github.com/team/repo", "build_type": "railpack"}, testutil.AuthHeader(member))
	require.Equal(t, 200, res.StatusCode)
	_ = res.Body.Close()
	stored, err = env.Queries.GetApplication(context.Background(), uuid)
	require.NoError(t, err)
	require.Empty(t, stored.RegistryPasswordEncrypted)
	require.False(t, stored.RegistryUsername.Valid)
	var leaked int
	require.NoError(t, env.Pool.QueryRow(context.Background(), "SELECT count(*) FROM audit_logs WHERE details::text LIKE $1", "%"+password+"%").Scan(&leaked))
	require.Zero(t, leaked)
}

func TestApplicationRegistryCredentialsValidateOnCreateAndSourceSwitch(t *testing.T) {
	resetDB(t)
	token := env.SetupAdmin(t, "admin@test.com", "password123")
	project := env.CreateProject(t, token, "Sources", "sources")
	pid := extractID(project["id"])
	for _, credentials := range []map[string]any{{"registry_username": "user"}, {"registry_password": "password"}, {"registry_username": "user", "registry_password": ""}} {
		credentials["name"] = "Invalid"
		credentials["type"] = "image"
		credentials["build_type"] = "image"
		credentials["source_image"] = "nginx:latest"
		res := env.DoRequest(t, http.MethodPost, "/api/projects/"+pid+"/applications", credentials, testutil.AuthHeader(token))
		require.Equal(t, 400, res.StatusCode)
		_ = res.Body.Close()
	}
	app := env.CreateApplication(t, token, pid, map[string]any{"name": "Switch", "type": "git", "build_type": "railpack", "source_repo": "https://github.com/team/repo"})
	path := "/api/projects/" + pid + "/applications/" + extractID(app["id"])
	res := env.DoRequest(t, "POST", path+"/change-source", map[string]any{"type": "image", "source_image": "ghcr.io/team/app", "registry_username": "switch-user", "registry_password": "switch-password"}, testutil.AuthHeader(token))
	require.Equal(t, 200, res.StatusCode)
	body := testutil.ReadJSON(t, res)
	require.Equal(t, true, body["has_registry_credentials"])
	require.NotContains(t, fmt.Sprint(body), "switch-password")
}
