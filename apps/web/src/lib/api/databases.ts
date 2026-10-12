import type {
  Database,
  DatabaseBackup,
  DatabaseCredentials,
  DatabaseRestore,
} from "@/lib/types";
import { api } from "./client";

export function listDatabases(projectId: string) {
  return api.get<Database[]>(`/projects/${projectId}/databases`);
}

export function getDatabase(projectId: string, databaseId: string) {
  return api.get<Database>(`/projects/${projectId}/databases/${databaseId}`);
}

export function getDatabaseVolume(projectId: string, databaseId: string) {
  return api.get<{ name: string; size_bytes: number | null }>(
    `/projects/${projectId}/databases/${databaseId}/volume`,
  );
}

/** Live connection credentials — split out of getDatabase() so a decrypted
 *  secret only ever goes over the wire on its own request, session-gated
 *  server-side rather than riding along on the main database fetch. */
export function getDatabaseCredentials(projectId: string, databaseId: string) {
  return api.get<DatabaseCredentials>(
    `/projects/${projectId}/databases/${databaseId}/credentials/reveal`,
  );
}

export function createDatabase(
  projectId: string,
  data: {
    name: string;
    slug?: string;
    type: string;
    version?: string;
    credentials?: {
      user?: string;
      password?: string;
      database_name?: string;
      root_password?: string;
    };
    // "other" type only:
    image?: string;
    container_port?: number;
    data_dir?: string;
    env?: Record<string, string>;
    backup_mode?: "volume_snapshot" | "command";
    backup_command?: string;
    restore_command?: string;
  },
) {
  return api.post<Database>(`/projects/${projectId}/databases`, data);
}

export function setDatabaseResources(
  projectId: string,
  databaseId: string,
  data: { cpu_limit: number; memory_limit: number },
) {
  return api.put<Database>(
    `/projects/${projectId}/databases/${databaseId}/resources`,
    data,
  );
}

export function setDatabaseExternalAccess(
  projectId: string,
  databaseId: string,
  enabled: boolean,
) {
  return api.post<{ status: string }>(
    `/projects/${projectId}/databases/${databaseId}/external-access`,
    { enabled },
  );
}

export function listDatabaseBackups(projectId: string, databaseId: string) {
  return api.get<DatabaseBackup[]>(
    `/projects/${projectId}/databases/${databaseId}/backups`,
  );
}

export function backupDatabase(projectId: string, databaseId: string) {
  return api.post<{ status: string }>(
    `/projects/${projectId}/databases/${databaseId}/backups`,
    {},
  );
}

export function listDatabaseRestores(projectId: string, databaseId: string) {
  return api.get<DatabaseRestore[]>(
    `/projects/${projectId}/databases/${databaseId}/restores`,
  );
}

export function restoreDatabase(
  projectId: string,
  databaseId: string,
  backupId: string,
) {
  return api.post<{ status: string }>(
    `/projects/${projectId}/databases/${databaseId}/restore`,
    { backup_id: backupId },
  );
}

export function deleteDatabaseBackup(
  projectId: string,
  databaseId: string,
  backupId: string,
) {
  return api.delete<{ status: string }>(
    `/projects/${projectId}/databases/${databaseId}/backups/${backupId}`,
  );
}

export function upgradeDatabase(
  projectId: string,
  databaseId: string,
  targetVersion: string,
) {
  return api.post<{ status: string }>(
    `/projects/${projectId}/databases/${databaseId}/upgrade`,
    { target_version: targetVersion },
  );
}

/**
 * Backups outlive the database unless deleteBackups is set. The flag is only
 * ever sent when true, so an older caller — or a request that loses it — keeps
 * the data rather than destroying it.
 */
export function deleteDatabase(
  projectId: string,
  databaseId: string,
  deleteBackups = false,
) {
  const query = deleteBackups ? "?delete_backups=true" : "";
  return api.delete<void>(
    `/projects/${projectId}/databases/${databaseId}${query}`,
  );
}

export function stopDatabase(projectId: string, databaseId: string) {
  return api.post<Database>(
    `/projects/${projectId}/databases/${databaseId}/stop`,
  );
}

export function startDatabase(projectId: string, databaseId: string) {
  return api.post<Database>(
    `/projects/${projectId}/databases/${databaseId}/start`,
  );
}

export function restartDatabase(projectId: string, databaseId: string) {
  return api.post<Database>(
    `/projects/${projectId}/databases/${databaseId}/restart`,
  );
}

// Recreate the container from the stored record (image, env, volume, network).
// Unlike restart, this recovers a container that has drifted or been deleted.
export function reloadDatabase(projectId: string, databaseId: string) {
  return api.post<{ status: string }>(
    `/projects/${projectId}/databases/${databaseId}/reload`,
  );
}

/** What deleting a database destroys beyond the database itself. */
export interface DatabaseDeletionImpact {
  backup_count: number;
  backup_destinations: string[];
}

export function getDatabaseDeletionImpact(
  projectId: string,
  databaseId: string,
) {
  return api.get<DatabaseDeletionImpact>(
    `/projects/${projectId}/databases/${databaseId}/deletion-impact`,
  );
}

/**
 * A backup whose database has been deleted. It carries what the database was,
 * because there is no database page left to read that from.
 */
export interface OrphanedBackup {
  id: string;
  status: string;
  size_bytes: number;
  has_remote: boolean;
  started_at: string;
  finished_at?: string;
  error?: string;
  tombstone_id: string;
  database_name: string;
  database_slug: string;
  database_type: string;
  database_deleted_at: string;
}

export function listOrphanedBackups(projectId: string) {
  return api.get<OrphanedBackup[]>(`/projects/${projectId}/orphaned-backups`);
}

export function restoreOrphanedBackup(projectId: string, backupId: string) {
  return api.post<Database>(
    `/projects/${projectId}/orphaned-backups/${backupId}/restore`,
  );
}

export function deleteOrphanedBackup(projectId: string, backupId: string) {
  return api.delete<void>(
    `/projects/${projectId}/orphaned-backups/${backupId}`,
  );
}
