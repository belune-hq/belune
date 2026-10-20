package backup

import (
	"fmt"
	"os"
	"strings"

	"github.com/belune-hq/belune/internal/config"
)

// RemoteConfig is the resolved control-plane remote-storage configuration —
// comparable with == so Service can cheaply detect when it needs to rebuild
// its minio client.
type RemoteConfig struct {
	Enabled   bool
	Endpoint  string
	Region    string
	Bucket    string
	Prefix    string
	AccessKey string
	SecretKey string
	UseSSL    bool
}

// remoteConfigKeys, in the order SaveRemoteConfig writes them. Matches the
// BACKUP_S3_*/BACKUP_REMOTE_ENABLED names in .env so scripts/backup.sh (which
// greps the same key names) and a dashboard-managed
// cfg.BackupRemoteConfigPath file are interchangeable.
const (
	keyEnabled   = "BACKUP_REMOTE_ENABLED"
	keyEndpoint  = "BACKUP_S3_ENDPOINT"
	keyRegion    = "BACKUP_S3_REGION"
	keyBucket    = "BACKUP_S3_BUCKET"
	keyPrefix    = "BACKUP_S3_PREFIX"
	keyAccessKey = "BACKUP_S3_ACCESS_KEY"
	keySecretKey = "BACKUP_S3_SECRET_KEY"
	keyUseSSL    = "BACKUP_S3_USE_SSL"
)

// LoadRemoteConfig resolves control-plane remote-storage config fresh on
// every call (no caching) — cfg.BackupRemoteConfigPath is read directly, with
// per-field fallback to the .env-sourced BackupS3* fields on cfg for any key
// missing from that file (partial files and "file absent" both fall back
// cleanly). This is what lets a dashboard edit take effect on the very next
// backup, with no API restart.
func LoadRemoteConfig(cfg *config.Config) RemoteConfig {
	vals, _ := readFlatEnvFile(cfg.BackupRemoteConfigPath)

	get := func(key, fallback string) string {
		if v, ok := vals[key]; ok && v != "" {
			return v
		}
		return fallback
	}
	getBool := func(key string, fallback bool) bool {
		if v, ok := vals[key]; ok {
			return strings.EqualFold(v, "true")
		}
		return fallback
	}

	return RemoteConfig{
		Enabled:   getBool(keyEnabled, cfg.BackupRemoteEnabled),
		Endpoint:  get(keyEndpoint, cfg.BackupS3Endpoint),
		Region:    get(keyRegion, cfg.BackupS3Region),
		Bucket:    get(keyBucket, cfg.BackupS3Bucket),
		Prefix:    get(keyPrefix, cfg.BackupS3Prefix),
		AccessKey: get(keyAccessKey, cfg.BackupS3AccessKey),
		SecretKey: get(keySecretKey, cfg.BackupS3SecretKey),
		UseSSL:    getBool(keyUseSSL, cfg.BackupS3UseSSL),
	}
}

// SaveRemoteConfig writes rc to path as flat KEY=value lines, mode 0600
// (contains a secret key). Overwrites the EXISTING file in place rather than
// write-to-temp-then-rename: install.sh/update.sh `touch` and chown this file
// (not its parent directory, which stays root-owned) to the belune container
// uid, the same managed-host-file pattern as file mounts — so the container's
// non-root user has write access to the file's contents but cannot create a
// new directory entry (a rename's destination) in its parent. The file must
// already exist; install.sh/update.sh guarantee that.
func SaveRemoteConfig(path string, rc RemoteConfig) error {
	var b strings.Builder
	fmt.Fprintf(&b, "%s=%s\n", keyEnabled, boolStr(rc.Enabled))
	fmt.Fprintf(&b, "%s=%s\n", keyEndpoint, rc.Endpoint)
	fmt.Fprintf(&b, "%s=%s\n", keyRegion, rc.Region)
	fmt.Fprintf(&b, "%s=%s\n", keyBucket, rc.Bucket)
	fmt.Fprintf(&b, "%s=%s\n", keyPrefix, rc.Prefix)
	fmt.Fprintf(&b, "%s=%s\n", keyAccessKey, rc.AccessKey)
	fmt.Fprintf(&b, "%s=%s\n", keySecretKey, rc.SecretKey)
	fmt.Fprintf(&b, "%s=%s\n", keyUseSSL, boolStr(rc.UseSSL))

	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

func boolStr(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

// readFlatEnvFile parses a flat KEY=value file: one assignment per line,
// blank lines and #-comments skipped, values optionally double-quoted. A
// missing file returns (nil, nil) — the normal, expected case before the
// dashboard has ever written one — so callers should fall back, not error.
func readFlatEnvFile(path string) (map[string]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	result := make(map[string]string)
	for line := range strings.SplitSeq(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		result[strings.TrimSpace(key)] = strings.Trim(strings.TrimSpace(value), `"`)
	}
	return result, nil
}
