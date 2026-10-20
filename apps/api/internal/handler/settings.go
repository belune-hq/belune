package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/robfig/cron/v3"

	"github.com/belune-hq/belune/internal/config"
	"github.com/belune-hq/belune/internal/proxy"
	"github.com/belune-hq/belune/internal/service"
	"github.com/belune-hq/belune/internal/store/generated"
)

type settingResponse struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

// defaultInstanceName is used when the operator has not set one. It seeds both
// the GitHub App manifest name and the dashboard brand.
const defaultInstanceName = "Belune"

// instanceName returns the configured "instance_name" setting, or the default
// when unset/blank.
func (h *Handler) instanceName(ctx context.Context) string {
	s, err := h.queries.GetSetting(ctx, "instance_name")
	if err != nil || strings.TrimSpace(s.Value) == "" {
		return defaultInstanceName
	}
	return strings.TrimSpace(s.Value)
}

//apidoc:tag platform/maintenance
func (h *Handler) ListSettings(w http.ResponseWriter, r *http.Request) {
	settings, err := h.queries.ListSettings(r.Context())
	if err != nil {
		writeServerError(w, r, "failed to list settings", err)
		return
	}

	result := make([]settingResponse, 0, len(settings))
	for _, s := range settings {
		// The SMTP password is keyring-encrypted and managed via the dedicated
		// /api/settings/smtp endpoints — never expose it in the generic listing.
		if s.Key == service.SettingSMTPPassword {
			continue
		}
		result = append(result, settingResponse{
			Key:   s.Key,
			Value: s.Value,
		})
	}

	writeJSON(w, http.StatusOK, result)
}

