export interface User {
  id: string;
  email: string;
  role: "admin" | "member";
  username: string;
  first_name: string;
  last_name: string;
  created_at?: string;
  last_active_at?: string | null;
}

export interface Project {
  id: string;
  name: string;
  slug: string;
  user_id: string;
  created_at: string;
  updated_at: string;
  /** Most recent deployment start across the project's apps; null if never deployed. */
  last_deployed_at: string | null;
  /** Visible and usable by every Member, not only the owner, when true. */
  shared: boolean;
}

export interface Application {
  id: string;
  project_id: string;
  name: string;
  slug: string;
  // `type` is labelled "Source" in the UI and `build_type` is labelled "Build
  // Method". The field names are kept for schema/API stability; only the
  // user-facing copy was renamed. Grep for the labels, not the field names,
  // when hunting for the UI.
  type: "git" | "image";
  source_repo: string | null;
  source_image: string | null;
  dockerfile_path: string;
  // Subdirectory of the repo to build from — detection, the Dockerfile path,
  // and the Dockerfile build context all resolve relative to this. Git
  // source only; null/empty = the repo root (today's only behavior).
  root_directory: string | null;
  build_type: "dockerfile" | "buildpacks" | "railpack" | "image";
  build_type_override: string | null;
  builder_image: string | null;
  cpu_limit: number;
  memory_limit: number;
  // The port the container listens on. Null = unset: the health check then
  // takes the newest domain's port and the proxy a domain's own, both falling
  // back to 8080. See resolvePorts.
  container_port: number | null;
  status: string;
  auto_deploy_branch: string | null;
  // Secrets never cross the wire. These say only whether one is set; the
  // values come from audited reveal endpoints.
  has_webhook_secret: boolean;
  has_git_credentials: boolean;
  deploy_hook_enabled: boolean;
  health_check_path: string | null;
  // "http" (control-plane probe of a path), "command" (Docker HEALTHCHECK run
  // in the container, continuous, drives status), or "none".
  health_check_type: "none" | "http" | "command";
  health_check_command: string | null;
  health_check_expect_status: number | null;
  health_check_interval_seconds: number | null;
  health_check_retries: number | null;
  health_check_start_period_seconds: number | null;
  health_check_timeout_seconds: number | null;
  parent_application_id: string | null;
  branch: string | null;
  preview_branch_pattern: string | null;
  preview_domain_template: string | null;
  last_activity_at: string;
  last_deployed_at: string | null;
  // What it would take to make the running container match the saved config.
  // Derived server-side so the list and the detail page cannot disagree, and
  // suppressed until the app has deployed once. The raw timestamps are for
  // "changed 5m ago" detail only — branch on `pending_change`.
  pending_change: "" | "config" | "source";
  config_changed_at: string | null;
  source_changed_at: string | null;
  source_kind: string | null;
  source_ref: string | null;
  // Set when the repository comes from a connected git account rather than a
  // plain URL.
  git_integration_id: string | null;
  readonly_rootfs: boolean;
  container_caps: "minimal" | "standard";
  created_at: string;
  updated_at: string;
}

// Deploy hook state. `token` and `path` are only populated by generate and
// reveal — the status read reports `enabled` alone.
export interface DeployHook {
  enabled: boolean;
  path?: string;
  token?: string;
}

export interface Deployment {
  id: string;
  application_id: string;
  status: "pending" | "building" | "deploying" | "success" | "failed";
  triggered_by:
    | "push"
    | "manual"
    | "api"
    | "rollback"
    | "reload"
    | "rebuild"
    | "template"
    | "hook";
  commit_sha: string | null;
  // Provenance from the push webhook payload; null for deploys with no
  // upstream commit (manual, reload, rebuild, template, deploy hook).
  commit_message: string | null;
  commit_author: string | null;
  build_logs: string;
  error_message: string | null;
  image_tag: string | null;
  started_at: string;
  build_started_at: string | null;
  build_ended_at: string | null;
  deploy_started_at: string | null;
  finished_at: string | null;
}

export interface ApplicationVolume {
  id: string;
  name: string;
  mount_path: string;
  size_bytes: number;
  created_at: string;
}

export interface VolumeBackupConfig {
  id: string;
  application_volume_id: string;
  destination_id: string;
  prefix: string;
  schedule: string;
  timezone: string;
  keep_latest?: number;
  enabled: boolean;
  quiesce: boolean;
  last_run_at?: string;
  created_at: string;
}

export interface VolumeBackup {
  id: string;
  status: "running" | "succeeded" | "failed";
  size_bytes: number;
  has_remote: boolean;
  config_id?: string;
  started_at: string;
  finished_at?: string;
  error?: string;
  log?: string;
}

