import { useQuery, useMutation, useQueryClient } from "@tanstack/react-query";
import { queryKeys } from "./query-keys";
import * as configApi from "@/lib/api/database-backup-configs";
import type { SaveBackupConfig } from "@/lib/api/database-backup-configs";

export function useDatabaseBackupConfigs(projectId: string, databaseId: string) {
  return useQuery({
    queryKey: queryKeys.databases.backupConfigs(projectId, databaseId),
    queryFn: () => configApi.listBackupConfigs(projectId, databaseId),
  });
}

export function useCreateBackupConfig(projectId: string, databaseId: string) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (data: SaveBackupConfig) =>
      configApi.createBackupConfig(projectId, databaseId, data),
    onSuccess: () =>
      qc.invalidateQueries({
        queryKey: queryKeys.databases.backupConfigs(projectId, databaseId),
      }),
  });
}

export function useUpdateBackupConfig(projectId: string, databaseId: string) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: ({
      configId,
      data,
    }: {
      configId: string;
      data: SaveBackupConfig;
    }) => configApi.updateBackupConfig(projectId, databaseId, configId, data),
    onSuccess: () =>
      qc.invalidateQueries({
        queryKey: queryKeys.databases.backupConfigs(projectId, databaseId),
      }),
  });
}

export function useDeleteBackupConfig(projectId: string, databaseId: string) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (configId: string) =>
      configApi.deleteBackupConfig(projectId, databaseId, configId),
    onSuccess: () => {
      qc.invalidateQueries({
        queryKey: queryKeys.databases.backupConfigs(projectId, databaseId),
      });
      qc.invalidateQueries({
        queryKey: queryKeys.databases.backups(projectId, databaseId),
      });
    },
  });
}

export function useRunBackupConfig(projectId: string, databaseId: string) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (configId: string) =>
      configApi.runBackupConfig(projectId, databaseId, configId),
    onSuccess: () => {
      const key = queryKeys.databases.backups(projectId, databaseId);
      // The worker inserts the "running" run row shortly after the task is
      // enqueued; re-poll briefly so it appears without a manual refresh (after
      // which the running-state polling in useDatabaseBackups takes over).
      qc.invalidateQueries({ queryKey: key });
      [1000, 2500, 5000].forEach((ms) =>
        setTimeout(() => qc.invalidateQueries({ queryKey: key }), ms),
      );
    },
  });
}
