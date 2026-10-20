package worker_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/belune-hq/belune/internal/store/generated"
	"github.com/belune-hq/belune/internal/version"
	"github.com/belune-hq/belune/internal/worker"
)

// updateCheckKeys are every setting row HandleUpdateCheck reads or writes.
// Tests wipe them before and after so a shared testcontainers DB never leaks
// state between cases.
var updateCheckKeys = []string{
	"update_check_enabled",
	"update_skip_version",
	"update_latest_version",
	"update_latest_breaking",
	"update_latest_requires_host_update",
	"update_latest_notes_url",
	"update_last_checked_at",
}

func resetUpdateCheckSettings(t *testing.T) {
	t.Helper()
	clear := func() {
		for _, k := range updateCheckKeys {
			_, err := testPool.Exec(context.Background(), `DELETE FROM settings WHERE key = $1`, k)
			require.NoError(t, err)
		}
	}
	clear()
	t.Cleanup(clear)
}

func setUpdateCheckSetting(t *testing.T, key, value string) {
	t.Helper()
	_, err := testQueries.UpsertSetting(context.Background(), generated.UpsertSettingParams{Key: key, Value: value})
	require.NoError(t, err)
}

func getUpdateCheckSetting(t *testing.T, key string) string {
	t.Helper()
	s, err := testQueries.GetSetting(context.Background(), key)
	if err != nil {
		return ""
	}
	return s.Value
}

func newUpdateCheckHandler(manifestURL string) *worker.TaskHandler {
	return &worker.TaskHandler{
		DB:                testPool,
		Queries:           testQueries,
		UpdateManifestURL: manifestURL,
	}
}