export interface AppVolumeBackupConfig extends VolumeBackupConfig {
  volume_name: string;
  mount_path: string;
}

export interface VolumeRestore {
  id: string;
  backup_id?: string;
  status: "running" | "succeeded" | "failed";
  started_at: string;
  finished_at?: string;
  error?: string;
  log?: string;
}

export interface FileMount {
  id: string;
  mount_path: string;
  /** Decrypted content for non-secret mounts; empty when content_masked. */
  content?: string;
  is_secret: boolean;
  file_mode: string;
  /** True when the mount holds secret content masked in this response. */
  content_masked: boolean;
  created_at: string;
}

export interface Domain {
  id: string;
  application_id: string;
  hostname: string;
  ssl_enabled: boolean;
  verified_at: string | null;
  created_at: string;
}

export interface EnvVar {
  id: string;
  key: string;
  // Absent for a secret row — the list endpoint never sends a secret's value
  // (real or masked); fetch it via the reveal endpoint instead.
  value?: string;
  is_secret: boolean;
  created_at: string;
  updated_at: string;
}

export interface EnvVarInput {
  key: string;
  value: string;
  is_secret: boolean;
  // True for a saved secret the editor never touched: value still holds the
  // "••••••••" list mask, not the real secret, so the backend must reuse the
  // stored ciphertext for this key instead of encrypting value.
  unchanged?: boolean;
}

export interface MetricsOverview {
  projects: number;
  applications: number;
  databases: number;
  deployments: number;
  containers: {
    running: number;
    stopped: number;
    error: number;
    total: number;
    /** Per-category running/total; categories the platform models (application, database). */
    by_type: Record<string, { running: number; total: number }>;
  };
}

export interface ServiceMetrics {
  cpu_percent: number;
  memory_used: number;
  memory_limit: number;
  uptime_seconds: number;
  status: string;
  domain?: string;
  port?: number;
}

/** Per-service runtime snapshot keyed by application id. */
export type ProjectMetrics = Record<string, ServiceMetrics>;

export interface ServerService {
  name: string;
  description: string;
  status: "running" | "error";
}

export interface ServerServices {
  healthy: number;
  total: number;
  services: ServerService[];
}

export interface HostMetricPoint {
  cpu_percent: number | null;
  memory_used: number | null;
  memory_total: number | null;
  disk_used: number | null;
  disk_total: number | null;
  // Swap is shown as its own series, never summed with RAM. swap_total is 0 on
  // hosts with no swap configured.
  swap_used: number | null;
  swap_total: number | null;
  recorded_at: string;
}

export interface AppMetricPoint {
  cpu_percent: number | null;
  memory_usage: number | null;
  memory_limit: number | null;
  network_rx_bytes: number | null;
  network_tx_bytes: number | null;
  recorded_at: string;
}

export interface SettingEntry {
  key: string;
  value: string;
}

export interface GlobalDeployment {
  id: string;
  application_id: string;
  status: string;
  triggered_by: string;
  commit_sha: string | null;
  commit_message: string | null;
  commit_author: string | null;
  build_logs: string | null;
  error_message: string | null;
  image_tag: string | null;
  started_at: string;
  finished_at: string | null;
  application_name: string;
  application_slug: string;
  project_id: string;
  project_name: string;
}

export interface RequestLog {
  id: string;
  application_id: string;
  method: string;
  path: string;
  status_code: number;
  latency_ms: number;
  hostname: string;
  request_size: number | null;
  response_size: number | null;
  client_ip: string | null;
  user_agent: string | null;
  recorded_at: string;
}

export interface ContainerLog {
  id: string;
  source_type: "application" | "database";
  source_id: string;
  level: "debug" | "info" | "warning" | "error";
  stream: "stdout" | "stderr";
  message: string;
  recorded_at: string;
  container_id: string | null;
  deployment_id: string | null;
}

// A log "session": one container generation that produced log lines, or the
// NULL bucket for rows collected before sessions existed. Keyed by container so
// databases get sessions too — they have no deployment, but are replaced by a
// new container on a version upgrade. deployment_id is only for labelling.
export interface ContainerLogSession {
  container_id: string | null;
  deployment_id: string | null;
  first_at: string;
  last_at: string;
  line_count: number;
  triggered_by: string | null;
  status: string | null;
  commit_sha: string | null;
  started_at: string | null;
}

