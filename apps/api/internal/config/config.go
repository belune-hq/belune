package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/weiliang79/belune/internal/pkg/crypto"
)

// SettingPublicIP is the settings-table key that overrides the BELUNE_PUBLIC_IP
// env baseline at runtime, so the operator can fix the advertised server IP from
// the dashboard without a restart. Empty/unset falls back to the env, then to
// autodetection. Shared by the settings handler (validates + writes) and the TLS
// DNS check (reads).
const SettingPublicIP = "public_ip"

// SettingControlPlaneBackupEnabled gates the in-app cron sweep for
// control-plane backups (Postgres + Caddy TLS data + .env) — "false" disables
// it; any other value (including unset) enables it. Manual "Run Backup Now"
// clicks are unaffected either way.
const SettingControlPlaneBackupEnabled = "control_plane_backup_enabled"

// SettingControlPlaneBackupSchedule is the cron expression the sweep checks
// control-plane backups against. Unset falls back to
// DefaultControlPlaneBackupSchedule.
const SettingControlPlaneBackupSchedule = "control_plane_backup_schedule"

// DefaultControlPlaneBackupSchedule matches the cadence of the systemd timer
// it replaces (belune-backup.timer ran at 02:00 daily), so upgrading an
// existing install preserves the backup cadence operators are used to.
const DefaultControlPlaneBackupSchedule = "0 2 * * *"

// SettingControlPlaneBackupRetainDays/Count are the dashboard-editable
// retention knobs for control-plane backups (both local and remote copies).
// Unset falls back to Config.BackupRetainDays/BackupRetainCount (the .env
// values) — read fresh at rotation time, same fallback shape as the
// schedule above.
const SettingControlPlaneBackupRetainDays = "control_plane_backup_retain_days"
const SettingControlPlaneBackupRetainCount = "control_plane_backup_retain_count"

