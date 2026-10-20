package worker

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"filippo.io/age"

	"github.com/belune-hq/belune/internal/config"
	"github.com/belune-hq/belune/internal/service/backup"
)

// controlPlaneArchivePrefix names every control-plane backup archive and its
// working directory. Must match scripts/backup.sh's BACKUP_NAME exactly — the
// two producers share one consumer (restore.sh) and one retention sweep.
const controlPlaneArchivePrefix = "belune-backup-"

// controlPlaneLockName is the lockfile scripts/backup.sh also takes (via
// `flock`), inside the same bind-mounted backups directory.
const controlPlaneLockName = ".lock"

// resolveInfraContainer finds a platform container by its compose-service
// label, scoped to the same compose project as Caddy (mirrors
// handler.resolvePlatformContainer — duplicated rather than shared because the
// handler's version lives on Handler, not TaskHandler, and pulling it into a
// shared package for two callers isn't worth the indirection yet).
func (h *TaskHandler) resolveInfraContainer(ctx context.Context, svcLabel string) (string, error) {
	rt, err := h.Runtimes.Local(ctx)
	if err != nil {
		return "", fmt.Errorf("resolve local runtime: %w", err)
	}
	all, err := rt.ListAllContainers(ctx)
	if err != nil {
		return "", err
	}

	project := ""
	for _, c := range all {
		if c.Name == h.Config.CaddyContainerName {
			project = c.Labels["com.docker.compose.project"]
			break
		}
	}

	for _, c := range all {
		if c.Labels["com.docker.compose.service"] != svcLabel {
			continue
		}
		if project != "" && c.Labels["com.docker.compose.project"] != project {
			continue
		}
		return c.ID, nil
	}
	return "", fmt.Errorf("no container for service %q", svcLabel)
}

// resolveCaddyContainer resolves Caddy the same way handler.resolvePlatformContainer
// does: the configured container name takes priority over label discovery.
func (h *TaskHandler) resolveCaddyContainer(ctx context.Context) (string, error) {
	if h.Config.CaddyContainerName != "" {
		return h.Config.CaddyContainerName, nil
	}
	return h.resolveInfraContainer(ctx, "caddy")
}

// buildControlPlaneArchive produces a belune-backup-<UTC-ts>.tar[.gz][.age]
// archive under h.Config.ControlPlaneBackupDir containing postgres.sql,
// caddy-data.tar.gz (if Caddy is running), and .env — byte-for-byte the same
// layout scripts/backup.sh produces, so restore.sh (host-only, unchanged) can
// consume archives from either producer. lg receives step/warn lines.
func (h *TaskHandler) buildControlPlaneArchive(ctx context.Context, lg *runLog) (archivePath string, err error) {
	backupDir := h.Config.ControlPlaneBackupDir
	if err := os.MkdirAll(backupDir, 0o755); err != nil {
		return "", fmt.Errorf("create backup dir: %w", err)
	}

	name := controlPlaneArchivePrefix + time.Now().UTC().Format("20060102T150405Z")
	workDir := filepath.Join(backupDir, name)
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		return "", fmt.Errorf("create work dir: %w", err)
	}
	defer os.RemoveAll(workDir)

	lg.step("Dumping Postgres database...")
	pgContainer, err := h.resolveInfraContainer(ctx, "postgres")
	if err != nil {
		return "", fmt.Errorf("resolve postgres container: %w", err)
	}
	if err := h.dumpPostgres(ctx, pgContainer, filepath.Join(workDir, "postgres.sql")); err != nil {
		return "", fmt.Errorf("dump postgres: %w", err)
	}
	lg.step("Postgres dump written")

	lg.step("Backing up Caddy TLS data...")
	if caddyContainer, cErr := h.resolveCaddyContainer(ctx); cErr != nil {
		lg.warn("Caddy container not found — skipping TLS backup: %v", cErr)
	} else if tErr := h.tarCaddyData(ctx, caddyContainer, filepath.Join(workDir, "caddy-data.tar.gz")); tErr != nil {
		lg.warn("Caddy data backup failed (continuing): %v", tErr)
	} else {
		lg.step("Caddy data written")
	}

	lg.step("Copying .env...")
	if err := copyFile(h.Config.EnvFilePath, filepath.Join(workDir, ".env")); err != nil {
		return "", fmt.Errorf("copy .env: %w", err)
	}
	lg.step(".env backed up")

	// Dashboard-managed remote-storage config, if the operator has ever saved
	// one (install.sh/update.sh always touch the file, but on a stock install
	// it's just never been written to). Best-effort — an absent or unreadable
	// file just means the archive has one less thing to restore, not a failed
	// backup.
	if err := copyFile(h.Config.BackupRemoteConfigPath, filepath.Join(workDir, "backup-remote.env")); err != nil {
		lg.warn("backup-remote.env not included: %v", err)
	} else {
		lg.step("backup-remote.env backed up")
	}

	archivePath = filepath.Join(backupDir, name+".tar.gz")
	lg.step("Creating archive %s...", archivePath)
	if err := tarGzDir(workDir, name, archivePath); err != nil {
		return "", fmt.Errorf("create archive: %w", err)
	}

	if key := h.Config.BackupEncryptionKey; key != "" {
		lg.step("Encrypting archive with age...")
		encPath := archivePath + ".age"
		if err := ageEncryptFile(archivePath, encPath, key); err != nil {
			return "", fmt.Errorf("encrypt archive: %w", err)
		}
		_ = os.Remove(archivePath)
		archivePath = encPath
		lg.step("Encrypted archive: %s", archivePath)
	}

	return archivePath, nil
}