export interface Database {
  id: string;
  project_id: string;
  type: "postgres" | "mysql" | "redis" | "mongo" | "other";
  name: string;
  slug: string;
  version: string;
  status: string;
  internal_host: string;
  internal_port: number;
  cpu_limit: number;
  memory_limit: number;
  host_port: number | null;
  // "other" type only:
  image: string | null;
  container_port: number | null;
  data_dir: string | null;
  backup_mode: "none" | "volume_snapshot" | "command";
  backup_command: string | null;
  restore_command: string | null;
  source_kind: string | null;
  source_ref: string | null;
  created_at: string;
  volume?: { name: string; size_bytes: number };
  external_access?: {
    enabled: boolean;
    host_port?: number;
    ssh_host?: string;
    ssh_user?: string;
  };
  // True when the managed container has been removed from the host while the
  // record still exists — the case Restart/Start can't recover from. Only set in
  // the steady non-running states; the UI surfaces Reload to recreate it.
  container_missing?: boolean;
}

// Live connection credentials, fetched separately from Database — the
// backend requires a session for this one (a decrypted password, not just
// view-only project data), so it's never part of the main database fetch.
export interface DatabaseCredentials {
  credentials?: Record<string, string>;
  connection_string?: string;
}

export interface DatabaseBackup {
  id: string;
  status: "running" | "succeeded" | "failed";
  size_bytes: number;
  has_remote: boolean;
  remote_key?: string;
  config_id?: string;
  target_database?: string;
  started_at: string;
  finished_at?: string;
  error?: string;
  log?: string;
}

export type BackupProvider =
  | "s3"
  | "r2"
  | "b2"
  | "wasabi"
  | "minio"
  | "other"
  | "local";

export interface BackupDestination {
  id: string;
  project_id: string;
  name: string;
  provider: BackupProvider;
  endpoint: string;
  region: string;
  bucket: string;
  prefix: string;
  use_ssl: boolean;
  created_at: string;
  updated_at: string;
}

export interface DatabaseRestore {
  id: string;
  backup_id?: string;
  status: "running" | "succeeded" | "failed";
  started_at: string;
  finished_at?: string;
  error?: string;
  log?: string;
}

export interface DatabaseBackupConfig {
  id: string;
  database_id: string;
  destination_id: string;
  prefix: string;
  schedule: string;
  timezone: string;
  keep_latest?: number;
  enabled: boolean;
  databases: string[]; // specific databases to back up; empty = all
  all_databases: boolean;
  last_run_at?: string;
  created_at: string;
  updated_at: string;
}

export interface ProjectBackupActivity extends DatabaseBackup {
  kind: "database" | "volume";
  resource_id: string;
  resource_name: string;
  app_name?: string; // volume rows only: the owning application's name
}

export interface RouteFeature {
  id: string;
  domain_id: string;
  feature_type:
    | "basic_auth"
    | "redirect"
    | "headers"
    | "ip_allowlist"
    | "rate_limit";
  config: Record<string, unknown>;
  enabled: boolean;
  created_at: string;
  updated_at: string;
}

/** What the server last observed on the wire for a domain — not what its
 *  configuration says should happen. */
export type TLSStatus =
  | "unknown"
  | "disabled"
  | "pending"
  | "active"
  | "expiring"
  | "expired"
  | "failed"
  /** The proxy's own internal CA, when it is configured to issue that way (dev).
   *  Not the same as pending, which is that certificate when a public CA was
   *  expected and has not delivered. */
  | "local";

/** A configured feature the proxy reconciler is serving a domain WITHOUT.
 *  The API only lists one once it has persisted past a grace period, so a Caddy
 *  that is briefly unprobeable during an update never produces an entry. */
export interface NotAppliedFeature {
  feature_type: string;
  /** `module_absent`: the probe saw Caddy and the module was not there.
   *  `modules_unknown`: the probe could not read Caddy at all. */
  reason: "module_absent" | "modules_unknown";
  since: string;
}

export interface DomainExpanded extends Domain {
  /** The public URL prefix this domain answers on; "/" means the whole host. */
  path: string;
  /** Whether the prefix is removed before the app sees the request. */
  strip_path: boolean;
  /** Prefix prepended after the strip, for an app that insists on a base path. */
  internal_path: string;
  container_port?: number | null;
  force_https: boolean;
  ssl_mode: string;
  ssl_provider?: string | null;
  certificate_id?: string | null;
  /** The DNS provider issuing this domain's certificate (ssl_mode dns_challenge). */
  dns_provider_id?: string | null;
  advanced_config?: unknown;
  /** The API names this `route_features` (the SQL alias is returned verbatim).
   *  It was declared here as `features`, so every read was silently undefined:
   *  the domain row never showed its feature count and the features list always
   *  said "none configured" — for features that existed and were live in Caddy. */
  route_features?: RouteFeature[];
  /** Absent unless something is being withheld; see NotAppliedFeature. */
  not_applied?: NotAppliedFeature[];
  tls_status?: TLSStatus;
  tls_issuer?: string | null;
  tls_not_after?: string | null;
  tls_last_checked_at?: string | null;
  tls_error?: string | null;
  /** A suspicion that explains a pending domain; never a verdict on it. */
  tls_advisory?: string | null;
}