type Config struct {
	Port               int
	DatabaseURL        string
	RedisURL           string
	JWTSecret          string
	JWTExpiryHours     int // access-token TTL in hours (legacy name; default 1)
	JWTRefreshHours    int // refresh-token TTL in hours (default 168 = 7 days)
	Keyring            *crypto.Keyring
	CaddyAdminURL      string
	CaddyContainerName string // Docker container name/ID for Caddy; used to attach it to per-project networks
	// APIContainerName is this control-plane container's own Docker name/ID. The
	// worker attaches it to each per-project network so the post-deploy health
	// probe can reach app containers directly (app-to-app isolation is unaffected;
	// only the trusted control plane bridges in). Empty disables self-attach.
	APIContainerName string
	// CaddyTLSProbeAddr is where the TLS status probe dials to see what Caddy
	// actually serves. It is the proxy's own HTTPS listener, reached over the
	// internal network — never the public hostname, which would leave the check
	// at the mercy of external DNS and firewalls.
	CaddyTLSProbeAddr string
	// DashboardUpstream is the address Caddy dials to reach this API when serving
	// the dashboard. It must be resolvable *from inside the Caddy container*, so
	// it is the compose service name — not localhost, which would be Caddy itself.
	DashboardUpstream string
	// PublicIP is the address a user's DNS record must point at for a certificate
	// to be issuable. Empty means autodetect; autodetection failing is not fatal,
	// it just disables the DNS precheck.
	PublicIP      string
	AccessLogPath string
	CORSOrigins   []string
	SecureCookies bool
	TLS           bool   // when true, send HSTS headers
	LogLevel      string // debug, info, warn, error (default: info)
	// LogFormat selects the console encoding: "console" (human-readable,
	// default) or "json". JSON is worth keeping for anything that parses the
	// stream; the console format is for reading a terminal.
	LogFormat string // console, json (default: console)
	// LogColor: auto (colour only when stdout is a terminal) | always | never.
	LogColor            string // auto, always, never (default: auto)
	DisableRateLimiting bool   // set true in tests to avoid per-IP counter accumulation

	// Timeouts
	BuildTimeoutMinutes     int // max duration for build operations (default 30)
	TaskTimeoutMinutes      int // max duration for asynq tasks (default 45)
	ImagePullTimeoutMinutes int // max duration for image pull operations (default 10)

	// Resource limits
	MaxTerminalSessionsPerUser int // per-user cap on concurrent terminal sessions (default 5)
	MaxWebSocketConnsPerUser   int // per-user cap on concurrent WebSocket connections (default 20)

	// Preview environments
	PreviewIdleDays int // days after which idle preview apps are garbage-collected (default 7; 0 disables)

	// SkipMigrations is the runtime kill-switch for auto-migration. Set
	// BELUNE_SKIP_MIGRATIONS=true to bring the API up without running pending
	// migrations — useful when an in-progress migration left the schema in a
	// state the operator wants to repair manually before letting the next
	// version of the binary touch it.
	SkipMigrations bool

	// Tracing
	OTLPEndpoint string // OTEL_EXPORTER_OTLP_ENDPOINT; empty = no-op tracer
	OTLPInsecure bool   // OTEL_EXPORTER_OTLP_INSECURE; default true (loopback collectors)

	// Metrics
	// MetricsBind, when non-empty, starts a separate HTTP listener that serves
	// /metrics without auth (Prometheus-friendly). Typically "127.0.0.1:9090".
	// When empty, /metrics is mounted on the main router behind admin auth.
	MetricsBind string

	// Public base URL — required when SMTP is configured; used to construct
	// links in outbound emails (password reset, invitations).
	PublicBaseURL string

	// Optional public URL for inbound provider webhooks (GitHub/GitLab/etc).
	// Falls back to PublicBaseURL when empty. Set this to a tunnel (smee.io,
	// cloudflared) when PublicBaseURL is a localhost dev origin so GitHub will
	// accept the App's webhook URL.
	WebhookPublicURL string

	// ServerSSHHost / ServerSSHUser are presentation-only hints shown in the
	// database External Access card to build the SSH-tunnel command (ssh -L).
	// When empty the UI falls back to a generic placeholder.
	ServerSSHHost string
	ServerSSHUser string

	// SMTP configuration for outbound email. All fields are optional; when
	// SMTPHost is empty the email service writes to slog instead of dialing.
	SMTPHost      string
	SMTPPort      int // default 587
	SMTPUser      string
	SMTPPassword  string
	SMTPFromEmail string
	SMTPFromName  string // default "Belune"
	SMTPTLSMode   string // none | starttls | tls (default: starttls)

	// Backup remote storage. These are the FALLBACK values, read once from .env
	// at startup — service/backup.LoadRemoteConfig() prefers
	// BackupRemoteConfigPath (dashboard-managed, re-read on every backup) and
	// falls back to these per-field when a key is absent from that file. When
	// neither source enables remote storage, backup.sh writes local archives
	// only.
	BackupRemoteEnabled bool
	BackupS3Endpoint    string // empty = AWS; set for MinIO/B2/R2/Wasabi
	BackupS3Region      string // default "us-east-1"
	BackupS3Bucket      string
	BackupS3AccessKey   string
	BackupS3SecretKey   string
	BackupS3Prefix      string // key prefix inside the bucket (default "belune/")
	BackupS3UseSSL      bool   // default true
	BackupRetainDays    int    // delete objects older than N days (default 30)
	BackupRetainCount   int    // always keep the N most-recent objects (default 14)
	// BackupRemoteConfigPath is the dashboard-managed remote-storage config —
	// flat KEY=value, same shape as BACKUP_S3_*/BACKUP_REMOTE_ENABLED in .env,
	// but separate from it (never let the dashboard write bootstrap secrets
	// like ENCRYPTION_KEY) and writable via the Remote Storage card. Read fresh
	// by both the worker's S3 client and scripts/backup.sh/belune-backup-upload
	// on every backup — no restart needed after an edit. Defaults to
	// $BELUNE_DIR/backup-remote.env.
	BackupRemoteConfigPath string
	// Local directory where managed-database logical dumps are written before
	// (optional) upload to S3. Defaults to $BELUNE_DIR/backups/databases.
	DatabaseBackupDir string
	// Host directory under which per-application file/config mounts are
	// materialised (<dir>/<app-id>/<file-id>) before being bind-mounted read-only
	// into the app container. Must be a HOST path the Docker daemon can bind —
	// on the containerised-API deploy it must be shared into the API container at
	// the same path. Defaults to $BELUNE_DIR/filemounts.
	FileMountsDir string
	// Image for the short-lived helper that tars/untars a database volume during
	// "other"-type volume-snapshot backup/restore. Must contain `tar` and `sh`.
	DatabaseBackupHelperImage string
	// Number of most-recent backups to keep per managed database; older ones are
	// pruned (rows + local files + S3 objects) after a new backup succeeds.
	DatabaseBackupRetainCount int

	// ScheduledTaskMaxConcurrent caps scheduled-task runs in flight at once.
	// The worker has a fixed pool shared with deploys, so this is what stops a
	// handful of slow `* * * * *` tasks from starving them.
	ScheduledTaskMaxConcurrent int

	// ControlPlaneBackupDir is where the worker writes belune-backup-*.tar.gz
	// archives (Postgres dump + Caddy TLS data + .env). Must be the same HOST
	// directory scripts/backup.sh and restore.sh use, bind-mounted into this
	// container — otherwise the two producers/one consumer archive format
	// invariant breaks. Defaults to $BELUNE_DIR/backups.
	ControlPlaneBackupDir string
	// EnvFilePath is where the worker reads .env from to copy verbatim into a
	// control-plane backup archive (env_file: injects variables into the
	// process, but the file itself isn't otherwise visible in-container).
	// Defaults to $BELUNE_DIR/.env.
	EnvFilePath string
	// BackupEncryptionKey, when set, is an age public key (or a path to a file
	// containing one) that control-plane backup archives are encrypted to.
	// Mirrors scripts/backup.sh's BACKUP_ENCRYPTION_KEY so both producers emit
	// the same archive format.
	BackupEncryptionKey string
}

