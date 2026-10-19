export const queryKeys = {
  auth: {
    me: ["auth", "me"] as const,
    setup: ["auth", "setup"] as const,
    totp: ["auth", "totp"] as const,
  },
  projects: {
    all: ["projects"] as const,
    detail: (id: string) => ["projects", id] as const,
  },
  templates: {
    all: ["templates"] as const,
    detail: (id: string) => ["templates", id] as const,
  },
  applications: {
    all: (projectId: string) =>
      ["projects", projectId, "applications"] as const,
    detail: (projectId: string, applicationId: string) =>
      ["projects", projectId, "applications", applicationId] as const,
    buildCache: (projectId: string, applicationId: string) =>
      ["projects", projectId, "applications", applicationId, "cache"] as const,
    deployHook: (projectId: string, applicationId: string) =>
      [
        "projects",
        projectId,
        "applications",
        applicationId,
        "deploy-hook",
      ] as const,
  },
  deployments: {
    all: (projectId: string, applicationId: string) =>
      [
        "projects",
        projectId,
        "applications",
        applicationId,
        "deployments",
      ] as const,
    detail: (projectId: string, applicationId: string, deploymentId: string) =>
      [
        "projects",
        projectId,
        "applications",
        applicationId,
        "deployments",
        deploymentId,
      ] as const,
  },
  volumes: {
    all: (projectId: string, applicationId: string) =>
      [
        "projects",
        projectId,
        "applications",
        applicationId,
        "volumes",
      ] as const,
  },
  scheduledTasks: {
    all: (projectId: string, applicationId: string) =>
      [
        "projects",
        projectId,
        "applications",
        applicationId,
        "scheduled-tasks",
      ] as const,
    runs: (projectId: string, applicationId: string, taskId: string) =>
      [
        "projects",
        projectId,
        "applications",
        applicationId,
        "scheduled-tasks",
        taskId,
        "runs",
      ] as const,
  },
  fileMounts: {
    all: (projectId: string, applicationId: string) =>
      [
        "projects",
        projectId,
        "applications",
        applicationId,
        "file-mounts",
      ] as const,
  },
  volumeBackupConfigs: (
    projectId: string,
    applicationId: string,
    volumeId: string,
  ) =>
    [
      "projects",
      projectId,
      "applications",
      applicationId,
      "volumes",
      volumeId,
      "backup-configs",
    ] as const,
  volumeBackups: (projectId: string, applicationId: string, volumeId: string) =>
    [
      "projects",
      projectId,
      "applications",
      applicationId,
      "volumes",
      volumeId,
      "backups",
    ] as const,
  volumeRestores: (
    projectId: string,
    applicationId: string,
    volumeId: string,
  ) =>
    [
      "projects",
      projectId,
      "applications",
      applicationId,
      "volumes",
      volumeId,
      "restores",
    ] as const,
  appVolumeBackupConfigs: (projectId: string, applicationId: string) =>
    [
      "projects",
      projectId,
      "applications",
      applicationId,
      "volume-backup-configs",
    ] as const,
  domains: {
    all: (projectId: string, applicationId: string) =>
      [
        "projects",
        projectId,
        "applications",
        applicationId,
        "domains",
      ] as const,
  },
  envvars: {
    all: (projectId: string, applicationId: string) =>
      ["projects", projectId, "applications", applicationId, "env"] as const,
  },
  projectEnvvars: {
    all: (projectId: string) => ["projects", projectId, "env"] as const,
  },
  users: {
    all: ["users"] as const,
  },
  features: ["features"] as const,
  version: ["version"] as const,
  metrics: ["metrics"] as const,
  projectMetrics: (projectId: string) =>
    ["project-metrics", projectId] as const,
  serverServices: ["server-services"] as const,
  stats: ["stats"] as const,
  notifications: {
    list: ["notifications"] as const,
    unread: ["notifications", "unread"] as const,
  },
  hostMetrics: (range: string) => ["metrics", "host", range] as const,
  hostMetricsRange: (from: string, to: string) =>
    ["metrics", "host", "range", from, to] as const,
  settings: ["settings"] as const,
  databases: {
    all: (projectId: string) => ["projects", projectId, "databases"] as const,
    detail: (projectId: string, databaseId: string) =>
      ["projects", projectId, "databases", databaseId] as const,
    volume: (projectId: string, databaseId: string) =>
      ["projects", projectId, "databases", databaseId, "volume"] as const,
    credentials: (projectId: string, databaseId: string) =>
      ["projects", projectId, "databases", databaseId, "credentials"] as const,
    backups: (projectId: string, databaseId: string) =>
      ["projects", projectId, "databases", databaseId, "backups"] as const,
    restores: (projectId: string, databaseId: string) =>
      ["projects", projectId, "databases", databaseId, "restores"] as const,
    backupConfigs: (projectId: string, databaseId: string) =>
      [
        "projects",
        projectId,
        "databases",
        databaseId,
        "backup-configs",
      ] as const,
    deletionImpact: (projectId: string, databaseId: string) =>
      [
        "projects",
        projectId,
        "databases",
        databaseId,
        "deletion-impact",
      ] as const,
    // Backups whose database is gone. Project-scoped, because that is the only
    // level at which an orphan still has an owner.
    orphaned: (projectId: string) =>
      ["projects", projectId, "orphaned-backups"] as const,
  },
  backupDestinations: (projectId: string) =>
    ["projects", projectId, "backup-destinations"] as const,
  projectBackups: (projectId: string) =>
    ["projects", projectId, "project-backups"] as const,
  containerLogs: {
    history: (
      source: string,
      projectId: string,
      sourceId: string,
      params?: object,
    ) =>
      [
        "projects",
        projectId,
        source === "database" ? "databases" : "applications",
        sourceId,
        "logs",
        "history",
        params,
      ] as const,
    sessions: (source: string, projectId: string, sourceId: string) =>
      [
        "projects",
        projectId,
        source === "database" ? "databases" : "applications",
        sourceId,
        "logs",
        "sessions",
      ] as const,
  },
  globalDeployments: (params?: object) => ["deployments", params] as const,
  requestLogs: (params?: object) => ["requests", params] as const,
  requestSummary: (params?: object) => ["requests-summary", params] as const,
  auditLogs: (params?: object) => ["audit-logs", params] as const,
  auditActions: ["audit-actions"] as const,
  certificates: ["certificates"] as const,
  usableCertificates: (hostname: string) =>
    ["certificates", "usable", hostname] as const,
  notificationChannels: ["notification-channels"] as const,
  notificationEvents: ["notification-events"] as const,
  smtpSettings: ["smtp-settings"] as const,
  domainTLSStatus: ["domain-tls-status"] as const,
  proxyCapabilities: ["proxy-capabilities"] as const,
  gitProviders: ["git-providers"] as const,
  gitIntegrations: ["git-integrations"] as const,
  gitAvailableProviders: ["git-available-providers"] as const,
  routeFeatures: (projectId: string, applicationId: string, domainId: string) =>
    [
      "projects",
      projectId,
      "applications",
      applicationId,
      "domains",
      domainId,
      "features",
    ] as const,
  quotas: {
    all: ["quotas"] as const,
    detail: (scope: string, scopeId: string) =>
      ["quotas", scope, scopeId] as const,
  },
  previews: {
    all: (projectId: string, applicationId: string) =>
      [
        "projects",
        projectId,
        "applications",
        applicationId,
        "previews",
      ] as const,
  },
  backups: {
    runs: (limit: number, offset: number) =>
      ["backups", "runs", limit, offset] as const,
    status: ["backups", "status"] as const,
  },
  alertPreferences: ["alert-preferences"] as const,
  tokens: ["tokens"] as const,
  invitations: ["invitations"] as const,
  proxyReconciler: ["proxy", "reconciler"] as const,
  maintenanceQueue: ["maintenance", "queue"] as const,
  maintenancePlatformLogs: ["maintenance", "platform-logs"] as const,
  maintenanceServerIP: ["maintenance", "server-ip"] as const,
  maintenanceUpdateStatus: ["maintenance", "update-status"] as const,
  docker: {
    overview: ["docker", "overview"] as const,
    containers: ["docker", "containers"] as const,
    images: ["docker", "images"] as const,
    volumes: ["docker", "volumes"] as const,
    networks: ["docker", "networks"] as const,
  },
};