export interface AuditLog {
  id: string;
  user_id: string | null;
  action: string;
  resource_type: string;
  resource_id: string | null;
  resource_name?: string;
  details: Record<string, unknown> | null;
  ip_address: string | null;
  created_at: string;
  user_email?: string;
}

export interface Invitation {
  id: string;
  email: string;
  role: string;
  invited_by_user_id: string;
  expires_at: string;
  accepted_at: string | null;
  created_at: string;
}

export interface BackupRun {
  id: string;
  started_at: string;
  finished_at: string | null;
  status: "running" | "succeeded" | "failed";
  remote_key: string | null;
  size_bytes: number;
  error: string | null;
  log: string;
  encrypted: boolean;
}

export interface BackupRemoteConfig {
  endpoint: string;
  region: string;
  bucket: string;
  prefix: string;
  use_ssl: boolean;
}

export interface BackupStatus {
  last_succeeded_at: string | null;
  last_attempted_at: string | null;
  last_error: string | null;
  remote_enabled: boolean;
  remote: BackupRemoteConfig | null;
  retention: { days: number; count: number };
  encryption_enabled: boolean;
  /** age public key backups are encrypted to — safe to display, never a secret. */
  encryption_recipient: string | null;
}

export interface AlertPreferences {
  deploy_failures: boolean;
  deploy_success: boolean;
  build_failures: boolean;
  quota_threshold: boolean;
  quota_threshold_percent: number;
}

export interface Notification {
  id: string;
  user_id: string;
  type: string;
  title: string;
  body: string;
  link: string | null;
  read: boolean;
  created_at: string;
}

// "write" grants "read" and "deploy" too, and "read" grants "metrics" —
// deploy and metrics are narrower, independent carve-outs, not steps on a
// ladder. See middleware.scopeGrants (apps/api) for the authoritative table.
export type TokenScope = "read" | "write" | "deploy" | "metrics";

export interface ApiToken {
  id: string;
  name: string;
  scopes: TokenScope[];
  role_at_issue: "admin" | "member";
  /** True when this token is narrowed to project_ids. Never derive this from
   *  project_ids.length — a pinned token can legitimately end up with zero
   *  reachable projects (every one it was pinned to got deleted), and that
   *  must read as "pinned to nothing," not "unpinned." */
  pinned: boolean;
  /** Every project this token is pinned to. Always present (never null),
   *  empty when pinned is false. Unpinned reaches every project the owner
   *  can reach, evaluated at use time. */
  project_ids: string[];
  expires_at: string | null;
  last_used_at: string | null;
  created_at: string;
}

// Only the create response carries the plaintext, and only once.
export interface CreatedApiToken extends ApiToken {
  token: string;
}

export interface HostResources {
  cpu_percent: number;
  memory_used: number;
  memory_total: number;
  disk_used: number;
  disk_total: number;
  recorded_at: string;
}

export interface Stats {
  is_admin: boolean;
  // Exhaustive fleet breakdown — the buckets always sum to total. `busy` is the
  // residual (a database creating / upgrading / backing up), so a service can
  // never fall out of the card entirely.
  app_health: {
    running: number;
    errored: number;
    stopped: number;
    unhealthy: number;
    inactive: number;
    busy: number;
    total: number;
  };
  deploy_7d: {
    succeeded: number;
    failed: number;
    total: number;
    median_build_ms: number;
  };
  // Disjoint buckets — each affected service is counted once, so they sum to
  // total and one broken service is never reported as two issues.
  needs_attention: {
    unhealthy_services: number;
    failed_deploys: number;
    error_services: number;
    failed_backups: number;
    // Scheduled tasks whose latest run failed — tasks, not runs.
    failed_tasks: number;
    total: number;
  };
  host: HostResources | null;
  /**
   * Configuration findings — accepted but inadvisable settings. Deliberately
   * separate from needs_attention, which counts failing workloads: folding a
   * config finding into that number would corrupt it. Admin-only, so absent
   * for members.
   */
  config_warnings?: ConfigWarning[];
}

export interface ConfigWarning {
  code: string;
  message: string;
  remedy: string;
}

// ---- Docker inspect (read-only admin pages) --------------------------------

