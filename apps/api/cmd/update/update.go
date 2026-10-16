package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"strings"
	"syscall"
	"time"
)

const (
	// imageRepo is the GHCR image path. sourceRepo is the GitHub repository the
	// version-pinned infra files come from. They hold the same string today and
	// are separate anyway, because the two move independently: GHCR packages
	// belong to the ACCOUNT, not the repository, so a rename or an org transfer
	// relocates the repo and leaves the package where it was.
	//
	// ⚠️ imageRepo can only change in a release that installs have ALREADY taken.
	// An install resolves its next version with its OWN copy of this constant, so
	// a release that moves the path is a release the version before it cannot
	// find. sourceRepo is the softer of the two — the fetch below runs
	// `curl -sSfL`, so GitHub's transfer redirect carries it.
	imageRepo         = "weiliang79/belune"
	sourceRepo        = "weiliang79/belune"
	defaultInstallDir = "/opt/belune"
	healthURL         = "http://localhost:8080/healthz"
	systemdDir        = "/etc/systemd/system"

	// The health wait polls every healthPoll and gives up once healthPoll has
	// been slept maxWaitSeconds/healthPoll times.
	maxWaitSeconds = 90
	healthPoll     = 2 * time.Second
)

// infraFile is "<local path under install dir>|<path in repo>". The local
// docker-compose.yml is the repo's infra/docker-compose.prod.yml; everything else
// keeps its path. This is exactly the set install.sh lays down, so a fresh
// install and an updated one converge on the same files.
type infraFile struct{ local, repo string }

var infraFiles = []infraFile{
	{"docker-compose.yml", "infra/docker-compose.prod.yml"},
	{"infra/caddy/Caddyfile.template", "infra/caddy/Caddyfile.template"},
	{"infra/buildkit/buildkitd.toml", "infra/buildkit/buildkitd.toml"},
	{".env.example", ".env.example"},
	{"infra/systemd/belune.service", "infra/systemd/belune.service"},
	{"infra/systemd/belune-backup.service", "infra/systemd/belune-backup.service"},
	{"scripts/backup.sh", "scripts/backup.sh"},
	{"scripts/restore.sh", "scripts/restore.sh"},
	{"scripts/update.sh", "scripts/update.sh"},
}

// execFiles are the paths that must stay executable after the swap.
var execFiles = map[string]bool{
	"scripts/backup.sh":  true,
	"scripts/restore.sh": true,
	"scripts/update.sh":  true,
}

// cmd is one subprocess. Stdin is always inherited — a child of the shell
// updater inherited it too, and Go's default would be /dev/null instead. A nil
// stdout/stderr is /dev/null (`>/dev/null`, `2>/dev/null`).
type cmd struct {
	name   string
	args   []string
	stdout io.Writer
	stderr io.Writer
}

type runner func(cmd) error

func execRunner(c cmd) error {
	p := exec.Command(c.name, c.args...)
	p.Stdin = os.Stdin
	p.Stdout = c.stdout
	p.Stderr = c.stderr
	return p.Run()
}

// exitError carries the status the process should leave with. Nothing in Run
// calls os.Exit: the deferred cleanup (the staging dir, and above all the
// revert) only runs if Run returns, so every way out of the reachable window
// has to be a return.
type exitError struct{ code int }

func (e *exitError) Error() string { return fmt.Sprintf("exit status %d", e.code) }

func exitCode(err error) int {
	var ee *exitError
	switch {
	case err == nil:
		return 0
	case errors.As(err, &ee):
		return ee.code
	default:
		return 1
	}
}

type updater struct {
	installDir string

	stdout, stderr io.Writer
	stdin          io.Reader // the no-backup prompt's input
	isTerminal     func() bool
	run            runner
	sleep          func(time.Duration)
	chdir          func(string) error
	systemdDir     string
	umask          os.FileMode

	currentImage   string
	currentVersion string
	targetVersion  string
	targetImage    string
	stageDir       string
	infraBackup    string
	revertArmed    bool
}