// dumpPostgres runs pg_dump inside the postgres container and streams it to a
// local file, matching scripts/backup.sh's `docker exec … pg_dump … > file`.
func (h *TaskHandler) dumpPostgres(ctx context.Context, container, destPath string) error {
	f, err := os.Create(destPath)
	if err != nil {
		return err
	}
	defer f.Close()

	user := envOrDefault("POSTGRES_USER", "belune")
	db := envOrDefault("POSTGRES_DB", "belune")

	rt, err := h.Runtimes.Local(ctx)
	if err != nil {
		return fmt.Errorf("resolve local runtime: %w", err)
	}

	var stderr bytes.Buffer
	exit, execErr := rt.ContainerExec(ctx, container,
		[]string{"pg_dump", "-U", user, "-d", db, "--no-password"}, nil, f, &stderr)
	if execErr != nil {
		return execErr
	}
	if exit != 0 {
		return fmt.Errorf("pg_dump exited %d: %s", exit, strings.TrimSpace(stderr.String()))
	}
	return nil
}

// tarCaddyData runs `tar -czf - /data /config` inside the Caddy container and
// streams it to a local file, matching scripts/backup.sh.
func (h *TaskHandler) tarCaddyData(ctx context.Context, container, destPath string) error {
	f, err := os.Create(destPath)
	if err != nil {
		return err
	}
	defer f.Close()

	rt, err := h.Runtimes.Local(ctx)
	if err != nil {
		return fmt.Errorf("resolve local runtime: %w", err)
	}

	var stderr bytes.Buffer
	exit, execErr := rt.ContainerExec(ctx, container,
		[]string{"tar", "-czf", "-", "/data", "/config"}, nil, f, &stderr)
	if execErr != nil {
		return execErr
	}
	if exit != 0 {
		return fmt.Errorf("tar exited %d: %s", exit, strings.TrimSpace(stderr.String()))
	}
	return nil
}

// envOrDefault reads a POSTGRES_* value from the process environment — the
// belune container already gets these via compose's env_file, the same .env
// scripts/backup.sh greps — falling back to fallback if unset.
func envOrDefault(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// copyFile copies src to dst, creating dst (and truncating it if present).
func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, in)
	return err
}

// tarGzDir writes a gzip-compressed tar of srcDir to destPath, with every
// entry's name prefixed by rootName + "/" — equivalent to
// `tar -czf dest -C $(dirname srcDir) rootName`.
func tarGzDir(srcDir, rootName, destPath string) error {
	out, err := os.Create(destPath)
	if err != nil {
		return err
	}
	defer out.Close()

	gz := gzip.NewWriter(out)
	tw := tar.NewWriter(gz)

	walkErr := filepath.Walk(srcDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(srcDir, path)
		if err != nil {
			return err
		}
		name := rootName
		if rel != "." {
			name = rootName + "/" + filepath.ToSlash(rel)
		}

		hdr, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}
		hdr.Name = name
		if info.IsDir() {
			hdr.Name += "/"
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		defer f.Close()
		_, err = io.Copy(tw, f)
		return err
	})
	if walkErr != nil {
		return walkErr
	}

	if err := tw.Close(); err != nil {
		return err
	}
	return gz.Close()
}