// maxSafeJWTExpiryHours is the longest access-token TTL that is not worth
// warning about. Refresh tokens run 168 h and already cover session length, so
// nothing legitimate needs a long access token — and an access token cannot be
// revoked, so a long one is a window that cannot be closed.
const maxSafeJWTExpiryHours = 12

// Warning is a configuration finding: not fatal, but something the operator
// should act on. Surfaced at startup and on the dashboard.
type Warning struct {
	// Code identifies the finding for the frontend; the text is human-facing.
	Code    string `json:"code"`
	Message string `json:"message"`
	Remedy  string `json:"remedy"`
}

// Validate reports configuration that is accepted but inadvisable. It never
// fails startup: an operator locked out of their own control plane by a warning
// is worse than the setting the warning is about.
func (c *Config) Validate() []Warning {
	var warnings []Warning
	if c.JWTExpiryHours > maxSafeJWTExpiryHours {
		warnings = append(warnings, Warning{
			Code: "jwt_expiry_too_long",
			Message: fmt.Sprintf(
				"Access tokens are valid for %d hours. Installs created before v0.1.0 carry JWT_EXPIRY_HOURS=24 in their .env; the installer no longer writes it and the default is 1 hour. Access tokens cannot be revoked, so a stolen one stays usable for the full window.",
				c.JWTExpiryHours),
			Remedy: "Remove JWT_EXPIRY_HOURS from your .env and restart. Sessions are unaffected; refresh tokens already cover them.",
		})
	}
	return warnings
}