func newUpdater() *updater {
	dir := os.Getenv("BELUNE_DIR")
	if dir == "" { // ${BELUNE_DIR:-/opt/belune}: empty counts as unset
		dir = defaultInstallDir
	}
	// Reading the umask means setting it; put it straight back.
	um := syscall.Umask(0)
	syscall.Umask(um)
	return &updater{
		installDir: dir,
		stdout:     os.Stdout,
		stderr:     os.Stderr,
		stdin:      os.Stdin,
		isTerminal: stdinIsTerminal,
		run:        execRunner,
		sleep:      time.Sleep,
		chdir:      chdirAndPWD,
		systemdDir: systemdDir,
		umask:      os.FileMode(um),
	}
}

// chdirAndPWD is bash's `cd`: besides moving, it points $PWD at the new
// directory and exports it, so every child (docker compose derives its project
// directory from the working directory, and backup.sh is bash) sees the install
// dir as its cwd rather than whatever PWD the container started with.
func chdirAndPWD(dir string) error {
	if err := os.Chdir(dir); err != nil {
		return err
	}
	if path.IsAbs(dir) {
		return os.Setenv("PWD", path.Clean(dir))
	}
	return nil
}

// ── output ─────────────────────────────────────────────────────────────────────

func say(w io.Writer, format string, a ...any) { _, _ = fmt.Fprintf(w, format+"\n", a...) }

func (u *updater) info(format string, a ...any)    { say(u.stdout, "  [info]  "+format, a...) }
func (u *updater) success(format string, a ...any) { say(u.stdout, "  [ok]    "+format, a...) }
func (u *updater) warn(format string, a ...any)    { say(u.stdout, "  [warn]  "+format, a...) }

// die is `die`: the message on stderr, then exit 1.
func (u *updater) die(format string, a ...any) error {
	say(u.stderr, "  [err]   "+format, a...)
	return &exitError{code: 1}
}

// fail is what `set -e` did to a coreutils command that failed: its own error on
// stderr, nothing of ours, and the script ends. fsops.go words those errors as
// coreutils does, because the dashboard shows the last lines of this output as the
// reason an update failed.
func (u *updater) fail(err error) error {
	say(u.stderr, "%v", err)
	return &exitError{code: 1}
}

// childFail is `set -e` on an external command: the script leaves with that
// command's own status (127 when it could not be run at all).
func (u *updater) childFail(err error) error {
	var ee *exec.ExitError
	switch {
	case errors.As(err, &ee):
		return &exitError{code: ee.ExitCode()}
	case errors.Is(err, exec.ErrNotFound):
		say(u.stderr, "%v", err)
		return &exitError{code: 127}
	default:
		say(u.stderr, "%v", err)
		return &exitError{code: 1}
	}
}

// ── subprocesses ───────────────────────────────────────────────────────────────

// inherit runs a command with the updater's own stdout/stderr, so its output
// lands in the container log exactly where the shell version's did.
func (u *updater) inherit(name string, args ...string) error {
	return u.run(cmd{name: name, args: args, stdout: u.stdout, stderr: u.stderr})
}

// quiet is `cmd >/dev/null 2>&1`.
func (u *updater) quiet(name string, args ...string) error {
	return u.run(cmd{name: name, args: args})
}

// capture is `$(cmd 2>/dev/null)`: stdout without its trailing newlines. The
// output is returned even when the command failed, as `$(cmd || true)` kept
// whatever it had printed.
func (u *updater) capture(name string, args ...string) string {
	var out bytes.Buffer
	_ = u.run(cmd{name: name, args: args, stdout: &out})
	return strings.TrimRight(out.String(), "\n")
}

// ── the update ─────────────────────────────────────────────────────────────────

