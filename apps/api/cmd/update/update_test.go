package main

import (
	"bytes"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// These tests drive Run with a fake subprocess runner and a real filesystem. They
// pin the control flow the shell updater had — what is fatal, where the revert
// window opens and closes, what is run in which order — NOT Docker or Compose
// behaviour, which only a real stack exercises. internal/scripttest runs the built
// binary against fake docker/curl on PATH; this covers the cases that need a
// scripted runner (a hung health check, a panic, a prompt).

const (
	origEnv     = "BELUNE_IMAGE=ghcr.io/weiliang79/belune:0.1.10\nJWT_SECRET=x\n"
	origCompose = "services: {old: {}}\n"
)

type call struct {
	line string // "name arg arg …"
	cwd  string
}

type harness struct {
	t       *testing.T
	u       *updater
	out     *bytes.Buffer // stdout and stderr, interleaved as an operator sees them
	install string
	tmp     string // $TMPDIR: where the staging dir lives
	calls   []call
	sleeps  []time.Duration
	// handler may take over a command; handled=false falls through to the default.
	handler func(c cmd) (handled bool, err error)
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	// Files are created through the real umask; pin it so modes are the same on
	// every machine.
	old := syscall.Umask(0o022)
	t.Cleanup(func() { syscall.Umask(old) })

	h := &harness{t: t, out: &bytes.Buffer{}, install: t.TempDir(), tmp: t.TempDir()}
	t.Setenv("TMPDIR", h.tmp)
	writeFile(t, filepath.Join(h.install, "docker-compose.yml"), origCompose, 0o644)
	writeFile(t, filepath.Join(h.install, ".env"), origEnv, 0o600)
	writeFile(t, filepath.Join(h.install, "scripts", "backup.sh"), "#!/bin/bash\n# --local-only-ok\n", 0o755)
	h.u = &updater{
		installDir: h.install,
		stdout:     h.out,
		stderr:     h.out,
		stdin:      strings.NewReader(""),
		isTerminal: func() bool { return false },
		run:        h.fakeRun,
		sleep:      func(d time.Duration) { h.sleeps = append(h.sleeps, d) },
		chdir:      func(d string) error { t.Chdir(d); return nil },
		systemdDir: filepath.Join(h.tmp, "no-systemd"),
		umask:      0o022,
	}
	return h
}

func writeFile(t *testing.T, path, content string, mode os.FileMode) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte(content), mode))
}

func (h *harness) fakeRun(c cmd) error {
	cwd, _ := os.Getwd()
	h.calls = append(h.calls, call{line: c.name + " " + strings.Join(c.args, " "), cwd: cwd})
	if h.handler != nil {
		if handled, err := h.handler(c); handled {
			return err
		}
	}
	switch c.name {
	case "cp": // the revert's `cp -a` is the one command worth running for real
		return execRunner(c)
	case "curl":
		for i, a := range c.args {
			if a == "-o" {
				return os.WriteFile(c.args[i+1], []byte("staged\n"), 0o644)
			}
		}
	case "docker":
		if len(c.args) > 1 && c.args[0] == "run" && c.stdout != nil && c.args[len(c.args)-2] == "cat" {
			_, _ = io.WriteString(c.stdout, "HELPER")
		}
	}
	return nil
}

var stageRe = regexp.MustCompile(`/tmp\.[A-Za-z0-9]+`)

// lines is the call log with machine-specific paths made stable.
func (h *harness) lines() []string {
	var out []string
	for _, c := range h.calls {
		l := strings.ReplaceAll(c.line, h.install, "<INSTALL>")
		l = strings.ReplaceAll(l, h.tmp, "<TMP>")
		out = append(out, stageRe.ReplaceAllString(l, "/<STAGE>"))
	}
	return out
}

func (h *harness) did(prefix string) bool {
	for _, l := range h.lines() {
		if strings.HasPrefix(l, prefix) {
			return true
		}
	}
	return false
}

func (h *harness) run(args ...string) int {
	h.t.Helper()
	return exitCode(h.u.Run(args))
}

func (h *harness) read(rel string) string {
	h.t.Helper()
	b, err := os.ReadFile(filepath.Join(h.install, rel))
	require.NoError(h.t, err)
	return string(b)
}

func (h *harness) assertNoStagingLeft() {
	h.t.Helper()
	entries, err := os.ReadDir(h.tmp)
	require.NoError(h.t, err)
	assert.Empty(h.t, entries, "the staging directory must be removed on every exit")
}

