import type { LogLevel } from "@/lib/logs/level";
import type { ContainerLog, ContainerLogSession } from "@/lib/types";
import { api } from "./client";

export type ContainerLogSource = "application" | "database";

export interface ContainerLogParams {
  limit?: number;
  offset?: number;
  q?: string;
  level?: LogLevel | "";
  stream?: "stdout" | "stderr" | "";
  since?: string;
  until?: string;
  // A container id to isolate one session, or "none" for the unassigned
  // (earlier) bucket. Undefined means all sessions.
  session?: string;
  // Only lines from this process type, across container generations.
  processType?: string;
}

// Both application and database container logs share the same history endpoint
// shape, differing only in the resource segment of the path.
function basePath(
  source: ContainerLogSource,
  projectId: string,
  sourceId: string,
) {
  const resource = source === "database" ? "databases" : "applications";
  return `/projects/${projectId}/${resource}/${sourceId}/logs/history`;
}

export function listContainerLogs(
  source: ContainerLogSource,
  projectId: string,
  sourceId: string,
  params?: ContainerLogParams,
) {
  const query = new URLSearchParams();
  if (params?.limit) query.set("limit", String(params.limit));
  if (params?.offset) query.set("offset", String(params.offset));
  if (params?.q) query.set("q", params.q);
  if (params?.level) query.set("level", params.level);
  if (params?.stream) query.set("stream", params.stream);
  if (params?.since) query.set("since", params.since);
  if (params?.until) query.set("until", params.until);
  if (params?.session) query.set("session", params.session);
  if (params?.processType) query.set("process_type", params.processType);
  const qs = query.toString();
  return api.get<ContainerLog[]>(
    `${basePath(source, projectId, sourceId)}${qs ? `?${qs}` : ""}`,
  );
}

export function listContainerLogSessions(
  source: ContainerLogSource,
  projectId: string,
  sourceId: string,
) {
  const resource = source === "database" ? "databases" : "applications";
  return api.get<ContainerLogSession[]>(
    `/projects/${projectId}/${resource}/${sourceId}/logs/sessions`,
  );
}