// Run is the whole update. It returns nil for success and *exitError otherwise.
//
// The contract with whatever spawns it is frozen and deliberately dumb:
// /usr/local/bin/belune-update <target-version>. Old releases' launchers are old
// code on every future update and cannot be repaired, so nothing may be added to
// that contract. The version is required: the launchers resolve "latest"
// themselves because they need it to pick the image to pull.
func (u *updater) Run(args []string) (err error) {
	installDir := u.installDir

	if !isRegularFile(installDir + "/docker-compose.yml") {
		return u.die("No docker-compose.yml found at %s. Is Belune installed?", installDir)
	}

	if err := u.chdir(installDir); err != nil {
		return u.fail(err)
	}

	say(u.stdout, "")
	say(u.stdout, "  Belune — Updater")
	say(u.stdout, "  ============================")
	say(u.stdout, "")

	// ── Resolve current and target versions ────────────────────────────────────

	u.currentImage = currentImage()
	if u.currentImage == "" {
		return u.die("No BELUNE_IMAGE in .env — cannot tell what is installed.")
	}
	// Everything after the last colon; the whole string when there is none.
	u.currentVersion = u.currentImage[strings.LastIndex(u.currentImage, ":")+1:]

	target := ""
	if len(args) > 0 {
		target = args[0]
	}
	if target == "" {
		return u.die("No target version given. Usage: belune-update <version>")
	}

	// Git tags carry a leading v, image tags do not (see install.sh). CURRENT_VERSION
	// is read back off an image reference, so it never has one — without normalising
	// here, "already on this version" could not match even when it was true, and the
	// pull below would ask for a tag that was never published.
	u.targetVersion = strings.TrimPrefix(target, "v")

	u.targetImage = "ghcr.io/" + imageRepo + ":" + u.targetVersion

	u.info("Currently installed: %s", u.currentVersion)
	u.info("Updating to:         %s", u.targetVersion)

	if u.currentVersion == u.targetVersion {
		u.success("Already on %s — nothing to do.", u.targetVersion)
		return nil
	}

	// The launcher already pulled this (it had to, to run us out of it), so this is
	// normally a no-op. Kept so a direct run of this binary stays safe; the cost is
	// one registry round trip.
	//
	// ⚠️ No "does that version exist?" here, unlike the bash original. Because the
	// launcher already pulled this image to run us, a failure at this point almost
	// never means a missing tag — it means the registry became unreachable in the
	// seconds since. Asking about the version would send the operator to check a
	// tag whose own updater is printing the question. Docker's own error is
	// inherited to stderr immediately above, so the cause is already on screen.
	u.info("Pulling %s...", u.targetImage)
	if u.inherit("docker", "pull", u.targetImage) != nil {
		return u.die("Could not pull %s.", u.targetImage)
	}

	// ── Fetch version-pinned infra files ───────────────────────────────────────

	// Git tags keep the leading v; image tags drop it. The target version was
	// normalised without one above, so re-add it for the git ref — same
	// construction install.sh uses so the two stay in lockstep.
	rawURL := "https://raw.githubusercontent.com/" + sourceRepo + "/v" + u.targetVersion

	// Download everything to a staging dir first: a failed fetch must abort before a
	// single file on disk is touched, so a transient network error can never leave a
	// half-updated infra set. Cleaned up on any exit.
	//
	// Between moving the pin and starting `docker compose up -d`, a failure would
	// leave .env and the infra files on the new version while the containers still
	// run the old image — and the next unrelated `up -d` would silently jump to it.
	// So a failure in that window puts both back (u.revertArmed, below). Deliberately
	// NOT extended past `up -d`: by then containers may be recreated and migrations
	// applied, and reverting the image over a migrated schema is worse than the
	// printed rollback.
	u.info("Fetching infra files for %s...", u.targetVersion)
	u.stageDir, err = os.MkdirTemp("", "tmp.")
	if err != nil {
		return u.fail(err)
	}
	defer func() {
		// A panic must leave the way any other failure does: through the cleanup
		// below, with a non-zero status.
		if r := recover(); r != nil {
			say(u.stderr, "  [err]   internal error: %v", r)
			err = &exitError{code: 1}
		}
		u.onExit(exitCode(err))
	}()
	for _, f := range infraFiles {
		dest := u.stageDir + "/" + f.local
		if err := mkdirAll(path.Dir(dest)); err != nil {
			return u.fail(err)
		}
		if u.inherit("curl", "-sSfL", rawURL+"/"+f.repo, "-o", dest) != nil {
			return u.die("Could not fetch %s for %s. Nothing has changed.", f.repo, u.targetVersion)
		}
	}
	u.success("Infra files fetched.")

	// ── Back up before migrating ───────────────────────────────────────────────

	// 0.x minor releases may contain breaking changes, and migrations are
	// forward-only: this backup is the rollback path for the data, while the image
	// tag below is the rollback path for the code.
	backupSh := installDir + "/scripts/backup.sh"
	if isExecutable(backupSh) || isRegularFile(backupSh) {
		u.info("Taking a pre-update backup...")
		// --local-only-ok: this backup is a local rollback point, so a failed remote
		// upload is a warning here. It is fatal for a manual or scheduled backup.
		// An older installed backup.sh would read the flag as its output directory,
		// so only pass it when the script advertises it.
		backupArgs := []string{backupSh}
		if b, rerr := os.ReadFile(backupSh); rerr != nil {
			say(u.stderr, "grep: %s: %s", backupSh, strerror(rerr))
		} else if bytes.Contains(b, []byte("--local-only-ok")) {
			backupArgs = append(backupArgs, "--local-only-ok")
		}
		if u.inherit("bash", backupArgs...) == nil {
			u.success("Backup complete.")
		} else if err := u.askContinueWithoutBackup(
			"The pre-update backup failed (see the output above for the reason).",
			"Continue updating without a backup?"); err != nil {
			return err
		}
	} else if err := u.askContinueWithoutBackup(
		fmt.Sprintf("scripts/backup.sh was not found in %s, so no pre-update backup could be taken.", installDir),
		"Continue without a backup?"); err != nil {
		return err
	}

	// ── Move the pin ───────────────────────────────────────────────────────────

	if err := copyFile(".env", ".env.backup-"+u.currentVersion); err != nil {
		return u.fail(err)
	}
	u.revertArmed = true
	if err := u.movePin(); err != nil {
		return u.fail(err)
	}
	u.success("Pinned to %s.", u.targetVersion)

	// Swap in the new infra files, keeping a per-version copy of the old ones so the
	// rollback can restore the exact set that ran with the previous image. The backup
	// preserves each file's path under the install dir.
	//
	// This also replaces scripts/update.sh — the launcher — and an operator running
	// that on the host has a bash still reading it while this runs. That is why the
	// launcher is wrapped in main() with its `exit` on the same line (see it), and
	// it is the only place the swap can hurt anyone: this binary lives in the image.
	u.infraBackup = installDir + "/.infra-backup-" + u.currentVersion
	if err := mkdirAll(u.infraBackup); err != nil {
		return u.fail(err)
	}
	for _, f := range infraFiles {
		if isRegularFile(f.local) {
			if err := mkdirAll(u.infraBackup + "/" + path.Dir(f.local)); err != nil {
				return u.fail(err)
			}
			if err := copyFile(f.local, u.infraBackup+"/"+f.local); err != nil {
				return u.fail(err)
			}
		}
		if err := mkdirAll(path.Dir(f.local)); err != nil {
			return u.fail(err)
		}
		if err := copyFile(u.stageDir+"/"+f.local, f.local); err != nil {
			return u.fail(err)
		}
		if execFiles[f.local] {
			if err := addExec(f.local, u.umask); err != nil {
				return u.fail(err)
			}
		}
	}
	u.success("Infra files updated (previous set saved in %s).", u.infraBackup)

	// ── Restart (migrations run automatically on startup) ──────────────────────

	// Ensure the file-mounts directory exists and is owned by the belune container's
	// uid before the reconcile brings the (possibly newly-added) bind mount up. An
	// install from before this dir was managed won't have it; create + chown it here
	// so file mounts work after updating, matching install.sh. Idempotent.
	if err := mkdirAll(installDir + "/filemounts"); err != nil {
		return u.fail(err)
	}
	fmUID := u.capture("docker", "run", "--rm", "--entrypoint", "id", u.targetImage, "-u")
	fmGID := u.capture("docker", "run", "--rm", "--entrypoint", "id", u.targetImage, "-g")
	haveIDs := fmUID != "" && fmGID != ""
	if haveIDs {
		if err := chownIDs(installDir+"/filemounts", fmUID, fmGID); err != nil {
			return u.fail(err)
		}
	}

	// Same for the backups directory: the new compose bind-mounts it into the belune
	// container so the worker can write control-plane archives natively. An install
	// from before Shape A already has ./backups (root-owned, from belune-backup.timer
	// running backup.sh as root) — chown it so the non-root worker can write there too.
	if err := mkdirAll(installDir + "/backups"); err != nil {
		return u.fail(err)
	}
	if haveIDs {
		if err := chownIDs(installDir+"/backups", fmUID, fmGID); err != nil {
			return u.fail(err)
		}
		// ⚠️ The lock file INSIDE it too, not just the directory. The chown above is
		// not recursive, so an install whose .lock was created by a root backup (the
		// host CLI, or the pre-update backup in the root helper) keeps it root-owned
		// — and the non-root worker then cannot open it, so every dashboard backup
		// reports "already in progress" forever. Repairs installs that already have
		// one; new ones get 0666 from backup.sh. Harmless when absent.
		lock := installDir + "/backups/.lock"
		if exists(lock) {
			// `|| true` both: repairing the lock must never fail the update.
			if err := chownIDs(lock, fmUID, fmGID); err != nil {
				say(u.stderr, "%v", err)
			}
			if err := chmod(lock, 0o666); err != nil {
				say(u.stderr, "%v", err)
			}
		}
	}

	// Same for the remote-storage config file (Q1): a FILE bind mount, so `touch`
	// it (not mkdir) before the reconcile brings the newly-added mount up, then
	// chown it — an install predating this has no such file, and the container
	// needs to already own it since it can't create a new directory entry in the
	// root-owned install dir.
	remoteEnv := installDir + "/backup-remote.env"
	if err := touch(remoteEnv); err != nil {
		return u.fail(err)
	}
	if err := chmod(remoteEnv, 0o600); err != nil {
		return u.fail(err)
	}
	if haveIDs {
		if err := chownIDs(remoteEnv, fmUID, fmGID); err != nil {
			return u.fail(err)
		}
	}

	// Full reconcile, not --no-deps belune: the refreshed compose may change any
	// service (a new dependency, a Caddy/Redis/BuildKit tweak), and only `up -d` over
	// the whole project applies those. Compose recreates only what actually changed,
	// so an image-only update still just replaces the belune container.
	// Past this point a failure is handled by rollbackHint, not an automatic revert.
	u.revertArmed = false
	u.info("Reconciling the stack (docker compose up -d)...")
	if u.inherit("docker", "compose", "up", "-d") != nil {
		u.rollbackHint()
		return u.die("Failed to start %s.", u.targetVersion)
	}

	// ── Re-extract helper binaries ─────────────────────────────────────────────

	if err := mkdirAll(installDir + "/bin"); err != nil {
		return u.fail(err)
	}
	u.info("Re-extracting belune-backup-upload helper...")
	if u.extractBackupUpload() {
		u.success("belune-backup-upload updated.")
	} else {
		u.info("belune-backup-upload not found in image — skipping.")
	}

	// ── Wait for health ────────────────────────────────────────────────────────

	u.info("Waiting for Belune to become ready...")
	for elapsed := 0; u.quiet("curl", "-sf", healthURL) != nil; {
		u.sleep(healthPoll)
		elapsed += int(healthPoll / time.Second)
		if elapsed >= maxWaitSeconds {
			say(u.stdout, "")
			u.warn("API did not become ready after %ds.", maxWaitSeconds)
			say(u.stdout, "  Check the logs:  docker compose logs --tail=50 belune")
			u.rollbackHint()
			return &exitError{code: 1}
		}
	}

	say(u.stdout, "")
	u.success("Updated %s → %s", u.currentVersion, u.targetVersion)
	u.info("Previous .env saved as .env.backup-%s", u.currentVersion)
	u.info("Previous infra files saved in %s", u.infraBackup)

	// The systemd units are refreshed in the install dir, but the active copies live
	// in /etc/systemd/system (install.sh puts them there for a root+systemd install).
	// Only tell the operator to re-copy when they actually differ, so a Docker-only
	// install never sees an irrelevant instruction.
	if isDir(u.systemdDir) && isRegularFile(u.systemdDir+"/belune.service") {
		// Daily backups now run in-app (Server → Backups: cron + retention). An
		// install that predates that still has belune-backup.timer firing at 02:00,
		// which would race the in-app run on the same archive lockfile and double
		// the backup cadence — retire it. belune-backup.service is left alone: it is
		// still the manual/CLI DR fallback (`systemctl start belune-backup.service`).
		timer := u.systemdDir + "/belune-backup.timer"
		if isRegularFile(timer) {
			u.info("Retiring belune-backup.timer — daily backups now run in-app (Server → Backups).")
			_ = u.quiet("systemctl", "disable", "--now", "belune-backup.timer")
			if err := os.Remove(timer); err != nil && !errors.Is(err, os.ErrNotExist) {
				return u.fail(fmt.Errorf("rm: cannot remove '%s': %s", timer, strerror(err)))
			}
			if err := u.inherit("systemctl", "daemon-reload"); err != nil {
				return u.childFail(err)
			}
			u.success("belune-backup.timer disabled and removed.")
		}

		for _, unit := range []string{"belune.service", "belune-backup.service"} {
			if !u.sameInstalledUnit(unit) {
				say(u.stdout, "")
				u.warn("systemd units changed in this release. To apply them:")
				if u.installDir == defaultInstallDir {
					say(u.stdout, "      sudo cp infra/systemd/*.service /etc/systemd/system/")
				} else {
					// A plain cp would install units pointing at /opt/belune, which
					// does not exist here — the same rewrite install.sh does.
					say(u.stdout, "      sudo sed \"s|%s|%s|g\" infra/systemd/belune.service > /etc/systemd/system/belune.service", defaultInstallDir, u.installDir)
					say(u.stdout, "      sudo sed \"s|%s|%s|g\" infra/systemd/belune-backup.service > /etc/systemd/system/belune-backup.service", defaultInstallDir, u.installDir)
				}
				say(u.stdout, "      sudo systemctl daemon-reload")
				break
			}
		}
	}
	say(u.stdout, "")
	return nil
}

