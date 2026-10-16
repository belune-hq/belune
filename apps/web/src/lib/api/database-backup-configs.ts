import type { DatabaseBackupConfig } from "@/lib/types";
import { api } from "./client";

export interface SaveBackupConfig {
  destination_id: string;
  prefix?: string;
  schedule: string;
  timezone?: string;
  keep_latest?: number | null;
  enabled?: boolean;
  databases?: string[]; // specific databases; empty = all databases (cluster)
}

export function listBackupConfigs(projectId: string, databaseId: string) {
  return api.get<DatabaseBackupConfig[]>(
    `/projects/${projectId}/databases/${databaseId}/backup-configs`,
  );
}

export function createBackupConfig(
  projectId: string,
  databaseId: string,
  data: SaveBackupConfig,
) {
  return api.post<DatabaseBackupConfig>(
    `/projects/${projectId}/databases/${databaseId}/backup-configs`,
    data,
  );
}

export function updateBackupConfig(
  projectId: string,
  databaseId: string,
  configId: string,
  data: SaveBackupConfig,
) {
  return api.put<DatabaseBackupConfig>(
    `/projects/${projectId}/databases/${databaseId}/backup-configs/${configId}`,
    data,
  );
}

export function deleteBackupConfig(
  projectId: string,
  databaseId: string,
  configId: string,
) {
  return api.delete<{ status: string }>(
    `/projects/${projectId}/databases/${databaseId}/backup-configs/${configId}`,
  );
}

export function runBackupConfig(
  projectId: string,
  databaseId: string,
  configId: string,
) {
  return api.post<{ status: string }>(
    `/projects/${projectId}/databases/${databaseId}/backup-configs/${configId}/run`,
    {},
  );
}