var fetchLines = []string{
	"curl -sSfL https://raw.githubusercontent.com/belune-hq/belune/v0.1.11/infra/docker-compose.prod.yml -o <TMP>/<STAGE>/docker-compose.yml",
	"curl -sSfL https://raw.githubusercontent.com/belune-hq/belune/v0.1.11/infra/caddy/Caddyfile.template -o <TMP>/<STAGE>/infra/caddy/Caddyfile.template",
	"curl -sSfL https://raw.githubusercontent.com/belune-hq/belune/v0.1.11/infra/buildkit/buildkitd.toml -o <TMP>/<STAGE>/infra/buildkit/buildkitd.toml",
	"curl -sSfL https://raw.githubusercontent.com/belune-hq/belune/v0.1.11/.env.example -o <TMP>/<STAGE>/.env.example",
	"curl -sSfL https://raw.githubusercontent.com/belune-hq/belune/v0.1.11/infra/systemd/belune.service -o <TMP>/<STAGE>/infra/systemd/belune.service",
	"curl -sSfL https://raw.githubusercontent.com/belune-hq/belune/v0.1.11/infra/systemd/belune-backup.service -o <TMP>/<STAGE>/infra/systemd/belune-backup.service",
	"curl -sSfL https://raw.githubusercontent.com/belune-hq/belune/v0.1.11/scripts/backup.sh -o <TMP>/<STAGE>/scripts/backup.sh",
	"curl -sSfL https://raw.githubusercontent.com/belune-hq/belune/v0.1.11/scripts/restore.sh -o <TMP>/<STAGE>/scripts/restore.sh",
	"curl -sSfL https://raw.githubusercontent.com/belune-hq/belune/v0.1.11/scripts/update.sh -o <TMP>/<STAGE>/scripts/update.sh",
}

func TestRun_StepsInOrderAndInTheInstallDir(t *testing.T) {
	h := newHarness(t)

	require.Equal(t, 0, h.run("v0.1.11"), h.out.String())

	// Every fetch before anything else touches the host, then the backup, then
	// the uid probe, then a FULL `up -d` (a refreshed compose may change any
	// service — --no-deps belune would silently skip those), then the helper
	// extraction, then the health check.
	want := append([]string{"docker pull ghcr.io/weiliang79/belune:0.1.11"}, fetchLines...)
	want = append(want,
		"bash <INSTALL>/scripts/backup.sh --local-only-ok",
		"docker run --rm --entrypoint id ghcr.io/weiliang79/belune:0.1.11 -u",
		"docker run --rm --entrypoint id ghcr.io/weiliang79/belune:0.1.11 -g",
		"docker compose up -d",
		"docker run --rm --entrypoint= ghcr.io/weiliang79/belune:0.1.11 cat /usr/local/bin/belune-backup-upload",
		"curl -sf http://localhost:8080/healthz",
	)
	assert.Equal(t, want, h.lines())

	// Compose and backup.sh read their cwd: it must be the install dir, and $PWD
	// must say so (bash's cd exported it).
	want0, err := filepath.EvalSymlinks(h.install)
	require.NoError(t, err)
	for _, c := range h.calls {
		got, err := filepath.EvalSymlinks(c.cwd)
		require.NoError(t, err)
		assert.Equal(t, want0, got, c.line)
	}

	assert.Contains(t, h.read(".env"), "BELUNE_IMAGE=ghcr.io/weiliang79/belune:0.1.11\n")
	assert.Equal(t, origEnv, h.read(".env.backup-0.1.10"))
	assert.Equal(t, origCompose, h.read(".infra-backup-0.1.10/docker-compose.yml"))
	assert.Equal(t, "staged\n", h.read("docker-compose.yml"))
	h.assertNoStagingLeft()
	assert.Contains(t, h.out.String(), "Updated 0.1.10 → 0.1.11")
}

