package crowdsec

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/belune-hq/belune/internal/store/generated"
	"github.com/jackc/pgx/v5"
)

const capiConnectionKey = "crowdsec_capi_connection"

var ErrCAPIRateLimited = errors.New("CrowdSec's Central API is rate-limiting this server's IP address. Disable Central API and wait about an hour before enabling or checking again; ongoing retries can extend the limit")

type CAPIConnectionCheck struct {
	State     string    `json:"state"`
	Reason    string    `json:"reason"`
	CheckedAt time.Time `json:"checked_at"`
}

func capiRateLimited(message string) bool {
	lower := strings.ToLower(message)
	return strings.Contains(lower, "403") || strings.Contains(lower, "api error: forbidden")
}

// Polling must never authenticate: capi status forces a new upstream login on
// every invocation. Read only local files and return the last deliberate check.
func (m *CAPIManager) localState(ctx context.Context, name string) (bool, error) {
	read := func(file string) ([]byte, error) {
		raw, err := m.exec(ctx, name, []string{"sh", "-ec", `test ! -L /etc/crowdsec/` + file + `; if [ -f /etc/crowdsec/` + file + ` ]; then test "$(wc -c < /etc/crowdsec/` + file + `)" -le 1048576; cat /etc/crowdsec/` + file + `; fi`}, "")
		return []byte(raw), err
	}
	base, err := read("config.yaml")
	if err != nil {
		return false, err
	}
	local, err := read("config.yaml.local")
	if err != nil {
		return false, err
	}
	config, err := capiMapping(base)
	if err != nil {
		return false, err
	}
	override, err := capiMapping(local)
	if err != nil {
		return false, err
	}
	mergeCAPIMaps(config, override)
	api, _ := config["api"].(map[string]any)
	server, _ := api["server"].(map[string]any)
	online, exists := server["online_client"]
	if !exists || online == nil {
		return false, nil
	}
	stanza, ok := online.(map[string]any)
	if !ok || stanza["credentials_path"] != onlineCredentialsPath {
		return true, errors.New("unsupported Central API credential configuration")
	}
	credentials, err := read("online_api_credentials.yaml")
	if err != nil {
		return false, err
	}
	if err := validateCAPICredentials(credentials); err != nil {
		return true, err
	}
	return true, nil
}

func (m *CAPIManager) connectionRecord(ctx context.Context) (*CAPIConnectionCheck, error) {
	row, err := m.store.GetSetting(ctx, capiConnectionKey)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var check CAPIConnectionCheck
	if err := json.Unmarshal([]byte(row.Value), &check); err != nil {
		return nil, errors.New("stored Central API connection check is invalid")
	}
	return &check, nil
}

// CheckConnection is an explicit operator action; automatic callers are limited
// to the single verification at the end of enabling CAPI.
func (m *CAPIManager) CheckConnection(ctx context.Context) (CAPIConnectionCheck, error) {
	if !m.checkMu.TryLock() {
		return CAPIConnectionCheck{}, ErrCAPIBusy
	}
	defer m.checkMu.Unlock()
	m.queueMu.Lock()
	defer m.queueMu.Unlock()
	operation, err := m.operation(ctx)
	if err != nil {
		return CAPIConnectionCheck{}, err
	}
	if operation.Phase == "queued" || operation.Phase == "applying" {
		return CAPIConnectionCheck{}, ErrCAPIBusy
	}
	previous, err := m.connectionRecord(ctx)
	if err != nil {
		return CAPIConnectionCheck{}, err
	}
	// Retrying a blocked IP resets CrowdSec's cooldown, even from a deliberate click.
	if previous != nil && previous.State == "rate_limited" && time.Since(previous.CheckedAt) < time.Hour {
		return *previous, nil
	}
	name, err := m.container(ctx)
	if err != nil {
		return CAPIConnectionCheck{}, err
	}
	health, healthErr := m.rt.ContainerHealth(ctx, name)
	if healthErr != nil || health != "healthy" {
		return m.unavailableConnection(ctx, "Wait for the local API to become healthy before checking Central API")
	}
	configured, err := m.localState(ctx, name)
	if err != nil || !configured {
		return m.unavailableConnection(ctx, "Enable Central API and wait for a healthy local API before checking the connection")
	}
	return m.checkConnection(ctx)
}

func (m *CAPIManager) checkConnection(ctx context.Context) (CAPIConnectionCheck, error) {
	checkCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	_, err := m.runner.Run(checkCtx, "capi", "status")
	cancel()
	check := CAPIConnectionCheck{State: "connected", Reason: "Central API authentication succeeded", CheckedAt: time.Now().UTC()}
	if err != nil {
		check.State = "failed"
		check.Reason = "Central API authentication failed. Check the server's network connection and registration settings"
		if capiRateLimited(err.Error()) {
			check.State = "rate_limited"
			check.Reason = ErrCAPIRateLimited.Error()
		}
	}
	return check, m.saveConnectionRecord(ctx, check)
}

func (m *CAPIManager) saveConnectionRecord(ctx context.Context, check CAPIConnectionCheck) error {
	raw, err := json.Marshal(check)
	if err != nil {
		return err
	}
	saveCtx, saveCancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
	defer saveCancel()
	_, err = m.store.UpsertSetting(saveCtx, generated.UpsertSettingParams{Key: capiConnectionKey, Value: string(raw)})
	return err
}

func (m *CAPIManager) unavailableConnection(ctx context.Context, reason string) (CAPIConnectionCheck, error) {
	check := CAPIConnectionCheck{State: "unavailable", Reason: reason, CheckedAt: time.Now().UTC()}
	return check, m.saveConnectionRecord(ctx, check)
}