// sameInstalledUnit reports whether the installed copy of a systemd unit matches
// the one this release ships.
//
// ⚠️ It compares against the unit as install.sh WRITES it, not as the repo holds
// it. install.sh rewrites /opt/belune to the real install dir (install.sh:460),
// so on any install that does not live at the default path a verbatim comparison
// is false forever — the drift warning fired on every single update, and the
// `cp` it printed would have repointed WorkingDirectory and ExecStart at a
// directory that does not exist on that host.
func (u *updater) sameInstalledUnit(unit string) bool {
	want, err := os.ReadFile("infra/systemd/" + unit)
	if err != nil {
		return false
	}
	if u.installDir != defaultInstallDir {
		want = bytes.ReplaceAll(want, []byte(defaultInstallDir), []byte(u.installDir))
	}
	got, err := os.ReadFile(u.systemdDir + "/" + unit)
	if err != nil {
		return false
	}
	return bytes.Equal(want, got)
}

// currentImage is the first BELUNE_IMAGE= line of .env, everything after the
// first '='. Empty when .env is missing or has none.
func currentImage() string {
	data, err := os.ReadFile(".env")
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(data), "\n") {
		if v, ok := strings.CutPrefix(line, "BELUNE_IMAGE="); ok {
			return v
		}
	}
	return ""
}

