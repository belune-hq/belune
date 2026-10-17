package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/docker/docker/pkg/stdcopy"
	"github.com/hibiken/asynq"
	"golang.org/x/mod/semver"

	"github.com/weiliang79/belune/internal/pkg/redact"
	"github.com/weiliang79/belune/internal/runtime"
	"github.com/weiliang79/belune/internal/store/generated"
	"github.com/weiliang79/belune/internal/version"
	"github.com/weiliang79/belune/internal/worker"
)

// updateImageRepo is where release images live. Hardcoded to match
// scripts/update.sh and install.sh rather than derived from the running image.
const updateImageRepo = "ghcr.io/weiliang79/belune"

// TriggerSelfUpdate applies the update the Server-page card is currently
// showing, by pulling the target image and spawning a detached helper container
// from it that runs its /usr/local/bin/belune-update against the host install
// directory. The helper survives
// this container being replaced — that IS the point: `docker compose up -d`
// inside it is what stops this process partway through handling whatever
// request comes after this one's response.
//
// Triple-gated like the host shell: admin + session (route group), step-up
// re-auth (password + a second-factor code for anyone who has one enrolled),
// and a manifest check that refuses outright when the target release needs a
// host-side action the container cannot perform on its own.
// POST /api/maintenance/update (admin, session-only)
//
//apidoc:tag platform/maintenance
func (h *Handler) TriggerSelfUpdate(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Password string `json:"password"`
		Method   string `json:"method"`
		Code     string `json:"code"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Password == "" {
		writeError(w, http.StatusBadRequest, "password required")
		return
	}
	if _, ok := h.stepUpReauth(w, r, req.Password, req.Method, req.Code); !ok {
		return
	}

	// Resolved from the update-check worker's cache (worker/update_check_task.go),
	// never re-resolved here — the operator updates to exactly the version the
	// card showed them, not whatever happens to be latest by the time they click.
	target := strings.TrimSpace(h.settingValue(r.Context(), "update_latest_version"))
	if target == "" {
		writeError(w, http.StatusBadRequest, "no update available")
		return
	}

	current := "v" + strings.TrimPrefix(version.Version, "v")
	if !semver.IsValid(current) {
		writeError(w, http.StatusBadRequest, "this build has no version to update from")
		return
	}
	if semver.Compare("v"+target, current) <= 0 {
		writeError(w, http.StatusBadRequest, "already up to date")
		return
	}

	if h.settingValue(r.Context(), "update_latest_requires_host_update") == "true" {
		writeError(w, http.StatusBadRequest,
			"this release changes host-level configuration and cannot be applied from the dashboard — run scripts/update.sh on the host instead")
		return
	}

	rt, err := h.runtimes.Local(r.Context())
	if err != nil {
		writeError(w, http.StatusBadGateway, "failed to reach the Docker host")
		return
	}

	// ⚠️ Refuse a second concurrent run. Nothing else stops one: the helper is
	// created with an empty name so Docker generates a fresh one every time, and
	// every gate above passes identically on a second click — the cached target
	// has not changed and this container has not been replaced yet.
	//
	// It is easy to reach rather than a tight race. The card keeps offering
	// "Update now" after the 202 (updateAvailable is derived from the settings
	// cache, which nothing here changes), and update.sh takes minutes — it runs
	// a full pre-update backup between reading .env and rewriting it. Two runs
	// inside that window compute the SAME backup filenames from the same
	// CURRENT_VERSION (.env.backup-<v>, .infra-backup-<v>), so the second
	// clobbers the first — and those are the files update.sh tells the operator
	// are their way back.
	//
	// Same shape and status as "a backup is already in progress" (backups.go)
	// and "a deployment is already in progress" (applications.go).
	if running, err := updateHelperRunning(r.Context(), rt); err != nil {
		writeError(w, http.StatusBadGateway, "failed to reach the Docker host")
		return
	} else if running {
		writeError(w, http.StatusConflict, "an update is already in progress")
		return
	}

	workingDir, err := h.selfWorkingDir(r.Context())
	if err != nil {
		writeServerError(w, r, err.Error(), err)
		return
	}

	// ⚠️ The helper runs the TARGET image's updater, so that image has to be
	// local before the container can even be created — and pulling it can take
	// minutes. This route sits inside the 15 s withTimeout group
	// (routes.go), which cancels r.Context(), so the pull CANNOT run on the
	// request: it would be cut off at 15 s, the operator would get a 503, and no
	// update would ever start — intermittently, since a small layer delta pulls
	// in seconds and only toolchain-bumping releases or fresh hosts hit it.
	//
	// So: answer 202 now and pull + spawn in the background on a context that
	// outlives the request. A failed pull is a late failure like the helper
	// dying on its first line, and is reported the same way — through
	// GetSelfUpdateStatus — before anything on the host has been touched.
	//
	// The pull happens BEFORE the helper exists, so updateHelperRunning cannot
	// see a second click that lands during it — hence the in-process guard. It is
	// taken here and released by the background goroutine once the helper is
	// started or the pull has failed, NOT when this handler returns. One API
	// process owns this endpoint, so a flag is enough.
	if !h.updateStarting.CompareAndSwap(false, true) {
		writeError(w, http.StatusConflict, "an update is already in progress")
		return
	}

	// Same construction as scripts/update.sh: image tags carry no leading v.
	// Deliberately NOT derived from the running container's image — that would
	// honour a mirrored registry, but it is a behaviour change on the most
	// dangerous path in the product and not this change's subject.
	targetImage := updateImageRepo + ":" + strings.TrimPrefix(target, "v")

	// Start a fresh attempt record: a stale helper id or pull error from the
	// previous attempt must not be reported as this one's.
	ctx := context.WithoutCancel(r.Context())
	h.clearUpdateAttempt(ctx)
	h.setSetting(ctx, settingUpdateHelperTarget, target)
	h.setSetting(ctx, settingUpdateHelperStartedAt, time.Now().UTC().Format(time.RFC3339))

	h.updateBeganAt.Store(time.Now().UnixNano())
	h.publishUpdating(true)

	go h.pullAndSpawnUpdateHelper(ctx, rt, targetImage, workingDir, target)

	h.audit(r, "trigger_update", "platform", "", map[string]any{
		"from": version.Version,
		"to":   target,
	})
	writeJSON(w, http.StatusAccepted, map[string]string{
		"status": "started",
		"target": target,
	})
}

// updatePullTimeout bounds the background pull. Generous — a cold host pulling
// every layer over a slow link — but finite, so a wedged registry connection
// frees the in-flight guard instead of blocking updates until a restart.
const updatePullTimeout = 20 * time.Minute

// updateFailureReason builds the operator-facing line for a failed pull or
// helper start: what was being done, the (redacted, bounded) cause, and the
// reassurance that matters — the helper is what touches the host, and it never
// ran.
func updateFailureReason(what string, err error) string {
	const maxCause = 300
	cause := redact.Error(err.Error())
	if len(cause) > maxCause {
		cause = cause[:maxCause] + "…"
	}
	return fmt.Sprintf("%s: %s. Nothing has changed.", what, cause)
}

// pullAndSpawnUpdateHelper is TriggerSelfUpdate's background half: pull the
// target image, then start the helper from it. ctx must NOT derive its
// cancellation from the request. Owns releasing updateStarting.
//
// Failures are written to settingUpdatePullError for GetSelfUpdateStatus.
// Nothing on the host has been touched by any of them — the helper is what
// touches things.
func (h *Handler) pullAndSpawnUpdateHelper(ctx context.Context, rt runtime.ContainerRuntime, targetImage, workingDir, target string) {
	defer h.updateStarting.Store(false)

	// fail records WHY, not a guess at why: a pull fails for rate limiting, DNS,
	// a full disk, a registry 5xx or our own timeout as readily as for a missing
	// tag, and a fixed "does that version exist?" would send the operator to
	// check a version that is fine. Redacted because a registry error can carry
	// a host or credential-shaped text, and truncated so it stays a status line.
	fail := func(what string, err error) {
		slog.Error(what, "image", targetImage, "error", err)
		h.setSetting(ctx, settingUpdatePullError, updateFailureReason(what, err))
		// Nothing was touched, so unlatch every client now rather than leaving
		// them on "updating" until their next poll.
		h.updateStarting.Store(false)
		// Nothing else clears this: a later host-run update would otherwise
		// anchor its elapsed time on this failed attempt. The success path needs
		// no clear because it replaces this container, and the value dies with
		// the process.
		h.updateBeganAt.Store(0)
		h.publishUpdating(false)
	}

	pullCtx, cancel := context.WithTimeout(ctx, updatePullTimeout)
	defer cancel()
	if err := rt.PullImage(pullCtx, targetImage); err != nil {
		if errors.Is(pullCtx.Err(), context.DeadlineExceeded) {
			err = fmt.Errorf("timed out after %s", updatePullTimeout)
		}
		fail("Could not pull "+targetImage, err)
		return
	}

	helperID, err := rt.SpawnUpdateHelper(ctx, runtime.UpdateHelperConfig{
		TargetImage: targetImage,
		WorkingDir:  workingDir,
		Version:     target,
	})
	if err != nil {
		fail("Could not start the update helper from "+targetImage, err)
		return
	}

	h.updateProbe.markRunning(time.Now())

	// Record the helper so GetSelfUpdateStatus can report on it afterwards.
	// Without this a helper that dies on its first line is invisible: the
	// dashboard would say "started" and then show nothing at all, forever.
	//
	// Deliberately absent from updatableSettings, like the update_latest_* cache
	// it sits beside — a token must not be able to forge or erase the record of
	// an update attempt.
	h.setSetting(ctx, settingUpdateHelperID, helperID)
}

// TriggerUpdateCheck runs the manifest check on demand instead of waiting for
// the daily sweep.
//
// ⚠️ Without this the check is reachable ONLY via `@every 24h`, whose first
// activation is after a full interval — so a fresh install reports nothing
// about updates for its first day, and flipping update_check_enabled on appears
// to do nothing for just as long. Neither is distinguishable from the feature
// being broken.
//
// Honours update_check_enabled: the toggle promises no outbound request when
// off, and a manual button that ignored it would make that promise false.
//
// POST /api/maintenance/update/check (admin, session-only)
//
//apidoc:tag platform/maintenance
//apidoc:title Check For Updates
func (h *Handler) TriggerUpdateCheck(w http.ResponseWriter, r *http.Request) {
	if h.settingValue(r.Context(), settingUpdateCheckEnabled) == "false" {
		writeError(w, http.StatusBadRequest,
			"update checks are turned off — enable them first")
		return
	}
	if _, err := h.asynq.Enqueue(asynq.NewTask(worker.TypeUpdateCheck, nil), asynq.Queue("low")); err != nil {
		writeServerError(w, r, "failed to queue the update check", err)
		return
	}
	// 202, not 200: the worker fetches the manifest after this returns, so the
	// cached values the caller reads back are the PREVIOUS check's until it
	// lands. The UI polls update_last_checked_at to know when that happened.
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "queued"})
}

// settingUpdateCheckEnabled mirrors the worker's constant of the same name.
// Duplicated rather than exported across the package boundary: the worker owns
// the key's meaning, and a handler importing worker constants for settings keys
// would invert that ownership.
const settingUpdateCheckEnabled = "update_check_enabled"

// The last update attempt, written by TriggerSelfUpdate and read by
// GetSelfUpdateStatus. Cache keys owned by this package, never writable through
// PUT /api/settings — see updatableSettings.
const (
	settingUpdateHelperID        = "update_helper_id"
	settingUpdateHelperTarget    = "update_helper_target"
	settingUpdateHelperStartedAt = "update_helper_started_at"
	// Why the background pull or helper start failed, when it did. Set only
	// before any helper exists, so it and settingUpdateHelperID are never both
	// present.
	settingUpdatePullError = "update_pull_error"
)

// GetSelfUpdateStatus reports what became of the last update this dashboard
// started, so a helper that failed is visible somewhere other than
// `docker logs` on a container the operator does not know exists.
//
// ⚠️ A SUCCESSFUL update also leaves an exited helper behind, so "exited" alone
// cannot mean failure. The version decides: once this build reports the version
// the attempt targeted, the update landed — and since a successful update
// replaces this container, the process answering here IS the new build. Only an
// exited helper with the version still unchanged is a failure.
//
// GET /api/maintenance/update/status (admin, session-only)
//
//apidoc:tag platform/maintenance
//apidoc:title Get Update Status
func (h *Handler) GetSelfUpdateStatus(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	resp := map[string]any{"state": "idle"}

	helperID := h.settingValue(ctx, settingUpdateHelperID)
	if helperID == "" {
		// No helper yet: either the background pull is still going, it failed, or
		// there has been no attempt at all. Only the first two carry a target.
		pullErr := h.settingValue(ctx, settingUpdatePullError)
		if pullErr == "" && !h.updateStarting.Load() {
			// Idle — or the API restarted mid-pull, which left nothing behind.
			writeJSON(w, http.StatusOK, resp)
			return
		}
		resp["target"] = h.settingValue(ctx, settingUpdateHelperTarget)
		if startedAt := h.settingValue(ctx, settingUpdateHelperStartedAt); startedAt != "" {
			resp["started_at"] = startedAt
		}
		if pullErr != "" {
			resp["state"] = "failed"
			resp["reason"] = pullErr
		} else {
			resp["state"] = "running"
		}
		writeJSON(w, http.StatusOK, resp)
		return
	}
	target := h.settingValue(ctx, settingUpdateHelperTarget)
	resp["target"] = target
	if startedAt := h.settingValue(ctx, settingUpdateHelperStartedAt); startedAt != "" {
		resp["started_at"] = startedAt
	}

	// Landed: this build is at or past what the attempt targeted. Clear the
	// record so a later failure cannot be confused with this success.
	current := "v" + strings.TrimPrefix(version.Version, "v")
	if target != "" && semver.IsValid(current) && semver.Compare("v"+target, current) <= 0 {
		h.clearUpdateAttempt(ctx)
		writeJSON(w, http.StatusOK, map[string]any{"state": "idle"})
		return
	}

	rt, err := h.runtimes.Local(ctx)
	if err != nil {
		writeError(w, http.StatusBadGateway, "failed to reach the Docker host")
		return
	}
	all, err := rt.ListAllContainers(ctx)
	if err != nil {
		writeError(w, http.StatusBadGateway, "failed to reach the Docker host")
		return
	}

	var helper *runtime.ContainerInfo
	for i := range all {
		if all[i].ID == helperID {
			helper = &all[i]
			break
		}
	}
	if helper == nil {
		// Reaped, pruned, or removed by hand — and the version never moved, so
		// whatever it did, it did not finish the job.
		resp["state"] = "failed"
		resp["reason"] = "The update helper is gone and this install is still on " + version.Version + "."
		writeJSON(w, http.StatusOK, resp)
		return
	}
	if helper.Status == "running" || helper.Status == "created" {
		resp["state"] = "running"
		writeJSON(w, http.StatusOK, resp)
		return
	}

	resp["state"] = "failed"
	resp["reason"] = updateHelperFailureReason(ctx, rt, helperID)
	writeJSON(w, http.StatusOK, resp)
}

// updateHelperFailureReason pulls the tail of the helper's own output, which is
// the only place the actual cause appears — a missing script, an unreadable
// .env, a release that could not be fetched. Best-effort: a reason we cannot
// read must not turn the status call itself into an error, since "it failed" is
// already the useful half.
func updateHelperFailureReason(ctx context.Context, rt runtime.ContainerRuntime, helperID string) string {
	const fallback = "The update did not complete. Check the update helper's container logs on the host."
	rc, err := rt.ContainerLogsTail(ctx, helperID, 20)
	if err != nil {
		return fallback
	}
	defer rc.Close()

	// ⚠️ Two layers of framing to undo, and getting either wrong puts garbage in
	// front of the operator rather than an explanation. Verified against a real
	// failing helper, not a stubbed string — a mocked reader has neither.
	//
	// 1. The helper is not a TTY, so the stream is stdcopy-multiplexed: an
	//    8-byte header per frame whose LENGTH bytes are frequently printable
	//    ASCII, so they survive any "strip control characters" approach and
	//    appear as stray letters. Demuxed the same way GetPlatformLogs does.
	// 2. ContainerLogsTail asks for timestamps on purpose (it is the only thing
	//    that reads uniformly across platform services), so every line carries
	//    an RFC3339Nano prefix that its callers strip. Same job here.
	var buf bytes.Buffer
	if _, err := stdcopy.StdCopy(&buf, &buf, io.LimitReader(rc, 8<<10)); err != nil && buf.Len() == 0 {
		return fallback
	}
	lines := make([]string, 0, 3)
	for l := range strings.SplitSeq(buf.String(), "\n") {
		if idx := strings.IndexByte(l, ' '); idx > 0 && idx <= 36 {
			if _, err := time.Parse(time.RFC3339Nano, l[:idx]); err == nil {
				l = l[idx+1:]
			}
		}
		if l = strings.TrimSpace(l); l != "" {
			lines = append(lines, l)
		}
	}
	if len(lines) == 0 {
		return fallback
	}
	if len(lines) > 3 {
		lines = lines[len(lines)-3:]
	}
	return strings.Join(lines, " · ")
}

func (h *Handler) clearUpdateAttempt(ctx context.Context) {
	for _, k := range []string{settingUpdateHelperID, settingUpdateHelperTarget, settingUpdateHelperStartedAt, settingUpdatePullError} {
		h.setSetting(ctx, k, "")
	}
}

// setSetting writes one settings row, logging rather than failing: every caller
// here is recording or clearing the update-attempt record, and losing that must
// not fail the request that was otherwise successful.
func (h *Handler) setSetting(ctx context.Context, key, value string) {
	if _, err := h.queries.UpsertSetting(ctx, generated.UpsertSettingParams{Key: key, Value: value}); err != nil {
		slog.Warn("update: failed to write setting", "key", key, "error", err)
	}
}

// updateHelperRunning reports whether a self-update helper is still at work.
//
// Matches on LabelUpdateHelper rather than LabelHelper: backup, restore and
// snapshot helpers all carry the latter, and none of them should block an
// update. Status mirrors isRunningHelper's own definition in the orphan sweep
// (cleanup_task.go) — "created" counts, because a helper that has been created
// but not yet started is about to run, not finished.
//
// An exited helper never blocks: the update either finished (this container was
// replaced, so nothing here is running anyway) or failed, and a failed update
// must not lock the operator out of retrying from the dashboard.
func updateHelperRunning(ctx context.Context, rt runtime.ContainerRuntime) (bool, error) {
	_, running, err := findUpdateHelper(ctx, rt)
	return running, err
}

// findUpdateHelper is updateHelperRunning's body, also returning the helper so
// the public update flag can read when it was created.
func findUpdateHelper(ctx context.Context, rt runtime.ContainerRuntime) (runtime.ContainerInfo, bool, error) {
	all, err := rt.ListAllContainers(ctx)
	if err != nil {
		return runtime.ContainerInfo{}, false, err
	}
	for _, c := range all {
		if c.Labels[runtime.LabelUpdateHelper] != "true" {
			continue
		}
		if c.Status == "running" || c.Status == "created" {
			return c, true, nil
		}
	}
	return runtime.ContainerInfo{}, false, nil
}

// settingValue reads one setting's trimmed value, or "" if it is unset or the
// read fails — every caller here treats both the same way (fall through to the
// next gate), so there is no reason to distinguish them.
func (h *Handler) settingValue(ctx context.Context, key string) string {
	s, err := h.queries.GetSetting(ctx, key)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(s.Value)
}
