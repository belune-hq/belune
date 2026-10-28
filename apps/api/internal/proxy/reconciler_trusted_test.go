package proxy_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/belune-hq/belune/internal/proxy"
)

func settingOf(v string) func(context.Context) (string, error) {
	return func(context.Context) (string, error) { return v, nil }
}

func TestSyncTrustedProxies_PushesTheStoredListCanonicalised(t *testing.T) {
	mp := &mockProxy{}
	r := proxy.NewReconciler(nil, mp, nil, time.Second)

	require.NoError(t, r.SyncTrustedProxies(context.Background(), settingOf(`["192.168.0.0/16","10.0.0.0/8"]`)))
	require.Len(t, mp.trusted, 1)
	assert.Equal(t, []string{"10.0.0.0/8", "192.168.0.0/16"}, mp.trusted[0].CIDRs)

	// No row, and an empty one, both mean none — pushed, so a removed setting
	// removes the config instead of leaving the last list in Caddy.
	require.NoError(t, r.SyncTrustedProxies(context.Background(), settingOf("")))
	assert.Empty(t, mp.trusted[1].CIDRs)
}

func TestSyncTrustedProxies_ATroubledReadTouchesNothing(t *testing.T) {
	t.Run("the database fails", func(t *testing.T) {
		mp := &mockProxy{}
		r := proxy.NewReconciler(nil, mp, nil, time.Second)
		err := r.SyncTrustedProxies(context.Background(), func(context.Context) (string, error) {
			return "", errors.New("connection refused")
		})
		require.Error(t, err)
		assert.Empty(t, mp.trusted, "a blip must not read as 'none' and remove a working CDN setting")
	})
	t.Run("the stored value is corrupt", func(t *testing.T) {
		mp := &mockProxy{}
		r := proxy.NewReconciler(nil, mp, nil, time.Second)
		require.Error(t, r.SyncTrustedProxies(context.Background(), settingOf("not json")))
		assert.Empty(t, mp.trusted)
	})
	t.Run("the stored value no longer validates", func(t *testing.T) {
		mp := &mockProxy{}
		r := proxy.NewReconciler(nil, mp, nil, time.Second)
		require.Error(t, r.SyncTrustedProxies(context.Background(), settingOf(`["0.0.0.0/0"]`)))
		assert.Empty(t, mp.trusted, "a range nobody could now submit must not be applied")
	})
}

func TestSyncTrustedProxies_ARefusalFromCaddyIsReported(t *testing.T) {
	mp := &mockProxy{trustedErr: errors.New("HTTP 500: loading trusted proxies modules")}
	r := proxy.NewReconciler(nil, mp, nil, time.Second)
	err := r.SyncTrustedProxies(context.Background(), settingOf(`["10.0.0.0/8"]`))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "trusted proxies")
}