func TestRun_VersionHandling(t *testing.T) {
	t.Run("no version", func(t *testing.T) {
		h := newHarness(t)
		assert.Equal(t, 1, h.run())
		assert.Contains(t, h.out.String(), "No target version given. Usage: belune-update <version>")
		assert.Empty(t, h.calls)
	})
	t.Run("empty version counts as none", func(t *testing.T) {
		h := newHarness(t)
		assert.Equal(t, 1, h.run(""))
		assert.Contains(t, h.out.String(), "No target version given")
	})
	t.Run("already on it, with or without the v", func(t *testing.T) {
		for _, v := range []string{"0.1.10", "v0.1.10"} {
			h := newHarness(t)
			assert.Equal(t, 0, h.run(v))
			assert.Contains(t, h.out.String(), "Already on 0.1.10 — nothing to do.")
			assert.Empty(t, h.calls, "nothing may run when there is nothing to do")
			assert.Equal(t, origEnv, h.read(".env"))
		}
	})
	t.Run("one v is stripped, not all", func(t *testing.T) {
		h := newHarness(t)
		h.handler = func(c cmd) (bool, error) { return c.name == "docker" && c.args[0] == "pull", io.EOF }
		assert.Equal(t, 1, h.run("vv0.1.11"))
		assert.Contains(t, h.out.String(), "Could not pull ghcr.io/weiliang79/belune:v0.1.11.")
		// The launcher already pulled the image to run us, so a failure here is
		// almost never a missing tag; the message must not send the operator to
		// check the version.
		assert.NotContains(t, h.out.String(), "Does that version exist?")
	})
	t.Run("current version is what follows the last colon", func(t *testing.T) {
		h := newHarness(t)
		writeFile(t, filepath.Join(h.install, ".env"), "BELUNE_IMAGE=localhost:5000/belune\n", 0o644)
		h.run("v0.1.11")
		assert.Contains(t, h.out.String(), "Currently installed: 5000/belune")
		h2 := newHarness(t)
		writeFile(t, filepath.Join(h2.install, ".env"), "BELUNE_IMAGE=belune\n", 0o644)
		h2.run("v0.1.11")
		assert.Contains(t, h2.out.String(), "Currently installed: belune")
	})
	t.Run("not installed", func(t *testing.T) {
		h := newHarness(t)
		require.NoError(t, os.Remove(filepath.Join(h.install, "docker-compose.yml")))
		assert.Equal(t, 1, h.run("v0.1.11"))
		assert.Contains(t, h.out.String(), "No docker-compose.yml found at "+h.install+". Is Belune installed?")
	})
	t.Run("no pin in .env", func(t *testing.T) {
		for _, env := range []string{"JWT_SECRET=x\n", "BELUNE_IMAGE=\n"} {
			h := newHarness(t)
			writeFile(t, filepath.Join(h.install, ".env"), env, 0o644)
			assert.Equal(t, 1, h.run("v0.1.11"))
			assert.Contains(t, h.out.String(), "No BELUNE_IMAGE in .env — cannot tell what is installed.")
		}
	})
}

func TestRun_FailedFetchAbortsBeforeAnythingOnDiskChanges(t *testing.T) {
	h := newHarness(t)
	h.handler = func(c cmd) (bool, error) {
		return c.name == "curl" && strings.Contains(strings.Join(c.args, " "), "buildkitd.toml"), io.EOF
	}

	assert.Equal(t, 1, h.run("v0.1.11"))

	assert.Contains(t, h.out.String(), "Could not fetch infra/buildkit/buildkitd.toml for 0.1.11. Nothing has changed.")
	assert.Equal(t, origEnv, h.read(".env"))
	assert.Equal(t, origCompose, h.read("docker-compose.yml"))
	assert.NoFileExists(t, filepath.Join(h.install, ".env.backup-0.1.10"))
	assert.NotContains(t, h.out.String(), "restoring", "the window has not opened, so there is nothing to revert")
	assert.False(t, h.did("bash"), "no backup before the fetch has succeeded")
	h.assertNoStagingLeft()
}

// ── the revert window ──────────────────────────────────────────────────────────

// Every way of failing between moving the pin and `docker compose up -d` must put
// the old pin and the old infra set back: otherwise the next unrelated `up -d`
// silently jumps to the new version.
func TestRun_FailureInsideTheWindowReverts(t *testing.T) {
	for name, tc := range map[string]struct {
		setup func(h *harness)
		// whether the infra backup directory exists to be restored from
		infraBackup bool
	}{
		"swap fails after compose was replaced": {func(h *harness) {
			writeFile(h.t, filepath.Join(h.install, "infra"), "not a directory\n", 0o644)
		}, true},
		"infra backup dir cannot be made": {func(h *harness) {
			writeFile(h.t, filepath.Join(h.install, ".infra-backup-0.1.10"), "a file\n", 0o644)
		}, false},
		"filemounts cannot be made": {func(h *harness) {
			writeFile(h.t, filepath.Join(h.install, "filemounts"), "a file\n", 0o644)
		}, true},
		"backups cannot be made": {func(h *harness) {
			writeFile(h.t, filepath.Join(h.install, "backups"), "a file\n", 0o644)
		}, true},
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			tc.setup(h)

			assert.Equal(t, 1, h.run("v0.1.11"), h.out.String())

			out := h.out.String()
			assert.Contains(t, out, "The update failed before the restart — restoring the previous pin and infra files.")
			assert.Contains(t, out, "Restored. Nothing has changed.")
			assert.Equal(t, origEnv, h.read(".env"), "the pin must be put back")
			assert.Equal(t, origCompose, h.read("docker-compose.yml"), "the swapped compose file must be put back")
			assert.False(t, h.did("docker compose up"), "the window ends at up -d and must not reach it")
			h.assertNoStagingLeft()
		})
	}
}

// A panic in the window is a failure like any other: it has to leave through the
// cleanup, with a non-zero status, not skip it.
func TestRun_PanicInsideTheWindowStillReverts(t *testing.T) {
	h := newHarness(t)
	h.handler = func(c cmd) (bool, error) {
		if flag, ok := idProbe(c); ok && flag == "-u" {
			panic("boom")
		}
		return false, nil
	}

	assert.Equal(t, 1, h.run("v0.1.11"))

	assert.Contains(t, h.out.String(), "internal error: boom")
	assert.Contains(t, h.out.String(), "Restored. Nothing has changed.")
	assert.Equal(t, origEnv, h.read(".env"))
	assert.Equal(t, origCompose, h.read("docker-compose.yml"))
	h.assertNoStagingLeft()
}