//apidoc:tag platform/maintenance
func (h *Handler) UpdateSettings(w http.ResponseWriter, r *http.Request) {
	var req []settingResponse
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	// The dashboard settings are not just stored strings: they decide the hostname
	// Caddy obtains a certificate for, and how. Validate before writing, then
	// apply to the proxy — a bad value here takes HTTPS down on the panel itself.
	changingDashboard := false
	for i, s := range req {
		// Reject before validating, so an unknown key is never silently dropped
		// and never reaches UpsertSetting. An empty key was previously skipped
		// by the write loop, which is the same silence in a different place.
		if !updatableSettings[s.Key] {
			writeError(w, http.StatusBadRequest,
				fmt.Sprintf("unknown setting %q — accepted keys are: %s", s.Key, knownSettingKeys()))
			return
		}

		switch s.Key {
		case proxy.SettingDashboardDomain:
			host := strings.TrimSpace(s.Value)
			if host != "" && !hostnameRegex.MatchString(host) {
				writeError(w, http.StatusBadRequest, "invalid dashboard domain: must be a hostname such as belune.example.com")
				return
			}
			req[i].Value = host
			changingDashboard = true

		case proxy.SettingDashboardSSLMode:
			mode := strings.TrimSpace(s.Value)
			if !proxy.ValidSSLMode(mode) {
				writeError(w, http.StatusBadRequest, "invalid TLS mode: expected automatic, custom, or off")
				return
			}
			req[i].Value = mode
			changingDashboard = true

		case proxy.SettingDashboardCertificateID:
			id := strings.TrimSpace(s.Value)
			// A certificate that does not exist would leave the dashboard with
			// nothing to serve on :443 — refuse it here rather than discover it in
			// the reconciler, where the operator would never see the reason.
			if id != "" {
				var uid pgtype.UUID
				if err := uid.Scan(id); err != nil {
					writeError(w, http.StatusBadRequest, "invalid certificate id")
					return
				}
				if _, err := h.queries.GetCertificate(r.Context(), uid); err != nil {
					writeError(w, http.StatusBadRequest, "the selected certificate no longer exists")
					return
				}
			}
			req[i].Value = id
			changingDashboard = true

		case config.SettingPublicIP:
			// Blank clears the override (fall back to env/autodetect). A non-blank
			// value must parse as an IP — a garbage baseline is worse than none, as
			// it would mark every domain as pointing at "not this server".
			ip := strings.TrimSpace(s.Value)
			if ip != "" && net.ParseIP(ip) == nil {
				writeError(w, http.StatusBadRequest, "invalid server IP: must be an IPv4 or IPv6 address")
				return
			}
			req[i].Value = ip

		case config.SettingControlPlaneBackupSchedule:
			// Blank falls back to config.DefaultControlPlaneBackupSchedule (see the
			// worker sweep) — only validate when the operator sets one explicitly.
			sched := strings.TrimSpace(s.Value)
			if sched != "" {
				if _, err := cron.ParseStandard(sched); err != nil {
					writeError(w, http.StatusBadRequest, "invalid cron schedule")
					return
				}
			}
			req[i].Value = sched

		case config.SettingControlPlaneBackupRetainDays:
			v, errMsg := validateRetentionSetting(s.Value, 1, 3650)
			if errMsg != "" {
				writeError(w, http.StatusBadRequest, "invalid retention days: "+errMsg)
				return
			}
			req[i].Value = v

		case config.SettingControlPlaneBackupRetainCount:
			v, errMsg := validateRetentionSetting(s.Value, 1, 1000)
			if errMsg != "" {
				writeError(w, http.StatusBadRequest, "invalid retention count: "+errMsg)
				return
			}
			req[i].Value = v

		case settingHostShellEnabled, "daily_cleanup_enabled", config.SettingControlPlaneBackupEnabled, "update_check_enabled":
			v, errMsg := validateBooleanSetting(s.Value)
			if errMsg != "" {
				writeError(w, http.StatusBadRequest, fmt.Sprintf("invalid %s: %s", s.Key, errMsg))
				return
			}
			req[i].Value = v

		case "instance_name":
			v, errMsg := validateInstanceNameSetting(s.Value)
			if errMsg != "" {
				writeError(w, http.StatusBadRequest, "invalid instance name: "+errMsg)
				return
			}
			req[i].Value = v

		// The version the operator dismissed via "skip this version" on the
		// Server-page update card. Blank clears the dismissal — same "blank
		// means unset" convention as every other knob here.
		case "update_skip_version":
			v, errMsg := validateSkipVersionSetting(s.Value)
			if errMsg != "" {
				writeError(w, http.StatusBadRequest, "invalid update_skip_version: "+errMsg)
				return
			}
			req[i].Value = v

		// Day-based retention knobs. Their readers accept the value only when it
		// parses above zero and quietly fall back to a default otherwise, so an
		// unvalidated typo here reads as "unset" everywhere downstream.
		case "app_log_retention_days", "request_log_retention_days",
			"audit_log_retention_days", "orphaned_backup_retention_days":
			v, errMsg := validateRetentionSetting(s.Value, 1, 3650)
			if errMsg != "" {
				writeError(w, http.StatusBadRequest, fmt.Sprintf("invalid %s: %s", s.Key, errMsg))
				return
			}
			req[i].Value = v

		// Hours, not days — the only knob measured that way. The ceiling is ten
		// years, generous enough that no existing value can be rejected by it.
		case "host_metrics_retention_hours":
			v, errMsg := validateRetentionSetting(s.Value, 1, 87600)
			if errMsg != "" {
				writeError(w, http.StatusBadRequest, "invalid host metrics retention: "+errMsg)
				return
			}
			req[i].Value = v
		}
	}

	// The three dashboard settings only make sense together, and a request may
	// carry any subset of them — so resolve what the combination *will* be and
	// judge that, rather than each field in isolation.
	domain, mode, certID := h.effectiveDashboardTLS(r, req)
	if changingDashboard && domain != "" && mode == proxy.SSLModeCustom && certID == "" {
		writeError(w, http.StatusBadRequest, "choose a certificate to serve, or switch the TLS mode to automatic")
		return
	}

	// No empty-key guard here any more: "" is not in updatableSettings, so the
	// allowlist above rejects it with a 400 rather than skipping it silently.
	for _, s := range req {
		if _, err := h.queries.UpsertSetting(r.Context(), generated.UpsertSettingParams{
			Key:   s.Key,
			Value: s.Value,
		}); err != nil {
			writeServerError(w, r, "failed to update setting", err)
			return
		}
	}

	if changingDashboard {
		// A full reconcile, not just the route: the mode also decides the auto-HTTPS
		// skip lists and which certificate is loaded, and those are the reconciler's
		// to write. Setting only the route would leave a window — up to a whole
		// reconcile interval — where the dashboard force-redirects to HTTPS while
		// still being skipped for certificates, which a browser shows as an error.
		//
		// Fall back to the route alone if there is no reconciler (tests), and treat
		// either failure as "saved, but not yet applied": the periodic pass fixes it.
		var err error
		if h.reconciler != nil {
			ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
			defer cancel()
			err = h.reconciler.ReconcileNow(ctx)
		} else {
			err = h.proxy.SetDashboardRoute(r.Context(), domain, mode)
		}
		if err != nil {
			slog.Error("failed to apply dashboard settings to proxy", "hostname", domain, "ssl_mode", mode, "error", err)
			writeError(w, http.StatusInternalServerError, "settings saved, but the proxy could not be updated — it will retry shortly")
			return
		}
	}

	h.audit(r, "update_settings", "settings", "", nil)

	writeJSON(w, http.StatusOK, map[string]string{"status": "updated"})
}