/** Links a Docker resource back to the platform app/database that owns it. */
export interface DockerOwner {
  type: "application" | "database";
  id: string;
  name: string;
  project_id: string;
}

export interface DockerSystemInfo {
  server_version: string;
  operating_system: string;
  os_type: string;
  architecture: string;
  kernel_version: string;
  storage_driver: string;
  logging_driver: string;
  cgroup_driver: string;
  ncpu: number;
  mem_total: number;
  docker_root_dir: string;
  name: string;
  containers: number;
  containers_running: number;
  containers_paused: number;
  containers_stopped: number;
  images: number;
}

export interface DockerDiskUsageEntry {
  count: number;
  size: number;
  reclaimable: number;
}

export interface DockerDiskUsage {
  layers_size: number;
  images: DockerDiskUsageEntry;
  containers: DockerDiskUsageEntry;
  volumes: DockerDiskUsageEntry;
  build_cache: DockerDiskUsageEntry;
}

export interface DockerOverview {
  info: DockerSystemInfo;
  /** Null while it is still being computed: `docker system df` is far too slow
   *  to run inside the request (33s on a small VPS), so it is refreshed in the
   *  background and arrives on a later poll rather than failing the page. */
  disk_usage: DockerDiskUsage | null;
  counts: {
    containers_running: number;
    containers_total: number;
    images: number;
    volumes: number;
  };
}

export interface DockerContainer {
  id: string;
  name: string;
  image: string;
  status: string;
  ports: Record<string, string>;
  managed: boolean;
  owner?: DockerOwner;
  created_at: string;
}

export interface DockerImage {
  id: string;
  repo_tags: string[] | null;
  size: number;
  shared_size: number;
  containers: number;
  dangling: boolean;
  managed: boolean;
  owner?: DockerOwner;
  created_at: string;
}

export interface DockerVolume {
  name: string;
  driver: string;
  mountpoint: string;
  scope: string;
  size: number;
  ref_count: number;
  managed: boolean;
  kind: "" | "data" | "cache";
  owner?: DockerOwner;
  created_at: string;
}

export interface DockerNetworkContainer {
  id: string;
  name: string;
  ipv4_address: string;
}

// ---- Maintenance (Server page) ---------------------------------------------

export interface ReconcilerStatus {
  interval_seconds: number;
  last_run_at: string;
  last_duration_ms: number;
  last_added: number;
  last_removed: number;
  last_error?: string;
  run_count: number;
  total_drift: number;
  last_updated: number;
  /** Routes served without a feature because the running Caddy lacks its module. */
  degraded?: ProxyDegradation[];
  /** The 3s probe's reading of Caddy's admin API. "unknown" = not probed yet. */
  admin_state: CaddyAdminState;
  caddy: CaddyPanel;
}

export interface ProxyDegradation {
  hostname: string;
  path: string;
  feature: string;
  module: string;
  /** module_absent = this Caddy lacks it; modules_unknown = Belune could not ask. */
  reason: "module_absent" | "modules_unknown";
  since: string;
}

export type CaddyAdminState =
  | "unknown"
  | "unreachable"
  | "healthy"
  | "lost_config";

/** Every nullable or "unknown" value here means "could not be determined",
 *  which is NOT the same as absent — render it as unknown, never as none. */
export interface CaddyPanel {
  discovery: {
    source: "env" | "label" | "assumed" | "";
    container_name: string;
  };
  admin: { url: string; state: CaddyAdminState };
  container: {
    state: "found" | "not_found" | "unknown";
    status?: string;
  };
  version: {
    value: string | null;
    upstream: string | null;
    source: "label" | "exec" | null;
  };
  image: string | null;
  modules: { known: boolean; ids: string[] };
  features: Record<string, { module: string; state: FeatureAvailability }>;
  /** Modules no route feature uses yet; same three states as `features`. */
  optional_modules: Record<
    "crowdsec" | "waf" | "layer4",
    { module: string; state: FeatureAvailability }
  >;
  access_log: AccessLogStatus;
}

export type AccessLogState =
  | "unknown"
  | "missing"
  | "unreadable"
  | "error"
  | "starting"
  | "no_traffic"
  | "no_app_traffic"
  | "ingesting";

export interface AccessLogStatus {
  path: string;
  resolved_path: string;
  file: "present" | "missing" | "unreadable" | "";
  state: AccessLogState;
  /** When counting began; the counters below are relative to it. */
  since: string;
  lines_read: number;
  rows_inserted: number;
  last_line_at?: string;
  last_insert_at?: string;
  last_error?: string;
  last_error_at?: string;
}