// movePin rewrites every BELUNE_IMAGE= line of .env to the target image.
//
// The rewrite goes through .env.tmp and a rename, as the sed it replaces did —
// including that the new file is created 0666 filtered by the umask, so .env ends
// up 0644 whatever mode it had. That is not a mode anyone chose; it is kept
// because changing it is a behaviour change this port is not allowed to make.
func (u *updater) movePin() error {
	data, err := os.ReadFile(".env")
	if err != nil {
		return err
	}
	pin := "BELUNE_IMAGE=" + u.targetImage
	lines := strings.Split(string(data), "\n")
	found := false
	for i, l := range lines {
		if strings.HasPrefix(l, "BELUNE_IMAGE=") {
			lines[i] = pin
			found = true
		}
	}
	if !found {
		// Unreachable in practice (current_image already found a pin line), kept
		// because the shell version had the branch.
		f, err := os.OpenFile(".env", os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o666)
		if err != nil {
			return err
		}
		if _, err := f.WriteString(pin + "\n"); err != nil {
			_ = f.Close()
			return err
		}
		return f.Close()
	}
	f, err := os.OpenFile(".env.tmp", os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o666)
	if err != nil {
		return err
	}
	if _, err := f.WriteString(strings.Join(lines, "\n")); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(".env.tmp", ".env"); err != nil {
		return fmt.Errorf("mv: cannot move '.env.tmp' to '.env': %s", strerror(err))
	}
	return nil
}