func Load() (*Config, error) {
	cfg := &Config{
		Port:               getEnvInt("PORT", 8080),
		DatabaseURL:        getEnv("DATABASE_URL", "postgres://belune:belune@localhost:5432/belune?sslmode=disable"),
		RedisURL:           getEnv("REDIS_URL", "redis://localhost:6379"),
		JWTSecret:          getEnv("JWT_SECRET", ""),
		JWTExpiryHours:     getEnvInt("JWT_EXPIRY_HOURS", 1),
		JWTRefreshHours:    getEnvInt("JWT_REFRESH_HOURS", 24*7),
		CaddyAdminURL:      getEnv("CADDY_ADMIN_URL", "http://localhost:2019"),
		CaddyContainerName: getEnv("CADDY_CONTAINER_NAME", "infra-caddy-1"),
		APIContainerName:   getEnv("API_CONTAINER_NAME", selfContainerRef()),
		CaddyTLSProbeAddr:  getEnv("CADDY_TLS_PROBE_ADDR", "caddy:443"),
		DashboardUpstream:  getEnv("DASHBOARD_UPSTREAM", "belune:8080"),
		PublicIP:           getEnv("BELUNE_PUBLIC_IP", ""),
		AccessLogPath:      getEnv("ACCESS_LOG_PATH", "../../infra/caddy/logs/access.log"),
		CORSOrigins:        getEnvList("CORS_ORIGINS", []string{"http://localhost:5173"}),
		SecureCookies:      getEnvBool("SECURE_COOKIES", false),
		TLS:                getEnvBool("TLS_ENABLED", false),
		LogLevel:           getEnv("LOG_LEVEL", "info"),
		LogFormat:          getEnv("LOG_FORMAT", "console"),
		LogColor:           getEnv("LOG_COLOR", "auto"),

		BuildTimeoutMinutes:     getEnvInt("BUILD_TIMEOUT_MINUTES", 30),
		TaskTimeoutMinutes:      getEnvInt("TASK_TIMEOUT_MINUTES", 45),
		ImagePullTimeoutMinutes: getEnvInt("IMAGE_PULL_TIMEOUT_MINUTES", 10),

		MaxTerminalSessionsPerUser: getEnvInt("MAX_TERMINAL_SESSIONS_PER_USER", 5),
		MaxWebSocketConnsPerUser:   getEnvInt("MAX_WEBSOCKET_CONNS_PER_USER", 20),

		PreviewIdleDays: getEnvInt("PREVIEW_IDLE_DAYS", 7),

		SkipMigrations: getEnvBool("BELUNE_SKIP_MIGRATIONS", false),

		OTLPEndpoint: getEnv("OTEL_EXPORTER_OTLP_ENDPOINT", ""),
		OTLPInsecure: getEnvBool("OTEL_EXPORTER_OTLP_INSECURE", true),

		MetricsBind: getEnv("METRICS_BIND", ""),

		PublicBaseURL:    getEnv("PUBLIC_BASE_URL", ""),
		WebhookPublicURL: getEnv("WEBHOOK_PUBLIC_URL", ""),

		ServerSSHHost: getEnv("SERVER_SSH_HOST", ""),
		ServerSSHUser: getEnv("SERVER_SSH_USER", ""),

		SMTPHost:      getEnv("SMTP_HOST", ""),
		SMTPPort:      getEnvInt("SMTP_PORT", 587),
		SMTPUser:      getEnv("SMTP_USER", ""),
		SMTPPassword:  getEnv("SMTP_PASSWORD", ""),
		SMTPFromEmail: getEnv("SMTP_FROM_EMAIL", ""),
		SMTPFromName:  getEnv("SMTP_FROM_NAME", "Belune"),
		SMTPTLSMode:   getEnv("SMTP_TLS_MODE", "starttls"),

		BackupRemoteEnabled:        getEnvBool("BACKUP_REMOTE_ENABLED", false),
		BackupS3Endpoint:           getEnv("BACKUP_S3_ENDPOINT", ""),
		BackupS3Region:             getEnv("BACKUP_S3_REGION", "us-east-1"),
		BackupS3Bucket:             getEnv("BACKUP_S3_BUCKET", ""),
		BackupS3AccessKey:          getEnv("BACKUP_S3_ACCESS_KEY", ""),
		BackupS3SecretKey:          getEnv("BACKUP_S3_SECRET_KEY", ""),
		BackupS3Prefix:             getEnv("BACKUP_S3_PREFIX", "belune/"),
		BackupS3UseSSL:             getEnvBool("BACKUP_S3_USE_SSL", true),
		BackupRemoteConfigPath:     getEnv("BACKUP_REMOTE_CONFIG_PATH", beluneDir()+"/backup-remote.env"),
		BackupRetainDays:           getEnvInt("BACKUP_RETAIN_DAYS", 30),
		BackupRetainCount:          getEnvInt("BACKUP_RETAIN_COUNT", 14),
		DatabaseBackupDir:          getEnv("DATABASE_BACKUP_DIR", beluneDir()+"/backups/databases"),
		FileMountsDir:              getEnv("FILE_MOUNTS_DIR", beluneDir()+"/filemounts"),
		DatabaseBackupHelperImage:  getEnv("DATABASE_BACKUP_HELPER_IMAGE", "alpine:3.20"),
		DatabaseBackupRetainCount:  getEnvInt("DATABASE_BACKUP_RETAIN_COUNT", 7),
		ScheduledTaskMaxConcurrent: getEnvInt("SCHEDULED_TASK_MAX_CONCURRENT", 2),
		ControlPlaneBackupDir:      getEnv("CONTROL_PLANE_BACKUP_DIR", beluneDir()+"/backups"),
		EnvFilePath:                getEnv("ENV_FILE_PATH", beluneDir()+"/.env"),
		BackupEncryptionKey:        getEnv("BACKUP_ENCRYPTION_KEY", ""),
	}

	if cfg.JWTSecret == "" {
		return nil, fmt.Errorf("JWT_SECRET is required")
	}
	if len(cfg.JWTSecret) < 32 {
		return nil, fmt.Errorf("JWT_SECRET must be at least 32 characters")
	}
	// Keyring accepts either ENCRYPTION_KEYS (multi-key) or the legacy
	// ENCRYPTION_KEY (single key, promoted to v1). At least one must be set.
	keyring, err := crypto.ParseKeyringEnv(
		getEnv("ENCRYPTION_KEYS", ""),
		getEnv("ENCRYPTION_KEY", ""),
		getEnv("ENCRYPTION_KEY_CURRENT", ""),
	)
	if err != nil {
		return nil, err
	}
	cfg.Keyring = keyring

	return cfg, nil
}