func TestRun_RestoreFailureSaysSo(t *testing.T) {
	h := newHarness(t)
	h.handler = func(c cmd) (bool, error) {
		if c.name == "cp" {
			return true, io.EOF // the infra restore itself fails
		}
		return false, nil
	}
	writeFile(t, filepath.Join(h.install, "infra"), "not a directory\n", 0o644)

	assert.Equal(t, 1, h.run("v0.1.11"))

	assert.Contains(t, h.out.String(), "Could not restore automatically; see .env.backup-0.1.10 and "+h.install+"/.infra-backup-0.1.10.")
	assert.NotContains(t, h.out.String(), "Restored.")
}

// Past `up -d` containers may be recreated and migrations applied, so reverting
// the image would be worse than the printed rollback. Nothing is put back.
func TestRun_FailureAfterUpDoesNotRevert(t *testing.T) {
	t.Run("up -d fails", func(t *testing.T) {
		h := newHarness(t)
		h.handler = func(c cmd) (bool, error) {
			return c.name == "docker" && len(c.args) > 1 && c.args[1] == "up", io.EOF
		}

		assert.Equal(t, 1, h.run("v0.1.11"))

		out := h.out.String()
		assert.Contains(t, out, "To roll back to 0.1.10:")
		assert.Contains(t, out, "      sed -i 's|^BELUNE_IMAGE=.*|BELUNE_IMAGE=ghcr.io/weiliang79/belune:0.1.10|' .env")
		assert.Contains(t, out, "      cp -a "+h.install+"/.infra-backup-0.1.10/. .")
		assert.Contains(t, out, "Failed to start 0.1.11.")
		assert.NotContains(t, out, "restoring")
		assert.Contains(t, h.read(".env"), "BELUNE_IMAGE=ghcr.io/weiliang79/belune:0.1.11")
		assert.Equal(t, "staged\n", h.read("docker-compose.yml"))
		assert.False(t, h.did("cp "), "no revert")
		h.assertNoStagingLeft()
	})

	t.Run("API never becomes healthy", func(t *testing.T) {
		h := newHarness(t)
		h.handler = func(c cmd) (bool, error) {
			return c.name == "curl" && len(c.args) > 1 && c.args[1] == "http://localhost:8080/healthz", io.EOF
		}

		assert.Equal(t, 1, h.run("v0.1.11"))

		// 90s wait, polled every 2s: 45 attempts, each followed by one sleep.
		health := 0
		for _, l := range h.lines() {
			if strings.HasPrefix(l, "curl -sf ") {
				health++
			}
		}
		assert.Equal(t, 45, health)
		assert.Len(t, h.sleeps, 45)
		for _, d := range h.sleeps {
			assert.Equal(t, 2*time.Second, d)
		}
		out := h.out.String()
		assert.Contains(t, out, "API did not become ready after 90s.")
		assert.Contains(t, out, "  Check the logs:  docker compose logs --tail=50 belune")
		assert.Contains(t, out, "To roll back to 0.1.10:")
		assert.NotContains(t, out, "restoring")
		assert.Contains(t, h.read(".env"), "BELUNE_IMAGE=ghcr.io/weiliang79/belune:0.1.11")
		h.assertNoStagingLeft()
	})

	t.Run("healthy on the fourth try", func(t *testing.T) {
		h := newHarness(t)
		fails := 3
		h.handler = func(c cmd) (bool, error) {
			if c.name == "curl" && len(c.args) > 1 && c.args[1] == "http://localhost:8080/healthz" && fails > 0 {
				fails--
				return true, io.EOF
			}
			return false, nil
		}
		assert.Equal(t, 0, h.run("v0.1.11"), h.out.String())
		assert.Len(t, h.sleeps, 3)
	})
}

// ── the no-backup prompt ───────────────────────────────────────────────────────

// idProbe recognises `docker run --rm --entrypoint id <image> -u|-g` and says
// which of the two it is.
func idProbe(c cmd) (flag string, ok bool) {
	n := len(c.args)
	if c.name != "docker" || n != 6 || c.args[0] != "run" || c.args[3] != "id" {
		return "", false
	}
	return c.args[5], true
}

func failingBackup(h *harness) {
	h.handler = func(c cmd) (bool, error) { return c.name == "bash", io.EOF }
}