export type ProxyTrafficState =
  | "ok"
  | "collecting"
  | "no_data"
  | "disabled"
  | "unreachable"
  | "unknown";

/** Traffic through Caddy's reverse_proxy handler. A null number means it could
 *  not be determined (or, for latency, that nothing completed) — never zero. */
export interface ProxyTraffic {
  state: ProxyTrafficState;
  error?: string;
  in_flight: number | null;
  proxied_total: number | null;
  window_seconds: number | null;
  requests_per_second: number | null;
  server_errors_per_second: number | null;
  mean_seconds: number | null;
  p50_seconds: number | null;
  p95_seconds: number | null;
  status_classes: Record<string, number> | null;
}

export interface ProxyConfig {
  /** How many secret values the server replaced with a marker. */
  redacted_fields: number;
  config: unknown;
}

/** Whether the running Caddy has the module a route feature needs. "unknown"
 *  means the probe could not ask — not that the module is missing. */
export type FeatureAvailability = "available" | "unavailable" | "unknown";

export interface ProxyCapabilities {
  /** False when the probe could not read the running Caddy's modules. */
  known: boolean;
  /** Only the feature types that need an optional module appear here. */
  features: Record<string, { module: string; state: FeatureAvailability }>;
}

/** Why the WAF is switched on but not active. The first two are the probe's two
 *  different answers, and must not be collapsed: `modules_unknown` means Belune
 *  could not ask Caddy, not that the module is missing. */
export type WafReason = "module_absent" | "modules_unknown" | "no_applications";

export interface WafState {
  /** The WAF route is in Caddy now. */
  active: boolean;
  /** Application hostnames it inspects. */
  hosts: number;
  reason?: WafReason;
  /** The last refusal, when Caddy would not take the route. */
  error?: string;
}

/** What the recorder has done since Belune started. Dropped and malformed are
 *  the two ways a detection is lost without leaving a row. */
export interface WafRecorderStats {
  since: string;
  recorded: number;
  dropped: number;
  malformed: number;
  last_error?: string;
  last_error_at?: string;
}

export interface WafTopRule {
  rule_id: number;
  message: string;
  category: string;
  events: number;
}

export interface WafTopClient {
  client_ip: string;
  events: number;
  last_seen: string;
}

/** The periods the WAF summary can cover. Deliberately not the dashboard-wide
 *  `TimeRange`: events are swept at `retention_days` (7 by default), so a 30d
 *  option would promise a month of history that is never there. */
export type WafWindow = "1h" | "24h" | "7d" | "all";

export interface WafSummary {
  window: WafWindow;
  /** The same period in hours, 0 for "all". */
  window_hours: number;
  events: number;
  requests: number;
  clients: number;
  /** Every event still kept, whatever the window — so an empty window can say
   *  "nothing lately" instead of looking like "nothing ever". */
  total: number;
  top_rules: WafTopRule[];
  top_clients: WafTopClient[];
}

/** How an exclusion narrows what it ignores. */
export type WafExclusionShape = "target" | "path" | "rule";

/** A place the WAF rule set has been told to stand down: a security downgrade. */
export interface WafExclusion {
  id: number;
  rule_id: number;
  shape: WafExclusionShape;
  /** Both set for a target, both empty otherwise. */
  collection: string;
  target_name: string;
  /** Rooted; a trailing * means "begins with". */
  path_pattern: string;
  scope: "install" | "application";
  application_id: string | null;
  application_name: string;
  /** The application's domains now. They are not stored with the exclusion. */
  hosts: string[];
  /** Scoped to an application with no domains: it applies to nothing. */
  inert: boolean;
  note: string;
  created_at: string;
}

export interface WafExclusionsPage {
  exclusions: WafExclusion[];
  /** The most the proxy will render. */
  max: number;
  /** The request fields a target exclusion may name. */
  collections: string[];
}

export interface WafExclusionInput {
  rule_id: number;
  collection?: string;
  target_name?: string;
  path_pattern?: string;
  /** Absent or empty is install-wide. */
  application_id?: string;
  /** Required when the whole rule is turned off. */
  note?: string;
}

/** A trusted range that overlaps a Docker network Caddy is attached to. */
export interface TrustedProxyOverlap {
  cidr: string;
  network: string;
  subnet: string;
}

/** A published set of proxy ranges: a snapshot, never applied by default. */
export interface TrustedProxyPreset {
  id: string;
  name: string;
  cidrs: string[];
  /** Where to re-check the list. */
  source: string;
  /** The date the list was copied (YYYY-MM-DD). */
  checked_at: string;
}

/** An address range of a Docker network Caddy is attached to: what a trusted range must not cover. */
export interface TrustedProxyNetwork {
  name: string;
  subnet: string;
}