// beluneDir returns the PaaS install directory, used to derive default paths.
func beluneDir() string {
	if dir := os.Getenv("BELUNE_DIR"); dir != "" {
		return dir
	}
	return "/opt/belune"
}

func getEnv(key, fallback string) string {
	if val, ok := os.LookupEnv(key); ok {
		return val
	}
	return fallback
}

// selfContainerRef returns this process's own container reference so the worker
// can bridge itself onto per-project networks without the operator having to
// name the container. Docker sets a container's hostname to its short ID by
// default, which the Docker API accepts as a container reference for
// NetworkConnect. Outside Docker this is just the host name; the self-attach
// then no-ops with a warning, which is fine. An explicit API_CONTAINER_NAME
// still overrides this (e.g. when the container is run with a custom hostname).
func selfContainerRef() string {
	host, err := os.Hostname()
	if err != nil {
		return ""
	}
	return host
}

func getEnvInt(key string, fallback int) int {
	if val, ok := os.LookupEnv(key); ok {
		if i, err := strconv.Atoi(val); err == nil {
			return i
		}
	}
	return fallback
}

func getEnvList(key string, fallback []string) []string {
	if val, ok := os.LookupEnv(key); ok {
		parts := strings.Split(val, ",")
		result := make([]string, 0, len(parts))
		for _, p := range parts {
			if trimmed := strings.TrimSpace(p); trimmed != "" {
				result = append(result, trimmed)
			}
		}
		if len(result) > 0 {
			return result
		}
	}
	return fallback
}

func getEnvBool(key string, fallback bool) bool {
	if val, ok := os.LookupEnv(key); ok {
		if b, err := strconv.ParseBool(val); err == nil {
			return b
		}
	}
	return fallback
}