// askContinueWithoutBackup prompts on a terminal; with no stdin (the dashboard
// runs this in a detached helper container) it aborts instead of reading EOF.
// Aborting stays the non-interactive default: silently updating with no rollback
// point is worse than stopping. cause is why there is no backup, prompt the
// question.
func (u *updater) askContinueWithoutBackup(cause, prompt string) error {
	if !u.isTerminal() {
		return u.die("%s There is no terminal to ask on, so the update was stopped and nothing has changed. To be asked whether to continue without a backup, run 'bash %s/scripts/update.sh' on the host.", cause, u.installDir)
	}
	u.warn("%s", cause)
	_, _ = fmt.Fprintf(u.stderr, "  %s [y/N] ", prompt)
	reply, err := readLine(u.stdin)
	if err != nil {
		// `read` failing (EOF on Ctrl-D) under `set -e` ends the script with no
		// message at all.
		return &exitError{code: 1}
	}
	if reply != "y" && reply != "Y" {
		return u.die("Aborted. Nothing has changed.")
	}
	return nil
}

// readLine is `read -r reply`: one byte at a time so nothing past the newline is
// consumed (the next child inherits the same stdin), with the default IFS
// stripping leading and trailing blanks. A line cut short by EOF is an error
// even though read would have filled the variable.
func readLine(r io.Reader) (string, error) {
	var sb strings.Builder
	b := make([]byte, 1)
	for {
		n, err := r.Read(b)
		if n == 1 {
			if b[0] == '\n' {
				return strings.Trim(sb.String(), " \t"), nil
			}
			sb.WriteByte(b[0])
		}
		if err != nil {
			return "", err
		}
	}
}