export interface TrustedProxies {
  cidrs: string[];
  overlaps: TrustedProxyOverlap[];
  /** What the overlap is judged against, so a form can warn before it saves. */
  networks: TrustedProxyNetwork[];
  /** False when Belune could not list its Docker networks: "no overlaps" is then
   *  not an answer. */
  overlaps_checked: boolean;
  presets: TrustedProxyPreset[];
}

export interface WafStatus {
  capability: { module: string; state: FeatureAvailability };
  /** The operator's switch. */
  enabled: boolean;
  mode: "detection_only";
  state: WafState;
  recorder: WafRecorderStats;
  retention_days: number;
  max_events: number;
  summary: WafSummary;
  /** Detections silenced on purpose: without this, fewer of them read as a broken
   *  WAF or a clean application. */
  exclusions: { total: number; inert: number };
}

/** Coraza reports "unknown" for a rule that declares no severity, which includes
 *  the anomaly-score rule that says a request would have been blocked. */
export type WafSeverity =
  | "emergency"
  | "alert"
  | "critical"
  | "error"
  | "warning"
  | "notice"
  | "info"
  | "debug"
  | "unknown";

export interface WafEvent {
  id: number;
  recorded_at: string;
  client_ip: string;
  rule_id: number;
  severity: WafSeverity;
  category: string | null;
  message: string;
  /** Null when attribution failed for this event. */
  host: string | null;
  application_id: string | null;
  /** Shared by the rules one request tripped. */
  tx_id: string | null;
  /**
   * The request behind this detection, joined from the request log by `tx_id`.
   * Null is normal, not an error: a detection outlives the request log once
   * either retention is lowered, and one that never carried a `tx_id` has
   * nothing to join.
   */
  request: WafRequestDetail | null;
  /**
   * The request variable the rule matched in, as a name — "REQUEST_COOKIES:session",
   * "ARGS:q" — and never the value found in it. Null for a rule that matches on the
   * request as a whole, and for detections recorded before Belune kept it.
   *
   * ⚠️ A hint, not evidence: the text it is read from can be influenced by the
   * request, so the name can be forged. Never key a decision on it.
   */
  matched_target: string | null;
  /**
   * What Coraza did on THIS rule's line: "blocked" for the rule that denied the
   * request, "detected" for one that only reported. Null for detections recorded
   * before 000077 and for a line whose opening was neither.
   *
   * ⚠️ Not whether the request was blocked. The signature rules of a blocked
   * request read "detected"; that is `request_blocked`.
   */
  disposition: "blocked" | "detected" | null;
  /**
   * Whether any rule of this row's transaction denied the request. False is
   * "not known to have been blocked", which is also what an older detection reads.
   */
  request_blocked: boolean;
  /** The total the scoring rule (949110) reported; null for every other rule. */
  anomaly_score: number | null;
}

/**
 * What Belune keeps of the request behind a detection. Deliberately not the
 * matched payload and not the headers at large — the URI arrives already
 * redacted from Caddy's encoder, and the headers are an allowlist of two:
 * Content-Type and User-Agent. Anything said about this in the UI has to name
 * both, or it promises less than is kept.
 */
export interface WafRequestDetail {
  method: string;
  path: string;
  /** 0 when the access entry carried a status outside 100-599. */
  status: number;
  content_type: string | null;
  user_agent: string | null;
}

/**
 * One client's detections in the summary window, collapsed into a row.
 *
 * `events` and `requests` are both here on purpose: one request can trip a
 * dozen rules, so a bare "12 detections" reads as twelve attacks when it was
 * one request with twelve opinions about it.
 */
export interface WafClientGroup {
  client_ip: string;
  /** Rule matches. */
  events: number;
  /** How many requests those matches came from. */
  requests: number;
  rules: number;
  /**
   * Distinct paths, counted through the request log. 0 is normal rather than
   * wrong: a detection stores no URI of its own, so once its request log is
   * swept there is no URL left to count.
   */
  urls: number;
  last_seen: string;
  /**
   * Whether this client passed the anomaly-score threshold anywhere in the
   * window. Unlike the counts beside it, this is not narrowed by a rule filter:
   * it is a fact about the client, not about the rule being listed.
   */
  would_block: boolean;
  /**
   * How many of this client's requests were actually refused in the window. Not
   * narrowed by a rule filter, for the same reason as `would_block`. Can be
   * non-zero alongside `would_block`: a client refused today may only have been
   * reported last week, before blocking was switched on.
   */
  blocked_requests: number;
}