func manifestServer(t *testing.T, body map[string]any) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		require.NoError(t, json.NewEncoder(w).Encode(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestHandleUpdateCheck_CachesManifest is the basic path the Server-page card
// depends on: every field of the matching release lands in its own setting.
func TestHandleUpdateCheck_CachesManifest(t *testing.T) {
	resetUpdateCheckSettings(t)

	srv := manifestServer(t, map[string]any{
		"latest": "9.9.9",
		"releases": []map[string]any{
			{"version": "9.9.9", "breaking": true, "requires_host_update": false, "notes_url": "https://example.com/9.9.9"},
			{"version": "9.9.8", "breaking": false, "requires_host_update": false, "notes_url": "https://example.com/9.9.8"},
		},
	})

	h := newUpdateCheckHandler(srv.URL)
	h.HandleUpdateCheck(context.Background())

	assert.Equal(t, "9.9.9", getUpdateCheckSetting(t, "update_latest_version"))
	assert.Equal(t, "true", getUpdateCheckSetting(t, "update_latest_breaking"),
		"must pick the release entry matching latest, not the first one in the list")
	assert.Equal(t, "false", getUpdateCheckSetting(t, "update_latest_requires_host_update"))
	assert.Equal(t, "https://example.com/9.9.9", getUpdateCheckSetting(t, "update_latest_notes_url"))
	assert.NotEmpty(t, getUpdateCheckSetting(t, "update_last_checked_at"))
}

// TestHandleUpdateCheck_DisabledSkipsFetch is the etiquette promise attached to
// the toggle: off means no outbound request at all, not just a silent no-op
// after the fact.
func TestHandleUpdateCheck_DisabledSkipsFetch(t *testing.T) {
	resetUpdateCheckSettings(t)
	setUpdateCheckSetting(t, "update_check_enabled", "false")

	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	h := newUpdateCheckHandler(srv.URL)
	h.HandleUpdateCheck(context.Background())

	assert.False(t, called, "a disabled check must not make the request at all")
}

// TestHandleUpdateCheck_FetchFailureLeavesCacheAlone: a belune.dev blip must
// not blank out the card — it should keep showing the last good answer until
// the next tick succeeds.
func TestHandleUpdateCheck_FetchFailureLeavesCacheAlone(t *testing.T) {
	resetUpdateCheckSettings(t)
	setUpdateCheckSetting(t, "update_latest_version", "1.2.3")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	h := newUpdateCheckHandler(srv.URL)
	h.HandleUpdateCheck(context.Background())

	assert.Equal(t, "1.2.3", getUpdateCheckSetting(t, "update_latest_version"))
}

// TestHandleUpdateCheck_ToleratesVPrefixMismatch guards the exact trap the
// build ships: version.Version is ldflags-stamped with a "v" prefix
// ("v0.1.7") while the manifest's "latest" field has none ("0.1.7"). A naive
// semver.Compare on the raw strings would treat every release as unrecognised
// (semver.IsValid requires the "v"), so this must complete cleanly rather than
// silently skip every comparison.
func TestHandleUpdateCheck_ToleratesVPrefixMismatch(t *testing.T) {
	resetUpdateCheckSettings(t)
	old := version.Version
	version.Version = "v0.1.0"
	t.Cleanup(func() { version.Version = old })

	srv := manifestServer(t, map[string]any{
		"latest":   "0.1.7",
		"releases": []map[string]any{{"version": "0.1.7", "notes_url": "https://example.com/0.1.7"}},
	})

	h := newUpdateCheckHandler(srv.URL)
	h.HandleUpdateCheck(context.Background())

	assert.Equal(t, "0.1.7", getUpdateCheckSetting(t, "update_latest_version"))
}

// TestHandleUpdateCheck_DevBuildSkipsComparison: an unstamped local build must
// not be treated as "behind" every published release.
func TestHandleUpdateCheck_DevBuildSkipsComparison(t *testing.T) {
	resetUpdateCheckSettings(t)
	old := version.Version
	version.Version = "dev"
	t.Cleanup(func() { version.Version = old })

	srv := manifestServer(t, map[string]any{
		"latest":   "9.9.9",
		"releases": []map[string]any{{"version": "9.9.9"}},
	})

	h := newUpdateCheckHandler(srv.URL)
	// Must not panic on the invalid semver, and still caches the manifest for
	// display purposes.
	h.HandleUpdateCheck(context.Background())

	assert.Equal(t, "9.9.9", getUpdateCheckSetting(t, "update_latest_version"))
}

// TestHandleUpdateCheck_UnknownLatestLeavesCacheAlone guards the fail-closed
// path for a manifest whose "latest" names a release it does not describe.
//
// ⚠️ The dangerous field is requires_host_update. A zero manifestRelease is
// all-false, and false is what PERMITS the one-click updater — so caching the
// zero value here would offer an in-app update to a release that may need a
// host-side action, with no notes link to check it against. Keeping yesterday's
// known-good values is the safe answer.
//
// Reachable in practice: hand-editing versions.json is the documented fallback
// when the release workflow's bump PR does not land, and that PR is itself
// mergeable by hand.
func TestHandleUpdateCheck_UnknownLatestLeavesCacheAlone(t *testing.T) {
	resetUpdateCheckSettings(t)
	setUpdateCheckSetting(t, "update_latest_version", "1.2.3")
	setUpdateCheckSetting(t, "update_latest_requires_host_update", "true")
	setUpdateCheckSetting(t, "update_latest_notes_url", "https://example.com/1.2.3")

	old := version.Version
	version.Version = "v1.0.0"
	t.Cleanup(func() { version.Version = old })

	// "9.9.9" is newer than the running build, so everything downstream of the
	// lookup would happily act on it — only the missing entry stops this.
	srv := manifestServer(t, map[string]any{
		"latest":   "9.9.9",
		"releases": []map[string]any{{"version": "1.2.3", "notes_url": "https://example.com/1.2.3"}},
	})

	h := newUpdateCheckHandler(srv.URL)
	h.HandleUpdateCheck(context.Background())

	assert.Equal(t, "1.2.3", getUpdateCheckSetting(t, "update_latest_version"),
		"an undescribed latest must not become the cached target")
	assert.Equal(t, "true", getUpdateCheckSetting(t, "update_latest_requires_host_update"),
		"⚠️ must not be reset to the zero value's false — that is the flag that permits the in-app update")
	assert.Equal(t, "https://example.com/1.2.3", getUpdateCheckSetting(t, "update_latest_notes_url"),
		"must not be blanked to the zero value's empty string")
}