// extractBackupUpload copies belune-backup-upload out of the target image to the
// install dir's bin/. Reports whether that worked.
//
// The destination is opened — and so truncated — before docker runs, as the
// shell redirection did.
func (u *updater) extractBackupUpload() bool {
	dst := u.installDir + "/bin/belune-backup-upload"
	f, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o666)
	if err != nil {
		say(u.stderr, "%s: %s", dst, strerror(err))
		return false
	}
	runErr := u.run(cmd{
		name:   "docker",
		args:   []string{"run", "--rm", "--entrypoint=", u.targetImage, "cat", "/usr/local/bin/belune-backup-upload"},
		stdout: f,
	})
	closeErr := f.Close()
	if runErr != nil || closeErr != nil {
		return false
	}
	return addExec(dst, u.umask) == nil
}

func (u *updater) rollbackHint() {
	say(u.stdout, "")
	say(u.stdout, "  To roll back to %s:", u.currentVersion)
	say(u.stdout, "")
	say(u.stdout, "      cd %s", u.installDir)
	say(u.stdout, "      sed -i 's|^BELUNE_IMAGE=.*|BELUNE_IMAGE=%s|' .env", u.currentImage)
	say(u.stdout, "      cp -a %s/. .", u.infraBackup)
	say(u.stdout, "      docker compose up -d")
	say(u.stdout, "")
	say(u.stdout, "  The cp restores the previous version's compose and infra files; the")
	say(u.stdout, "  full 'up -d' reverts any service the new compose had changed.")
	say(u.stdout, "")
	say(u.stdout, "  If the new version already applied migrations, restore the pre-update")
	say(u.stdout, "  backup as well — see docs/runbooks/disaster-recovery.md.")
	say(u.stdout, "")
}

// onExit is the EXIT trap: drop the staging dir, then — only if the pin has moved
// and `up -d` has not yet run — put .env and the infra set back.
func (u *updater) onExit(rc int) {
	if u.stageDir != "" {
		_ = os.RemoveAll(u.stageDir)
	}
	if !u.revertArmed || rc == 0 {
		return
	}
	u.revertArmed = false
	say(u.stderr, "  [warn]  The update failed before the restart — restoring the previous pin and infra files.")
	if u.restore() {
		say(u.stderr, "  [warn]  Restored. Nothing has changed.")
		return
	}
	infraBackup := u.infraBackup
	if infraBackup == "" { // the failure came before the backup dir was named
		infraBackup = "the infra backup"
	}
	say(u.stderr, "  [err]   Could not restore automatically; see .env.backup-%s and %s.", u.currentVersion, infraBackup)
}

// restore puts the previous pin back and, if the infra backup was taken, the
// previous infra set.
//
// The set goes back through the real `cp -a`, not a Go copy: `cp -a dir/. .`
// also stamps dir's mode and ownership onto the install dir itself, which a
// hand-written walk would not reproduce, and this is the one path that runs
// only when something has already gone wrong.
func (u *updater) restore() bool {
	if err := copyFile(".env.backup-"+u.currentVersion, ".env"); err != nil {
		say(u.stderr, "%v", err)
		return false
	}
	if isDir(u.infraBackup) {
		if err := u.inherit("cp", "-a", u.infraBackup+"/.", "."); err != nil {
			return false
		}
	}
	return true
}
