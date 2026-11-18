package docker

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/belune-hq/belune/internal/pkg/redact"
	"github.com/belune-hq/belune/internal/runtime"
	"github.com/docker/docker/api/types/registry"
	dockerclient "github.com/docker/docker/client"
	"github.com/stretchr/testify/require"
)

func TestPullImageRegistryAuthAndDaemonErrors(t *testing.T) {
	for _, tc := range []struct {
		name       string
		auth       bool
		status     int
		body, want string
	}{
		{name: "authenticated", auth: true, status: 200, body: `{"status":"done"}`},
		{name: "anonymous", status: 200, body: `{"status":"done"}`},
		{name: "missing credential", status: 401, body: `{"message":"unauthorized"}`, want: "no credentials configured for ghcr.io"},
		{name: "wrong credential", auth: true, status: 401, body: `{"message":"unauthorized"}`, want: "authentication failed for ghcr.io"},
		{name: "stream auth error", auth: true, status: 200, body: `{"errorDetail":{"code":401,"message":"unauthorized"},"error":"unauthorized"}`, want: "authentication failed for ghcr.io"},
		{name: "stream missing credential", status: 200, body: `{"errorDetail":{"message":"denied"}}`, want: "no credentials configured for ghcr.io"},
		{name: "stream non-auth error redacted", auth: true, status: 200, body: `{"errorDetail":{"message":"backend exploded: test-password-secret"}}`, want: "backend exploded: [REDACTED]"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				require.Contains(t, r.URL.Path, "/images/create")
				if tc.auth {
					auth, err := registry.DecodeAuthConfig(r.Header.Get("X-Registry-Auth"))
					require.NoError(t, err)
					require.Equal(t, "registry-user", auth.Username)
					require.Equal(t, "test-password-secret", auth.Password)
					require.Equal(t, "ghcr.io", auth.ServerAddress)
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.status)
				_, _ = fmt.Fprint(w, tc.body)
			}))
			defer server.Close()
			cli, err := dockerclient.NewClientWithOpts(dockerclient.WithHost("tcp://"+strings.TrimPrefix(server.URL, "http://")), dockerclient.WithVersion("1.47"))
			require.NoError(t, err)
			defer func() { _ = cli.Close() }()
			c := &Client{cli: cli}
			var auth *runtime.RegistryAuth
			if tc.auth {
				auth = &runtime.RegistryAuth{Username: "registry-user", Password: "test-password-secret", ServerAddress: "ghcr.io"}
			}
			err = c.PullImage(context.Background(), "ghcr.io/team/image:latest", auth)
			if tc.want == "" {
				require.NoError(t, err)
			} else {
				require.ErrorContains(t, err, tc.want)
				require.NotContains(t, err.Error(), "test-password-secret")
				require.Equal(t, err.Error(), redact.Error(err.Error()), "deployment redaction must preserve actionable diagnostics")
			}
		})
	}
}

func TestPullImageRejectsCredentialForDifferentHost(t *testing.T) {
	c := &Client{}
	err := c.PullImage(context.Background(), "ghcr.io/team/image", &runtime.RegistryAuth{ServerAddress: "docker.io", Password: "secret"})
	require.ErrorContains(t, err, "credential host does not match")
}
