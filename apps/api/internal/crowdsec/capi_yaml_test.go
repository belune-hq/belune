package crowdsec

import (
	"context"
	"errors"
	"testing"

	"github.com/belune-hq/belune/internal/store/generated"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

func TestCAPITemporaryConfigPreservesOperatorOverrides(t *testing.T) {
	base := []byte("config_paths:\n  config_dir: /etc/crowdsec\n  data_dir: /var/lib/crowdsec/data\napi:\n  server:\n    listen_uri: 0.0.0.0:8080\n")
	local := []byte("prometheus:\n  enabled: false\napi:\n  server:\n    online_client:\n      sharing: false\n    trusted_proxies:\n      - 10.0.0.1\n")
	raw, err := registrationConfig(base, local)
	require.NoError(t, err)
	var config map[string]any
	require.NoError(t, yaml.Unmarshal(raw, &config))
	require.Equal(t, "/etc/crowdsec", config["config_paths"].(map[string]any)["config_dir"])
	server := config["api"].(map[string]any)["server"].(map[string]any)
	require.Equal(t, "0.0.0.0:8080", server["listen_uri"])
	require.Equal(t, []any{"10.0.0.1"}, server["trusted_proxies"])
	online := server["online_client"].(map[string]any)
	require.Equal(t, false, online["sharing"])
	require.Equal(t, onlineCredentialsPath, online["credentials_path"])
	enabled, err := localCAPIConfig(local, true)
	require.NoError(t, err)
	disabled, err := localCAPIConfig(enabled, false)
	require.NoError(t, err)
	require.NoError(t, yaml.Unmarshal(disabled, &config))
	require.Equal(t, false, config["prometheus"].(map[string]any)["enabled"])
	server = config["api"].(map[string]any)["server"].(map[string]any)
	require.NotContains(t, server, "online_client")
	require.Equal(t, []any{"10.0.0.1"}, server["trusted_proxies"])
}

func TestCAPICredentialsValidatedBeforeApply(t *testing.T) {
	require.NoError(t, validateCAPICredentials([]byte("login: machine\npassword: generated-value\nurl: https://api.crowdsec.net/\n")))
	for _, raw := range []string{"", "login: only", "login: ' '\npassword: x\nurl: https://api.crowdsec.net/", "login: x\npassword: y\nurl: https://other.example/", "login: x\npassword: y\nurl: http://api.crowdsec.net/", "{ broken"} {
		require.Error(t, validateCAPICredentials([]byte(raw)))
	}
}

func TestCAPISharingRequiresActualValues(t *testing.T) {
	sharing, err := parseCAPISharing([]byte(`{"sharing_options":{"manual":false,"custom":true,"tainted":true,"context":false}}`))
	require.NoError(t, err)
	require.Equal(t, CAPISharing{Custom: true, Tainted: true}, sharing)
	for _, raw := range []string{`{}`, `{"sharing_options":{"custom":true}}`, `not-json`} {
		_, err := parseCAPISharing([]byte(raw))
		require.Error(t, err)
	}
}

type capiMemoryStore struct {
	row    generated.Setting
	exists bool
}

func (s *capiMemoryStore) GetSetting(context.Context, string) (generated.Setting, error) {
	if !s.exists {
		return generated.Setting{}, pgx.ErrNoRows
	}
	return s.row, nil
}
func (s *capiMemoryStore) UpsertSetting(_ context.Context, arg generated.UpsertSettingParams) (generated.Setting, error) {
	s.row = generated.Setting{Key: arg.Key, Value: arg.Value}
	s.exists = true
	return s.row, nil
}
func TestCAPIQueueRestoresStateOnEnqueueFailure(t *testing.T) {
	store := &capiMemoryStore{}
	m := NewCAPIManager(nil, store)
	enable := true
	rejected := errors.New("queue unavailable")
	require.ErrorIs(t, m.Queue(context.Background(), CAPIChange{Enabled: &enable}, func([]byte) error { return rejected }), rejected)
	state, err := m.operation(context.Background())
	require.NoError(t, err)
	require.Empty(t, state.Phase)
	require.NoError(t, m.Queue(context.Background(), CAPIChange{Enabled: &enable}, func([]byte) error { return nil }))
	require.ErrorIs(t, m.Queue(context.Background(), CAPIChange{Enabled: &enable}, func([]byte) error { t.Fatal("busy change must not enqueue"); return nil }), ErrCAPIBusy)
}