export interface WafClientsPage {
  window: WafWindow;
  clients: WafClientGroup[];
  /** More clients matched than are listed. There is no page after this one. */
  has_more: boolean;
}

export interface WafEventsPage {
  events: WafEvent[];
  /** Pass as `before` for the next page; null on the last. */
  next_before: number | null;
}

/** Why CrowdSec is switched on but not active. `lapi_unavailable` means the
 *  CrowdSec container is not answering; the first two are the Caddy probe's two
 *  different answers and must not be collapsed. */
export type CrowdSecReason =
  | "module_absent"
  | "modules_unknown"
  | "no_applications"
  | "lapi_unavailable";

export interface CrowdSecState {
  /** The bouncer route is in Caddy now. */
  active: boolean;
  /** Application hostnames it covers. */
  hosts: number;
  reason?: CrowdSecReason;
  /** The last refusal, when Caddy would not take the config. */
  error?: string;
}

/** What the last look at the CrowdSec container found. */
export interface CrowdSecLocalApi {
  checked_at: string;
  reachable: boolean;
  bouncer_registered: boolean;
  /** When the bouncer last fetched the ban list. Absent if it never has. */
  last_pull?: string;
  error?: string;
}

export interface CrowdSecPolicy {
  enabled: boolean;
  threshold: number;
  window_minutes: number;
  duration_minutes: number;
  max_active: number;
  categories: string[];
}

export interface CrowdSecAutobanStatus {
  last_run?: string;
  last_error?: string;
  /** Automatic bans in force now. */
  active: number;
  /** Bans issued since Belune started. */
  banned: number;
  /** Addresses over the threshold the last run left alone. */
  skipped: number;
  /** The last run stopped at the active-ban cap. */
  capped: boolean;
}

/** `unbannable`: private or loopback, no ban can name it. `allowlisted`: public,
 *  covered by an unexpired allowlist entry. `exposed`: public, and nothing covers
 *  it. `unknown`: the local API could not be asked. */
export type CrowdSecProtection =
  | "unbannable"
  | "allowlisted"
  | "exposed"
  | "unknown";

export interface CrowdSecStatus {
  capability: { module: string; state: FeatureAvailability };
  /** The operator's switch. */
  enabled: boolean;
  state: CrowdSecState;
  local_api: CrowdSecLocalApi;
  /** The address this request came from, as Caddy saw it. */
  your_ip: string;
  /** How that address stands against being banned. Not the same as whether the
   *  switch is on: a private address is never added to the allowlist, and a public
   *  one is only as safe as its (expiring) entry. */
  your_ip_protection: CrowdSecProtection;
  autoban: {
    policy: CrowdSecPolicy;
    status: CrowdSecAutobanStatus;
    /** The WAF rule categories a policy may choose from. */
    categories: string[];
  };
}

/** What created a ban. */
export type CrowdSecSource = "dashboard" | "waf" | "cli" | "crowdsec";

export interface CrowdSecDecision {
  id: number;
  value: string;
  scope: "ip" | "range";
  source: CrowdSecSource;
  reason: string;
  created_at: string;
  expires_at: string;
}

export interface CrowdSecDecisions {
  decisions: CrowdSecDecision[];
}

export interface CrowdSecAllowItem {
  value: string;
  description: string;
  created_at: string;
  /** Absent for an entry that never expires. */
  expires_at?: string;
}

export interface CrowdSecAllowlist {
  items: CrowdSecAllowItem[];
}

export interface QueueDepth {
  queue: string;
  pending: number;
  active: number;
  retry: number;
  archived: number;
}

export interface QueueStatus {
  queues: QueueDepth[];
  total_stuck: number;
}

export interface DockerNetwork {
  id: string;
  name: string;
  driver: string;
  scope: string;
  internal: boolean;
  managed: boolean;
  containers: DockerNetworkContainer[] | null;
  created_at: string;
}

export type ScheduledTaskRunStatus =
  | "running"
  | "succeeded"
  | "failed"
  | "skipped"
  | "timed_out";

export interface ScheduledTaskRun {
  id: string;
  trigger: "schedule" | "manual";
  status: ScheduledTaskRunStatus;
  exit_code?: number;
  output?: string;
  skip_reason?: string;
  started_at: string;
  finished_at?: string;
}

export interface ScheduledTask {
  id: string;
  application_id: string;
  name: string;
  command: string;
  cron: string;
  timezone: string;
  enabled: boolean;
  timeout_seconds: number;
  keep_runs: number;
  last_run_at?: string;
  // RFC 3339 with the task's own UTC offset.
  next_run_at?: string;
  latest_run?: ScheduledTaskRun;
  created_at: string;
  updated_at: string;
}
