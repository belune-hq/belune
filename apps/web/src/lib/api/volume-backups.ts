import type {
  VolumeBackupConfig,
  AppVolumeBackupConfig,
  VolumeBackup,
  VolumeRestore,
} from "@/lib/types";
import { api } from "./client";

export interface SaveVolumeBackupConfig {
  destination_id: string;
  prefix?: string;
  schedule?: string;
  timezone?: string;
  keep_latest?: number | null;
  quiesce: boolean;
  enabled?: boolean;
}

function base(projectId: string, applicationId: string, volumeId: string) {
  return `/projects/${projectId}/applications/${applicationId}/volumes/${volumeId}`;
}

export function listVolumeBackupConfigs(
  projectId: string,
  applicationId: string,
  volumeId: string,
) {
  return api.get<VolumeBackupConfig[]>(
    `${base(projectId, applicationId, volumeId)}/backup-configs`,
  );
}

// listAppVolumeBackupConfigs returns every volume backup config across all of an
// application's volumes (each carries its volume_name + mount_path).
export function listAppVolumeBackupConfigs(
  projectId: string,
  applicationId: string,
) {
  return api.get<AppVolumeBackupConfig[]>(
    `/projects/${projectId}/applications/${applicationId}/volume-backup-configs`,
  );
}

export function createVolumeBackupConfig(
  projectId: string,
  applicationId: string,
  volumeId: string,
  data: SaveVolumeBackupConfig,
) {
  return api.post<VolumeBackupConfig>(
    `${base(projectId, applicationId, volumeId)}/backup-configs`,
    data,
  );
}

export function updateVolumeBackupConfig(
  projectId: string,
  applicationId: string,
  volumeId: string,
  configId: string,
  data: SaveVolumeBackupConfig,
) {
  return api.put<VolumeBackupConfig>(
    `${base(projectId, applicationId, volumeId)}/backup-configs/${configId}`,
    data,
  );
}

export function deleteVolumeBackupConfig(
  projectId: string,
  applicationId: string,
  volumeId: string,
  configId: string,
) {
  return api.delete<{ status: string }>(
    `${base(projectId, applicationId, volumeId)}/backup-configs/${configId}`,
  );
}

export function runVolumeBackupConfig(
  projectId: string,
  applicationId: string,
  volumeId: string,
  configId: string,
) {
  return api.post<{ status: string }>(
    `${base(projectId, applicationId, volumeId)}/backup-configs/${configId}/run`,
    {},
  );
}

export function listVolumeBackups(
  projectId: string,
  applicationId: string,
  volumeId: string,
) {
  return api.get<VolumeBackup[]>(
    `${base(projectId, applicationId, volumeId)}/backups`,
  );
}

export function restoreVolumeBackup(
  projectId: string,
  applicationId: string,
  volumeId: string,
  backupId: string,
) {
  return api.post<{ status: string }>(
    `${base(projectId, applicationId, volumeId)}/backups/${backupId}/restore`,
    {},
  );
}

export function listVolumeRestores(
  projectId: string,
  applicationId: string,
  volumeId: string,
) {
  return api.get<VolumeRestore[]>(
    `${base(projectId, applicationId, volumeId)}/restores`,
  );
}
