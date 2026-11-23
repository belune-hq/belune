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

func TestCAPISharingMatchesCrowdSecDefaults(t *testing.T) {
	defaults := CAPISharing{Custom: true, Tainted: true}
	for name, tc := range map[string]struct {
		raw  string
		want CAPISharing
	}{
		"missing file":    {"", defaults},
		"empty mapping":   {"{}", defaults},
		"partial file":    {"share_custom: false\nshare_context: true\n", CAPISharing{Tainted: true, Context: true}},
		"unknown key":     {"future_option: true\n", defaults},
		"explicit values": {"share_manual_decisions: true\nshare_custom: false\nshare_tainted: false\nshare_context: true\n", CAPISharing{Manual: true, Context: true}},
		"null keys":       {"share_custom: null\nshare_manual_decisions: null\n", defaults},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := parseCAPISharing([]byte(tc.raw))
			require.NoError(t, err)
			require.Equal(t, tc.want, got)
		})
	}
	for _, raw := range []string{"[broken", "share_custom: definitely-not-a-boolean", "scalar"} {
		_, err := parseCAPISharing([]byte(raw))
		require.Error(t, err)
	}
}

type capiMemoryStore struct {
	rows map[string]generated.Setting
}

func (s *capiMemoryStore) GetSetting(_ context.Context, key string) (generated.Setting, error) {
	row, ok := s.rows[key]
	if !ok {
		return generated.Setting{}, pgx.ErrNoRows
	}
	return row, nil
}
func (s *capiMemoryStore) UpsertSetting(_ context.Context, arg generated.UpsertSettingParams) (generated.Setting, error) {
	if s.rows == nil {
		s.rows = map[string]generated.Setting{}
	}
	row := generated.Setting{Key: arg.Key, Value: arg.Value}
	s.rows[arg.Key] = row
	return row, nil
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
