package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"golang.org/x/mod/semver"

	"github.com/belune-hq/belune/internal/notify"
	"github.com/belune-hq/belune/internal/store/generated"
	"github.com/belune-hq/belune/internal/version"
)

// settingUpdateCheckEnabled gates the daily manifest check. Absent or any
// value other than "false" means enabled — same default direction and reason
// as settingDailyCleanup: disclosed in the release notes as opt-out, not
// opt-in, so it isn't silently a dead feature for every existing install.
const settingUpdateCheckEnabled = "update_check_enabled"

// settingUpdateSkipVersion is the operator's "skip this version" dismissal —
// the manifest's latest version is suppressed on the Server-page card (and the
// notification that introduced it) for exactly that one value, never for every
// release after it.
const settingUpdateSkipVersion = "update_skip_version"

// settingUpdateLatest*/settingUpdateLastCheckedAt cache the most recent
// manifest fetch. They are deliberately absent from
// handler.updatableSettings — writing them is this task's job alone, never
// PUT /api/settings, so a write-scoped admin token can never spoof "you are
// already current" by overwriting the cache directly.
const (
	settingUpdateLatestVersion            = "update_latest_version"
	settingUpdateLatestBreaking           = "update_latest_breaking"
	settingUpdateLatestRequiresHostUpdate = "update_latest_requires_host_update"
	settingUpdateLatestNotesURL           = "update_latest_notes_url"
	settingUpdateLastCheckedAt            = "update_last_checked_at"
)

// defaultUpdateManifestURL is belune.dev's published manifest (Cloudflare
// Pages), not the GitHub API — 60/hr unauthenticated with no per-release
// metadata. See the update-mechanism design notes.
const defaultUpdateManifestURL = "https://belune.dev/versions.json"

const updateManifestTimeout = 15 * time.Second

var updateHTTPClient = &http.Client{Timeout: updateManifestTimeout}

// versionManifest is belune.dev/versions.json's shape. Kept minimal — only the
// fields the update checker reads; the site owns the rest.
type versionManifest struct {
	Latest   string            `json:"latest"`
	Releases []manifestRelease `json:"releases"`
}

type manifestRelease struct {
	Version            string `json:"version"`
	Breaking           bool   `json:"breaking"`
	RequiresHostUpdate bool   `json:"requires_host_update"`
	NotesURL           string `json:"notes_url"`
}

// HandleUpdateCheck fetches the manifest, caches what it found, and notifies
// admins the first time a newer version appears. Registered @every 24h on the
// low queue in worker.go; like the other periodic sweeps (HandleTLSStatusSweep,
// HandleHostMetricsCleanup) it never returns an error — a fetch failure just
// waits for tomorrow's tick rather than entering asynq's retry/backoff.
func (h *TaskHandler) HandleUpdateCheck(ctx context.Context) {
	if !h.updateCheckEnabled(ctx) {
		slog.Info("update check disabled by setting; skipping")
		return
	}

	manifest, err := h.fetchUpdateManifest(ctx)
	if err != nil {
		slog.Warn("update check: failed to fetch manifest", "error", err)
		return
	}
	if manifest.Latest == "" {
		slog.Warn("update check: manifest had no latest version")
		return
	}

	// ⚠️ Fail closed when latest names a release the manifest does not describe.
	// The zero manifestRelease is all-false, and requires_host_update=false is
	// what PERMITS the in-app updater — so caching a zero value for an unknown
	// release would offer a one-click update to a version that may well need a
	// host-side action, with no notes link to check it against. Refusing to
	// cache anything leaves yesterday's known-good values in place instead.
	var release manifestRelease
	var found bool
	for _, r := range manifest.Releases {
		if r.Version == manifest.Latest {
			release, found = r, true
			break
		}
	}
	if !found {
		slog.Warn("update check: manifest's latest has no matching release entry; leaving the cache alone",
			"latest", manifest.Latest)
		return
	}

	previous, _ := h.Queries.GetSetting(ctx, settingUpdateLatestVersion)

	h.cacheSetting(ctx, settingUpdateLatestVersion, manifest.Latest)
	h.cacheSetting(ctx, settingUpdateLatestBreaking, strconv.FormatBool(release.Breaking))
	h.cacheSetting(ctx, settingUpdateLatestRequiresHostUpdate, strconv.FormatBool(release.RequiresHostUpdate))
	h.cacheSetting(ctx, settingUpdateLatestNotesURL, release.NotesURL)
	h.cacheSetting(ctx, settingUpdateLastCheckedAt, time.Now().UTC().Format(time.RFC3339))

	current := "v" + strings.TrimPrefix(version.Version, "v")
	if !semver.IsValid(current) {
		// Unstamped ("dev") build — nothing meaningful to compare against.
		return
	}
	latest := "v" + strings.TrimPrefix(manifest.Latest, "v")
	if semver.Compare(latest, current) <= 0 {
		return
	}

	// Only a transition notifies: a version that stays newer does not re-alert
	// every day, and a version the operator already dismissed does not either.
	skip, _ := h.Queries.GetSetting(ctx, settingUpdateSkipVersion)
	if manifest.Latest == previous.Value || manifest.Latest == skip.Value {
		return
	}

	h.notifyUpdateAvailable(ctx, manifest.Latest, release)
}