// updatableSettings is every key PUT /api/settings will write. Anything else is
// a 400 naming what IS accepted, never a silent upsert.
//
// The loop below used to write whatever it was handed behind an empty-key check
// alone, so a typo ("host_shel_enabled") returned 200, created a real row that
// read back correctly from ListSettings, and left the operator debugging a
// feature that never turned on. The readers compound it: HostMetricsCleanup
// takes the value only `if hours > 0` and silently falls back to 24 otherwise,
// so a malformed number is indistinguishable from an unset one at every layer.
//
// ⚠️ host_shell_enabled belongs HERE, as a member rather than an exception.
// PUT /api/settings is its only write path — hostshell.go merely reads it, and
// the dashboard's own toggle posts this exact key — so excluding it to "protect"
// the host shell would just break the toggle. Gating the host shell behind a
// dedicated endpoint is a different change; this allowlist is not it.
//
// ⚠️ The smtp_* keys are deliberately absent. They have their own endpoints so
// the password stays keyring-encrypted and masked; routing them through here
// would write a plaintext password into settings.
//
// ⚠️ audit_log_retention_days and orphaned_backup_retention_days have no UI
// control but ARE read (service/metrics.go, worker/cleanup_task.go). They are
// listed because an operator may have set them out of band, and omitting them
// would turn this fix into a regression for exactly those installs.
//
// ⚠️ Conversely, a real install can hold rows that are NOT here —
// deploy_history_retention_days and metrics_retention_days exist in installs
// created before those knobs were dropped, and nothing in the codebase reads
// either any more. That is the accumulation this allowlist exists to stop, and
// their absence is deliberate: the rows stay readable through ListSettings, but
// writing one again is a 400. A key being present in settings is not evidence
// it belongs here — check for a reader first.
var updatableSettings = map[string]bool{
	proxy.SettingDashboardDomain:                true,
	proxy.SettingDashboardSSLMode:               true,
	proxy.SettingDashboardCertificateID:         true,
	config.SettingPublicIP:                      true,
	config.SettingControlPlaneBackupEnabled:     true,
	config.SettingControlPlaneBackupSchedule:    true,
	config.SettingControlPlaneBackupRetainDays:  true,
	config.SettingControlPlaneBackupRetainCount: true,
	settingHostShellEnabled:                     true,
	"daily_cleanup_enabled":                     true,
	"instance_name":                             true,
	"app_log_retention_days":                    true,
	"request_log_retention_days":                true,
	"audit_log_retention_days":                  true,
	"orphaned_backup_retention_days":            true,
	"host_metrics_retention_hours":              true,
	// ⚠️ update_latest_* and update_last_checked_at are deliberately absent:
	// they are the update-check worker's own cache (worker/update_check_task.go)
	// and must never be writable through this endpoint — an admin PAT with
	// write scope must not be able to spoof "you are already current" by
	// overwriting what the last manifest fetch found.
	"update_check_enabled": true,
	"update_skip_version":  true,
}

