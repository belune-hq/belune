package proxy

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// trustedProxiesSetting reads the stored list. A missing row is "none".
func (r *Reconciler) trustedProxiesSetting(ctx context.Context) (string, error) {
	if r.trustedSetting != nil {
		return r.trustedSetting(ctx)
	}
	s, err := r.queries.GetSetting(ctx, SettingTrustedProxies)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("reading %s: %w", SettingTrustedProxies, err)
	}
	return s.Value, nil
}

// syncTrustedProxies brings Caddy's trusted proxies in line with the setting.
//
// Nothing is written when the setting cannot be read or no longer validates. Both
// directions are wrong: reading a failure as "none" would remove a working CDN
// setting over a database blip, and acting on a half-read list could trust a
// range nobody chose. The proxy keeps what it has, and the error says why.
func (r *Reconciler) syncTrustedProxies(ctx context.Context) error {
	raw, err := r.trustedProxiesSetting(ctx)
	if err != nil {
		return err
	}
	cidrs, err := DecodeTrustedProxies(raw)
	if err != nil {
		return fmt.Errorf("the stored trusted proxies are not valid, leaving the proxy as it is: %w", err)
	}
	if err := r.proxy.SyncTrustedProxies(ctx, TrustedProxiesConfig{CIDRs: cidrs}); err != nil {
		return fmt.Errorf("syncing the trusted proxies: %w", err)
	}
	return nil
}
