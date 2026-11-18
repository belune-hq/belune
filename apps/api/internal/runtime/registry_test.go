package runtime

import (
	"github.com/stretchr/testify/require"
	"testing"
)

func TestRegistryHostUsesDockerReferenceGrammar(t *testing.T) {
	for _, tc := range []struct{ image, host string }{
		{"nginx", "docker.io"}, {"myuser/myimage", "docker.io"},
		{"ghcr.io/belune-hq/belune:0.3.4", "ghcr.io"},
		{"localhost:5000/app", "localhost:5000"},
		{"registry.example.com/team/app", "registry.example.com"},
		{"index.docker.io/user/app", "docker.io"},
		{"[::1]:5000/app", "[::1]:5000"},
	} {
		t.Run(tc.image, func(t *testing.T) {
			host, err := RegistryHost(tc.image)
			require.NoError(t, err)
			require.Equal(t, tc.host, host)
		})
	}
	for _, host := range []string{"", "https://ghcr.io", "ghcr.io/team", "user:password@ghcr.io", "myuser"} {
		_, err := RegistryCredentialHost(host)
		require.Error(t, err, host)
	}
	host, err := RegistryCredentialHost(" GHCR.IO ")
	require.NoError(t, err)
	require.Equal(t, "ghcr.io", host)
}