func TestRun_NoTerminalAbortsInsteadOfReadingEOF(t *testing.T) {
	h := newHarness(t) // isTerminal is false
	failingBackup(h)

	assert.Equal(t, 1, h.run("v0.1.11"))

	out := h.out.String()
	assert.Contains(t, out, "The pre-update backup failed (see the output above for the reason). There is no terminal to ask on")
	assert.Contains(t, out, "run 'bash "+h.install+"/scripts/update.sh' on the host.")
	assert.NotContains(t, out, "Aborted.", "that is the answer-was-no message, which nobody gave")
	assert.Equal(t, origEnv, h.read(".env"), "an aborted update must not move the pin")
	assert.NoFileExists(t, filepath.Join(h.install, ".env.backup-0.1.10"))
	h.assertNoStagingLeft()
}

func TestRun_MissingBackupScriptWithNoTerminal(t *testing.T) {
	h := newHarness(t)
	require.NoError(t, os.Remove(filepath.Join(h.install, "scripts", "backup.sh")))

	assert.Equal(t, 1, h.run("v0.1.11"))

	assert.Contains(t, h.out.String(), "scripts/backup.sh was not found in "+h.install+", so no pre-update backup could be taken. There is no terminal to ask on")
	assert.False(t, h.did("bash"))
}

func TestRun_PromptOnATerminal(t *testing.T) {
	for _, tc := range []struct {
		name, typed string
		wantCode    int
		wantAbort   bool // the "Aborted." message, as opposed to a silent exit
	}{
		{"y", "y\n", 0, false},
		{"Y", "Y\n", 0, false},
		{"blanks around it are trimmed, as read does", "  y \t\n", 0, false},
		{"n", "n\n", 1, true},
		{"enter alone is no", "\n", 1, true},
		{"yes is not y", "yes\n", 1, true},
		{"CRLF is not y", "y\r\n", 1, true},
		// `read` failing under `set -e` ends the script without a word.
		{"EOF with nothing typed", "", 1, false},
		{"EOF before the newline", "y", 1, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			failingBackup(h)
			h.u.isTerminal = func() bool { return true }
			h.u.stdin = strings.NewReader(tc.typed)

			assert.Equal(t, tc.wantCode, h.run("v0.1.11"), h.out.String())

			out := h.out.String()
			assert.Contains(t, out, "  [warn]  The pre-update backup failed (see the output above for the reason).")
			assert.Contains(t, out, "  Continue updating without a backup? [y/N] ")
			assert.NotContains(t, out, "no terminal")
			assert.Equal(t, tc.wantAbort, strings.Contains(out, "Aborted. Nothing has changed."))
			if tc.wantCode == 0 {
				assert.Contains(t, out, "Updated 0.1.10 → 0.1.11")
			} else {
				assert.Equal(t, origEnv, h.read(".env"))
			}
		})
	}

	t.Run("nothing past the newline is consumed", func(t *testing.T) {
		// The next child inherits the same stdin, so read must not buffer ahead.
		h := newHarness(t)
		failingBackup(h)
		h.u.isTerminal = func() bool { return true }
		in := strings.NewReader("y\ntyped ahead")
		h.u.stdin = in

		require.Equal(t, 0, h.run("v0.1.11"), h.out.String())

		rest, _ := io.ReadAll(in)
		assert.Equal(t, "typed ahead", string(rest))
	})

	t.Run("missing script asks its own question", func(t *testing.T) {
		h := newHarness(t)
		require.NoError(t, os.Remove(filepath.Join(h.install, "scripts", "backup.sh")))
		h.u.isTerminal = func() bool { return true }
		h.u.stdin = strings.NewReader("n\n")

		assert.Equal(t, 1, h.run("v0.1.11"))
		assert.Contains(t, h.out.String(), "scripts/backup.sh was not found in "+h.install)
		assert.Contains(t, h.out.String(), "  Continue without a backup? [y/N] ")
	})
}

func TestRun_LocalOnlyFlagOnlyWhenTheInstalledScriptAdvertisesIt(t *testing.T) {
	for name, tc := range map[string]struct {
		body string
		want string
	}{
		"advertises it": {"#!/bin/bash\n# --local-only-ok\n", "bash <INSTALL>/scripts/backup.sh --local-only-ok"},
		// An older backup.sh would read the flag as its output directory.
		"legacy": {"#!/bin/bash\n", "bash <INSTALL>/scripts/backup.sh"},
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			writeFile(t, filepath.Join(h.install, "scripts", "backup.sh"), tc.body, 0o755)
			require.Equal(t, 0, h.run("v0.1.11"), h.out.String())
			assert.Contains(t, h.lines(), tc.want)
		})
	}
}

// ── the pin ────────────────────────────────────────────────────────────────────