// knownSettingKeys renders the allowlist for an error message, sorted so the
// response is stable rather than map-order.
func knownSettingKeys() string {
	keys := make([]string, 0, len(updatableSettings))
	for k := range updatableSettings {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return strings.Join(keys, ", ")
}

// validateBooleanSetting accepts "true", "false", or empty (clear to default).
//
// ⚠️ It does NOT normalise, because the two flags using it read OPPOSITE
// defaults from the same absent value: daily_cleanup_enabled and
// control_plane_backup_enabled are on unless the value is exactly "false",
// while host_shell_enabled is off unless it is exactly "true". Rewriting ""
// to either literal would flip one of them.
func validateBooleanSetting(raw string) (value, errMsg string) {
	trimmed := strings.TrimSpace(raw)
	switch trimmed {
	case "", "true", "false":
		return trimmed, ""
	}
	return "", `must be "true" or "false"`
}

// validateInstanceNameSetting trims and bounds the display name. Unbounded it
// reaches notification subjects and the dashboard header.
func validateInstanceNameSetting(raw string) (value, errMsg string) {
	trimmed := strings.TrimSpace(raw)
	if len(trimmed) > 200 {
		return "", "must be 200 characters or fewer"
	}
	return trimmed, ""
}

// validateSkipVersionSetting trims and bounds the dismissed-version string. It
// does not require a valid semver shape — the worker only ever writes one
// (from the manifest), and rejecting an operator's hand-typed value here would
// strand them with no way to silence a card they've already read.
func validateSkipVersionSetting(raw string) (value, errMsg string) {
	trimmed := strings.TrimSpace(raw)
	if len(trimmed) > 50 {
		return "", "must be 50 characters or fewer"
	}
	return trimmed, ""
}

// validateRetentionSetting trims and validates a retention-knob value. Blank
// falls back to the .env default (see config.SettingControlPlaneBackupRetain*),
// so only a non-blank value is range-checked.
func validateRetentionSetting(raw string, min, max int) (value, errMsg string) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "", ""
	}
	n, err := strconv.Atoi(trimmed)
	if err != nil || n < min || n > max {
		return "", fmt.Sprintf("must be a whole number between %d and %d", min, max)
	}
	return trimmed, ""
}

// effectiveDashboardTLS resolves what the dashboard's TLS settings will be once
// this request is written: the stored values, with anything in the request
// overriding them. A PATCH-shaped API that only ever sees a subset of the keys
// cannot otherwise tell whether "mode = custom" is about to be left without a
// certificate.
func (h *Handler) effectiveDashboardTLS(r *http.Request, req []settingResponse) (domain, mode, certID string) {
	read := func(key string) string {
		if s, err := h.queries.GetSetting(r.Context(), key); err == nil {
			return strings.TrimSpace(s.Value)
		}
		return ""
	}
	domain = read(proxy.SettingDashboardDomain)
	mode = read(proxy.SettingDashboardSSLMode)
	certID = read(proxy.SettingDashboardCertificateID)

	for _, s := range req {
		switch s.Key {
		case proxy.SettingDashboardDomain:
			domain = s.Value
		case proxy.SettingDashboardSSLMode:
			mode = s.Value
		case proxy.SettingDashboardCertificateID:
			certID = s.Value
		}
	}

	// Absent means automatic: that is what every install did before the mode
	// existed, so an upgrade keeps serving the certificate it already has.
	if mode == "" {
		mode = proxy.SSLModeAutomatic
	}
	return domain, mode, certID
}