// ageEncryptFile encrypts srcPath to destPath for the given age recipient.
// recipientOrPath is either a literal age1... public key or a path to a file
// containing one, matching scripts/backup.sh's BACKUP_ENCRYPTION_KEY handling.
func ageEncryptFile(srcPath, destPath, recipientOrPath string) error {
	pubKey := recipientOrPath
	if data, err := os.ReadFile(recipientOrPath); err == nil {
		pubKey = strings.TrimSpace(string(data))
	}

	recipient, err := age.ParseX25519Recipient(pubKey)
	if err != nil {
		return fmt.Errorf("parse age recipient: %w", err)
	}

	in, err := os.Open(srcPath)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.Create(destPath)
	if err != nil {
		return err
	}
	defer out.Close()

	w, err := age.Encrypt(out, recipient)
	if err != nil {
		return fmt.Errorf("open age writer: %w", err)
	}
	if _, err := io.Copy(w, in); err != nil {
		return err
	}
	return w.Close()
}

// resolveBackupRetention reads the dashboard-editable retention settings
// fresh, falling back per-key to the .env-configured Config defaults when a
// setting is unset. Mirrors handler.resolveBackupRetention — kept as a
// separate copy since the handler and worker packages don't share a
// settings-reading helper, and this is the only place either needs one.
func (h *TaskHandler) resolveBackupRetention(ctx context.Context) (days, count int) {
	days, count = h.Config.BackupRetainDays, h.Config.BackupRetainCount
	if s, err := h.Queries.GetSetting(ctx, config.SettingControlPlaneBackupRetainDays); err == nil {
		if n, convErr := strconv.Atoi(strings.TrimSpace(s.Value)); convErr == nil && n > 0 {
			days = n
		}
	}
	if s, err := h.Queries.GetSetting(ctx, config.SettingControlPlaneBackupRetainCount); err == nil {
		if n, convErr := strconv.Atoi(strings.TrimSpace(s.Value)); convErr == nil && n > 0 {
			count = n
		}
	}
	return days, count
}

// rotateLocalControlPlaneBackups deletes local control-plane archives beyond
// the retention policy (resolveBackupRetention — the same knobs that already
// govern remote rotation), mirroring pruneDatabaseBackups' keep-whichever-more
// rule. Best-effort; a listing/removal failure is logged and does not fail
// the backup that triggered it.
func (h *TaskHandler) rotateLocalControlPlaneBackups(ctx context.Context, lg *runLog) {
	entries, err := os.ReadDir(h.Config.ControlPlaneBackupDir)
	if err != nil {
		lg.warn("local rotation: list backup dir: %v", err)
		return
	}

	var objects []backup.BackupObject
	paths := map[string]string{} // key -> full path
	for _, e := range entries {
		if e.IsDir() || !strings.HasPrefix(e.Name(), controlPlaneArchivePrefix) {
			continue
		}
		if !strings.Contains(e.Name(), ".tar.gz") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		objects = append(objects, backup.BackupObject{
			Key:          e.Name(),
			Size:         info.Size(),
			LastModified: info.ModTime(),
		})
		paths[e.Name()] = filepath.Join(h.Config.ControlPlaneBackupDir, e.Name())
	}
	sort.Slice(objects, func(i, j int) bool { return objects[i].LastModified.Before(objects[j].LastModified) })

	retainDays, retainCount := h.resolveBackupRetention(ctx)
	toDelete := backup.SelectForDeletion(objects, time.Now(), retainDays, retainCount)
	for _, key := range toDelete {
		if err := os.Remove(paths[key]); err != nil {
			lg.warn("local rotation: remove %s: %v", key, err)
		}
	}
	if len(toDelete) > 0 {
		lg.step("Local rotation removed %d old archive(s)", len(toDelete))
	}
}