func TestMovePin(t *testing.T) {
	h := newHarness(t)
	h.u.targetImage = "ghcr.io/weiliang79/belune:0.1.11"
	t.Chdir(h.install)
	// Every BELUNE_IMAGE= line is rewritten (all of the line); commented-out and
	// indented ones are not; CRLF and a missing final newline survive.
	writeFile(t, ".env", "# BELUNE_IMAGE=old\r\nBELUNE_IMAGE=ghcr.io/weiliang79/belune:0.1.10\r\nA=1\nBELUNE_IMAGE=dup\n BELUNE_IMAGE=indented\nLAST=nonewline", 0o600)

	require.NoError(t, h.u.movePin())

	assert.Equal(t, "# BELUNE_IMAGE=old\r\nBELUNE_IMAGE=ghcr.io/weiliang79/belune:0.1.11\nA=1\nBELUNE_IMAGE=ghcr.io/weiliang79/belune:0.1.11\n BELUNE_IMAGE=indented\nLAST=nonewline", h.read(".env"))
	assert.NoFileExists(t, ".env.tmp")
	fi, err := os.Stat(".env")
	require.NoError(t, err)
	// Parity, not a design: the sed it replaces went through a freshly created
	// .env.tmp, so .env has always come out 0644 whatever mode it had. Changing
	// that is a behaviour change to be made on purpose, in its own commit.
	assert.Equal(t, os.FileMode(0o644), fi.Mode().Perm())
}

// ── filesystem steps ───────────────────────────────────────────────────────────

func TestInfraSwapMatchesCpAndChmod(t *testing.T) {
	t.Run("new files take the source mode, existing ones keep their own", func(t *testing.T) {
		h := newHarness(t)
		writeFile(t, filepath.Join(h.install, "scripts", "update.sh"), "old\n", 0o640)
		writeFile(t, filepath.Join(h.install, ".env.example"), "old\n", 0o600)

		require.Equal(t, 0, h.run("v0.1.11"), h.out.String())

		mode := func(rel string) os.FileMode {
			fi, err := os.Stat(filepath.Join(h.install, rel))
			require.NoError(t, err)
			return fi.Mode().Perm()
		}
		assert.Equal(t, os.FileMode(0o751), mode("scripts/update.sh"), "0640 stays 0640, then +x under umask 022")
		assert.Equal(t, os.FileMode(0o600), mode(".env.example"), "an existing file keeps its mode")
		assert.Equal(t, os.FileMode(0o644), mode("infra/caddy/Caddyfile.template"), "a new file takes the staged mode")
		assert.Equal(t, os.FileMode(0o755), mode("scripts/backup.sh"))
		assert.Equal(t, os.FileMode(0o755), mode("scripts/restore.sh"))
	})

	t.Run("chmod +x honours the umask", func(t *testing.T) {
		p := filepath.Join(t.TempDir(), "f")
		require.NoError(t, os.WriteFile(p, nil, 0o644))
		require.NoError(t, addExec(p, 0o077))
		fi, err := os.Stat(p)
		require.NoError(t, err)
		assert.Equal(t, os.FileMode(0o744), fi.Mode().Perm())
	})

	t.Run("cp into a directory puts the file inside it", func(t *testing.T) {
		dir := t.TempDir()
		src := filepath.Join(dir, "src")
		require.NoError(t, os.WriteFile(src, []byte("x"), 0o644))
		require.NoError(t, os.Mkdir(filepath.Join(dir, "dst"), 0o755))
		require.NoError(t, copyFile(src, filepath.Join(dir, "dst")))
		assert.FileExists(t, filepath.Join(dir, "dst", "src"))
	})

	t.Run("cp refuses a directory without touching the destination", func(t *testing.T) {
		dir := t.TempDir()
		dst := filepath.Join(dir, "dst")
		require.NoError(t, os.WriteFile(dst, []byte("keep"), 0o644))
		err := copyFile(dir, dst)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "omitting directory")
		b, _ := os.ReadFile(dst)
		assert.Equal(t, "keep", string(b))
	})
}

func TestToolErrorsReadLikeCoreutils(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "file"), nil, 0o644))

	err := mkdirAll(filepath.Join(dir, "file", "sub"))
	assert.EqualError(t, err, "mkdir: cannot create directory '"+filepath.Join(dir, "file")+"': Not a directory", "an ancestor in the way")
	err = mkdirAll(filepath.Join(dir, "file"))
	assert.EqualError(t, err, "mkdir: cannot create directory '"+filepath.Join(dir, "file")+"': File exists", "the path itself in the way")

	err = copyFile(filepath.Join(dir, "missing"), filepath.Join(dir, "x"))
	assert.EqualError(t, err, "cp: cannot stat '"+filepath.Join(dir, "missing")+"': No such file or directory")

	err = chmod(filepath.Join(dir, "missing"), 0o600)
	assert.EqualError(t, err, "chmod: changing permissions of '"+filepath.Join(dir, "missing")+"': No such file or directory")
}