func (h *TaskHandler) updateCheckEnabled(ctx context.Context) bool {
	s, err := h.Queries.GetSetting(ctx, settingUpdateCheckEnabled)
	if err != nil {
		return true
	}
	return s.Value != "false"
}

func (h *TaskHandler) cacheSetting(ctx context.Context, key, value string) {
	if _, err := h.Queries.UpsertSetting(ctx, generated.UpsertSettingParams{Key: key, Value: value}); err != nil {
		slog.Warn("update check: failed to cache setting", "key", key, "error", err)
	}
}

// fetchUpdateManifest is a plain GET, no payload — the etiquette promised to
// self-hosters in the update_check_enabled toggle's own description.
func (h *TaskHandler) fetchUpdateManifest(ctx context.Context) (versionManifest, error) {
	url := h.UpdateManifestURL
	if url == "" {
		url = defaultUpdateManifestURL
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return versionManifest{}, fmt.Errorf("build manifest request: %w", err)
	}

	resp, err := updateHTTPClient.Do(req)
	if err != nil {
		return versionManifest{}, fmt.Errorf("fetch manifest: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return versionManifest{}, fmt.Errorf("manifest returned %s", resp.Status)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return versionManifest{}, fmt.Errorf("read manifest: %w", err)
	}

	var manifest versionManifest
	if err := json.Unmarshal(body, &manifest); err != nil {
		return versionManifest{}, fmt.Errorf("parse manifest: %w", err)
	}
	return manifest, nil
}

// notifyUpdateAvailable tells admins a newer release exists, once per version.
func (h *TaskHandler) notifyUpdateAvailable(ctx context.Context, latest string, release manifestRelease) {
	if h.Notifier == nil && h.NotifyChannels == nil {
		return
	}
	title := fmt.Sprintf("Update available: v%s", latest)
	body := fmt.Sprintf("Belune v%s is available (currently running %s).", latest, version.Version)
	if release.Breaking {
		body += " This release includes breaking changes — read the notes before updating."
	}
	const link = "/server?tab=configuration"

	// Provider channels fire once per transition, not once per admin.
	h.dispatchToChannels(ctx, notify.Event{
		Type: notify.EventUpdateAvailable, Title: title, Body: body, Link: link, OccurredAt: time.Now(),
	})

	if h.Notifier == nil {
		return
	}
	admins, err := h.Queries.ListAdminUserIDs(ctx)
	if err != nil {
		slog.Warn("update check: failed to list admins for notification", "error", err)
		return
	}
	for _, id := range admins {
		h.Notifier.Notify(formatUUID(id), notify.EventUpdateAvailable, title, body, link)
	}
}
