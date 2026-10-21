import type {
  ProxyConfig,
  ProxyTraffic,
  QueueStatus,
  ReconcilerStatus,
} from "@/lib/types";
import { api } from "./client";

export type CleanupAction =
  | "deployments"
  | "images"
  | "volumes"
  | "containers"
  | "build_cache";

// Empty/omitted actions = full cleanup (all steps).
export function runCleanup(actions?: CleanupAction[]) {
  return api.post<{ status: string }>(
    "/maintenance/cleanup",
    actions && actions.length ? { actions } : {},
  );
}

export function getReconcilerStatus() {
  return api.get<ReconcilerStatus>("/maintenance/proxy");
}

export function getProxyMetrics() {
  return api.get<ProxyTraffic>("/maintenance/proxy/metrics");
}

export function getProxyConfig() {
  return api.get<ProxyConfig>("/maintenance/proxy/config");
}

export function reconcileProxy() {
  return api.post<ReconcilerStatus>("/maintenance/proxy/reconcile");
}

export function getQueueStatus() {
  return api.get<QueueStatus>("/maintenance/queue");
}

export function clearQueue() {
  return api.post<{ cleared: number }>("/maintenance/queue/clear");
}

export function clearPendingQueue() {
  return api.post<{ cleared: number }>("/maintenance/queue/clear-pending");
}

export type PlatformService =
  | "belune"
  | "caddy"
  | "redis"
  | "postgres"
  | "buildkit";

export function getPlatformLogs(service: PlatformService) {
  return api.get<{ service: string; content: string }>(
    `/maintenance/logs?service=${encodeURIComponent(service)}`,
  );
}

export type ServerIPSource = "manual" | "env" | "detected" | "unknown";

export function getServerIP() {
  return api.get<{ effective: string; source: ServerIPSource }>(
    "/maintenance/server-ip",
  );
}

/** Step-up re-auth for host root. A code is required of anyone with a second
 *  factor enabled — the password alone defends against a hijacked session, not
 *  a stolen password. */
export function createHostShellSession(password: string, code?: string) {
  return api.post<{ session_id: string }>("/maintenance/host-shell", {
    password,
    ...(code ? { method: "totp", code } : {}),
  });
}

export type RestartableService = "caddy" | "redis";

export function restartService(service: RestartableService) {
  return api.post<{ status: string; service: string }>(
    `/maintenance/restart?service=${encodeURIComponent(service)}`,
  );
}

/** Step-up re-auth to apply the update the Server-page card is showing — same
 *  shape as the host-shell gate: a code is required of anyone with a second
 *  factor enabled. Triggers a detached helper that replaces this container, so
 *  a successful call is normally followed by the dashboard's connection
 *  dropping briefly. */
export function triggerSelfUpdate(password: string, code?: string) {
  return api.post<{ status: string; target: string }>("/maintenance/update", {
    password,
    ...(code ? { method: "totp", code } : {}),
  });
}

/** What became of the last update this dashboard started. Polled while one is
 *  in flight, because POST /maintenance/update answers as soon as the helper
 *  container is CREATED — a helper that dies on its first line would otherwise
 *  leave the UI claiming an update had started, forever. */
export type SelfUpdateStatus = {
  state: "idle" | "running" | "failed";
  target?: string;
  started_at?: string;
  reason?: string;
};

export function getSelfUpdateStatus() {
  return api.get<SelfUpdateStatus>("/maintenance/update/status");
}

/** Run the manifest check now instead of waiting for the daily sweep. 202 —
 *  the worker fetches after this returns, so poll update_last_checked_at to
 *  know when the cached values actually moved. */
export function triggerUpdateCheck() {
  return api.post<{ status: string }>("/maintenance/update/check", {});
}