// The uid:gid come from `id` inside the TARGET image; here the current user's own
// are used so the chown is legal without root. As root the ownership is checked
// against ids that really differ.
func TestRun_ChownsTheDirectoriesTheContainerWritesTo(t *testing.T) {
	uid, gid := os.Getuid(), os.Getgid()
	if os.Geteuid() == 0 {
		uid, gid = 12345, 23456
	}
	h := newHarness(t)
	h.handler = func(c cmd) (bool, error) {
		if flag, ok := idProbe(c); ok {
			_, _ = io.WriteString(c.stdout, strconv.Itoa(map[string]int{"-u": uid, "-g": gid}[flag])+"\n")
			return true, nil
		}
		return false, nil
	}
	// A root-created lock, as the host CLI's or the pre-update backup's run leaves.
	writeFile(t, filepath.Join(h.install, "backups", ".lock"), "", 0o644)

	require.Equal(t, 0, h.run("v0.1.11"), h.out.String())

	for _, rel := range []string{"filemounts", "backups", "backups/.lock", "backup-remote.env"} {
		fi, err := os.Stat(filepath.Join(h.install, rel))
		require.NoError(t, err, rel)
		st := fi.Sys().(*syscall.Stat_t)
		assert.Equal(t, uint32(uid), st.Uid, rel)
		assert.Equal(t, uint32(gid), st.Gid, rel)
	}
	// Without this the non-root worker cannot open the lock, and every dashboard
	// backup reports "already in progress" forever.
	fi, err := os.Stat(filepath.Join(h.install, "backups", ".lock"))
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o666), fi.Mode().Perm())
	fi, err = os.Stat(filepath.Join(h.install, "backup-remote.env"))
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), fi.Mode().Perm())
}

func TestRun_NeedsBothIDsOrChownsNothing(t *testing.T) {
	h := newHarness(t)
	h.handler = func(c cmd) (bool, error) {
		if flag, ok := idProbe(c); ok {
			if flag == "-u" {
				_, _ = io.WriteString(c.stdout, "garbage-that-chown-would-reject\n")
			}
			return true, nil // no gid
		}
		return false, nil
	}

	require.Equal(t, 0, h.run("v0.1.11"), h.out.String())
	assert.DirExists(t, filepath.Join(h.install, "filemounts"))
}

func TestRun_GarbageIDsFailLikeChownAndRevert(t *testing.T) {
	h := newHarness(t)
	h.handler = func(c cmd) (bool, error) {
		if _, ok := idProbe(c); ok {
			_, _ = io.WriteString(c.stdout, "nope\n")
			return true, nil
		}
		return false, nil
	}

	assert.Equal(t, 1, h.run("v0.1.11"))
	assert.Contains(t, h.out.String(), "chown: invalid user: 'nope:nope'")
	assert.Contains(t, h.out.String(), "Restored. Nothing has changed.")
	assert.Equal(t, origEnv, h.read(".env"))
}

// ── helper binary ──────────────────────────────────────────────────────────────

func TestRun_ReextractsTheBackupHelper(t *testing.T) {
	t.Run("extracted and executable", func(t *testing.T) {
		h := newHarness(t)
		require.Equal(t, 0, h.run("v0.1.11"), h.out.String())
		assert.Contains(t, h.out.String(), "belune-backup-upload updated.")
		assert.Equal(t, "HELPER", h.read("bin/belune-backup-upload"))
		fi, err := os.Stat(filepath.Join(h.install, "bin", "belune-backup-upload"))
		require.NoError(t, err)
		assert.Equal(t, os.FileMode(0o755), fi.Mode().Perm())
	})
	t.Run("not in the image is not an error", func(t *testing.T) {
		h := newHarness(t)
		h.handler = func(c cmd) (bool, error) {
			return c.name == "docker" && c.args[0] == "run" && c.args[len(c.args)-2] == "cat", io.EOF
		}
		require.Equal(t, 0, h.run("v0.1.11"), h.out.String())
		assert.Contains(t, h.out.String(), "belune-backup-upload not found in image — skipping.")
		assert.Contains(t, h.out.String(), "Updated 0.1.10 → 0.1.11")
	})
}

// ── systemd ────────────────────────────────────────────────────────────────────

func TestRun_Systemd(t *testing.T) {
	// The staged copies are "staged\n", so a unit holding that is "unchanged".
	setup := func(t *testing.T, files map[string]string) *harness {
		h := newHarness(t)
		for name, body := range files {
			writeFile(t, filepath.Join(h.u.systemdDir, name), body, 0o644)
		}
		return h
	}
	const hint = "systemd units changed in this release. To apply them:"

	t.Run("no systemd units installed: silent", func(t *testing.T) {
		h := newHarness(t)
		require.Equal(t, 0, h.run("v0.1.11"), h.out.String())
		assert.NotContains(t, h.out.String(), hint)
		assert.False(t, h.did("systemctl"))
	})
	t.Run("unchanged units: silent", func(t *testing.T) {
		h := setup(t, map[string]string{"belune.service": "staged\n", "belune-backup.service": "staged\n"})
		require.Equal(t, 0, h.run("v0.1.11"), h.out.String())
		assert.NotContains(t, h.out.String(), hint)
	})
	t.Run("a changed unit is announced once", func(t *testing.T) {
		h := setup(t, map[string]string{"belune.service": "old\n", "belune-backup.service": "old\n"})
		require.Equal(t, 0, h.run("v0.1.11"), h.out.String())
		assert.Equal(t, 1, strings.Count(h.out.String(), hint))
		// The harness installs into a t.TempDir(), i.e. NOT /opt/belune, so the
		// remediation must be the rewriting form — a plain `cp` would install
		// units pointing at a directory that does not exist on this host.
		assert.Contains(t, h.out.String(), `      sudo sed "s|/opt/belune|`+h.install+`|g" infra/systemd/belune.service > /etc/systemd/system/belune.service`)
		assert.Contains(t, h.out.String(), "      sudo systemctl daemon-reload\n")
		assert.NotContains(t, h.out.String(), "sudo cp infra/systemd/*.service")
	})

	// ⚠️ The bug this guards: install.sh rewrites /opt/belune to the real install
	// dir, so comparing the repo copy verbatim reported drift on EVERY update of
	// a non-default install — and told the operator to run a cp that would break
	// it. Both units are staged carrying the default path; the installed copies
	// carry the rewritten one, which is "unchanged", not drift.
	t.Run("a rewritten unit on a non-default install dir is not drift", func(t *testing.T) {
		h := newHarness(t)
		h.handler = func(c cmd) (bool, error) {
			if c.name != "curl" {
				return false, nil
			}
			for i, a := range c.args {
				if a == "-o" && strings.HasSuffix(c.args[i+1], ".service") {
					return true, os.WriteFile(c.args[i+1], []byte("WorkingDirectory=/opt/belune\n"), 0o644)
				}
			}
			return false, nil
		}
		for _, unit := range []string{"belune.service", "belune-backup.service"} {
			writeFile(t, filepath.Join(h.u.systemdDir, unit), "WorkingDirectory="+h.install+"\n", 0o644)
		}
		require.Equal(t, 0, h.run("v0.1.11"), h.out.String())
		assert.NotContains(t, h.out.String(), hint)
	})
	t.Run("an unreadable unit counts as changed", func(t *testing.T) {
		h := setup(t, map[string]string{"belune.service": "staged\n"}) // belune-backup.service missing
		require.Equal(t, 0, h.run("v0.1.11"), h.out.String())
		assert.Contains(t, h.out.String(), hint)
	})
	t.Run("the old backup timer is retired", func(t *testing.T) {
		h := setup(t, map[string]string{"belune.service": "staged\n", "belune-backup.service": "staged\n", "belune-backup.timer": "t\n"})
		require.Equal(t, 0, h.run("v0.1.11"), h.out.String())
		assert.Contains(t, h.out.String(), "Retiring belune-backup.timer — daily backups now run in-app (Server → Backups).")
		assert.Contains(t, h.out.String(), "belune-backup.timer disabled and removed.")
		assert.NoFileExists(t, filepath.Join(h.u.systemdDir, "belune-backup.timer"))
		assert.Equal(t, []string{"systemctl disable --now belune-backup.timer", "systemctl daemon-reload"}, systemctlLines(h))
	})
	t.Run("a timer alone is left alone: the units gate it", func(t *testing.T) {
		h := setup(t, map[string]string{"belune-backup.timer": "t\n"})
		require.Equal(t, 0, h.run("v0.1.11"), h.out.String())
		assert.FileExists(t, filepath.Join(h.u.systemdDir, "belune-backup.timer"))
	})
	t.Run("a failing disable is ignored but a failing daemon-reload is not", func(t *testing.T) {
		h := setup(t, map[string]string{"belune.service": "staged\n", "belune-backup.service": "staged\n", "belune-backup.timer": "t\n"})
		h.handler = func(c cmd) (bool, error) {
			if c.name != "systemctl" {
				return false, nil
			}
			if c.args[0] == "disable" {
				return true, io.EOF
			}
			return true, exec.Command("sh", "-c", "exit 3").Run()
		}
		// The update itself succeeded; set -e still ended the script with
		// systemctl's own status.
		assert.Equal(t, 3, h.run("v0.1.11"), h.out.String())
		assert.NotContains(t, h.out.String(), "disabled and removed")
		assert.Equal(t, "BELUNE_IMAGE=ghcr.io/weiliang79/belune:0.1.11", strings.Split(h.read(".env"), "\n")[0], "no revert after up -d")
	})
}

func systemctlLines(h *harness) []string {
	var out []string
	for _, l := range h.lines() {
		if strings.HasPrefix(l, "systemctl") {
			out = append(out, l)
		}
	}
	return out
}

func TestNewUpdater_EmptyBeluneDirIsUnset(t *testing.T) {
	t.Setenv("BELUNE_DIR", "")
	assert.Equal(t, "/opt/belune", newUpdater().installDir)
	t.Setenv("BELUNE_DIR", "/srv/belune")
	assert.Equal(t, "/srv/belune", newUpdater().installDir)
}
