import { useQuery, useMutation, useQueryClient } from "@tanstack/react-query";
import { toast } from "sonner";
import { queryKeys } from "./query-keys";
import * as databasesApi from "@/lib/api/databases";

export function useDatabases(projectId: string) {
  return useQuery({
    queryKey: queryKeys.databases.all(projectId),
    queryFn: () => databasesApi.listDatabases(projectId),
    // Fast while a database is provisioning; slow floor otherwise so changes
    // from other sessions still surface without hammering the API.
    refetchInterval: (query) =>
      query.state.data?.some(
        (d) =>
          d.status === "creating" ||
          d.status === "upgrading" ||
          d.status === "backing_up",
      )
        ? 3000
        : 30000,
  });
}

export function useDatabase(projectId: string, databaseId: string) {
  return useQuery({
    queryKey: queryKeys.databases.detail(projectId, databaseId),
    queryFn: () => databasesApi.getDatabase(projectId, databaseId),
    refetchInterval: (query) => {
      const status = query.state.data?.status;
      return status === "creating" ||
        status === "upgrading" ||
        status === "backing_up"
        ? 3000
        : false;
    },
  });
}

// useDatabaseVolume lazily fetches the managed volume's name and size. Split from
// useDatabase because the size query can take tens of seconds on a busy host, so
// it must not block the detail page from rendering.
export function useDatabaseVolume(projectId: string, databaseId: string) {
  return useQuery({
    queryKey: queryKeys.databases.volume(projectId, databaseId),
    queryFn: () => databasesApi.getDatabaseVolume(projectId, databaseId),
    staleTime: 30_000,
    refetchOnWindowFocus: false,
  });
}

// useDatabaseCredentials lazily fetches the decrypted connection credentials.
// Split from useDatabase because the backend now requires a live session for
// this one — the API sits behind the reveal boundary since a connection
// password is a directly usable secret, not just view-only project data.
export function useDatabaseCredentials(
  projectId: string,
  databaseId: string,
  options?: { enabled?: boolean },
) {
  return useQuery({
    queryKey: queryKeys.databases.credentials(projectId, databaseId),
    queryFn: () => databasesApi.getDatabaseCredentials(projectId, databaseId),
    staleTime: 30_000,
    refetchOnWindowFocus: false,
    enabled: options?.enabled ?? true,
  });
}

// useDatabaseDeletionImpact reports what a delete would destroy. Enabled only
// while the confirmation dialog is open — the answer is only needed at the
// moment of consent, and it is two extra queries.
//
// No staleTime: this gates an irreversible action, so a cached answer is worse
// than a slightly slower dialog. A backup taken since the dialog was last
// opened must not be destroyed on the strength of a count that predates it.
export function useDatabaseDeletionImpact(
  projectId: string,
  databaseId: string,
  enabled: boolean,
) {
  return useQuery({
    queryKey: queryKeys.databases.deletionImpact(projectId, databaseId),
    queryFn: () =>
      databasesApi.getDatabaseDeletionImpact(projectId, databaseId),
    enabled,
    staleTime: 0,
    gcTime: 0,
  });
}

export function useCreateDatabase(projectId: string) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (data: Parameters<typeof databasesApi.createDatabase>[1]) =>
      databasesApi.createDatabase(projectId, data),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: queryKeys.databases.all(projectId) });
      qc.invalidateQueries({ queryKey: queryKeys.projects.detail(projectId) });
    },
  });
}

export function useDeleteDatabase(projectId: string) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: ({
      databaseId,
      deleteBackups = false,
    }: {
      databaseId: string;
      deleteBackups?: boolean;
    }) => databasesApi.deleteDatabase(projectId, databaseId, deleteBackups),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: queryKeys.databases.all(projectId) });
      qc.invalidateQueries({ queryKey: queryKeys.projects.detail(projectId) });
      // A kept backup becomes an orphan the project inventory has to show.
      qc.invalidateQueries({
        queryKey: queryKeys.databases.orphaned(projectId),
      });
    },
  });
}

// Lifecycle hooks refresh both the database detail and the project's database
// list so the row status updates without waiting for the next poll.
function useDatabaseLifecycle(
  projectId: string,
  mutationFn: () => Promise<unknown>,
) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn,
    // Awaited so the mutation stays `isPending` until the row's status reflects
    // the change — otherwise the action buttons briefly flash the wrong state
    // (e.g. Stop/Restart) between the spinner and Start.
    //
    // ⚠️ `all` alone is correct AND sufficient: invalidateQueries matches by
    // PREFIX, and ["projects", p, "databases"] prefixes `detail` and every key
    // under it (volume, credentials, backups, restores, backup-configs,
    // deletion-impact). Invalidating `detail` too fetched all of those twice.
    onSuccess: () =>
      qc.invalidateQueries({ queryKey: queryKeys.databases.all(projectId) }),
    onError: (error: Error) => toast.error(error.message),
  });
}

export function useStopDatabase(projectId: string, databaseId: string) {
  return useDatabaseLifecycle(projectId, () =>
    databasesApi.stopDatabase(projectId, databaseId),
  );
}

export function useStartDatabase(projectId: string, databaseId: string) {
  return useDatabaseLifecycle(projectId, () =>
    databasesApi.startDatabase(projectId, databaseId),
  );
}

export function useRestartDatabase(projectId: string, databaseId: string) {
  return useDatabaseLifecycle(projectId, () =>
    databasesApi.restartDatabase(projectId, databaseId),
  );
}

export function useReloadDatabase(projectId: string, databaseId: string) {
  return useDatabaseLifecycle(projectId, () =>
    databasesApi.reloadDatabase(projectId, databaseId),
  );
}

export function useSetDatabaseResources(projectId: string, databaseId: string) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (data: { cpu_limit: number; memory_limit: number }) =>
      databasesApi.setDatabaseResources(projectId, databaseId, data),
    onSuccess: () => {
      qc.invalidateQueries({
        queryKey: queryKeys.databases.detail(projectId, databaseId),
      });
    },
  });
}

export function useSetDatabaseExternalAccess(
  projectId: string,
  databaseId: string,
) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (enabled: boolean) =>
      databasesApi.setDatabaseExternalAccess(projectId, databaseId, enabled),
    onSuccess: () => {
      qc.invalidateQueries({
        queryKey: queryKeys.databases.detail(projectId, databaseId),
      });
    },
  });
}

export function useDatabaseBackups(projectId: string, databaseId: string) {
  return useQuery({
    queryKey: queryKeys.databases.backups(projectId, databaseId),
    queryFn: () => databasesApi.listDatabaseBackups(projectId, databaseId),
    // Poll faster while a backup is running so the row resolves promptly.
    refetchInterval: (query) =>
      query.state.data?.some((b) => b.status === "running") ? 3000 : false,
  });
}

export function useBackupDatabase(projectId: string, databaseId: string) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: () => databasesApi.backupDatabase(projectId, databaseId),
    onSuccess: () => {
      const key = queryKeys.databases.backups(projectId, databaseId);
      // The worker inserts the "running" run row shortly after the task is
      // enqueued; re-poll briefly so it shows up without a manual refresh.
      qc.invalidateQueries({ queryKey: key });
      [1000, 2500, 5000].forEach((ms) =>
        setTimeout(() => qc.invalidateQueries({ queryKey: key }), ms),
      );
    },
  });
}

export function useDeleteDatabaseBackup(projectId: string, databaseId: string) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (backupId: string) =>
      databasesApi.deleteDatabaseBackup(projectId, databaseId, backupId),
    onSuccess: () => {
      qc.invalidateQueries({
        queryKey: queryKeys.databases.backups(projectId, databaseId),
      });
    },
  });
}

export function useDatabaseRestores(projectId: string, databaseId: string) {
  return useQuery({
    queryKey: queryKeys.databases.restores(projectId, databaseId),
    queryFn: () => databasesApi.listDatabaseRestores(projectId, databaseId),
    refetchInterval: (query) =>
      query.state.data?.some((r) => r.status === "running") ? 3000 : false,
  });
}

export function useRestoreDatabase(projectId: string, databaseId: string) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (backupId: string) =>
      databasesApi.restoreDatabase(projectId, databaseId, backupId),
    onSuccess: () => {
      // The worker inserts the "running" restore row shortly after enqueue;
      // re-poll briefly so it shows up without a manual refresh.
      const key = queryKeys.databases.restores(projectId, databaseId);
      qc.invalidateQueries({ queryKey: key });
      [1000, 2500, 5000].forEach((ms) =>
        setTimeout(() => qc.invalidateQueries({ queryKey: key }), ms),
      );
    },
  });
}

export function useUpgradeDatabase(projectId: string, databaseId: string) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (targetVersion: string) =>
      databasesApi.upgradeDatabase(projectId, databaseId, targetVersion),
    onSuccess: () => {
      qc.invalidateQueries({
        queryKey: queryKeys.databases.detail(projectId, databaseId),
      });
    },
  });
}

/** Backups whose database is gone. They exist and are billed but have no
 * database page to appear on, which is why the project lists them. */
export function useOrphanedBackups(projectId: string) {
  return useQuery({
    queryKey: queryKeys.databases.orphaned(projectId),
    queryFn: () => databasesApi.listOrphanedBackups(projectId),
  });
}

export function useRestoreOrphanedBackup(projectId: string) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (backupId: string) =>
      databasesApi.restoreOrphanedBackup(projectId, backupId),
    onSuccess: () => {
      // The replacement is a live database again, so both listings move.
      qc.invalidateQueries({
        queryKey: queryKeys.databases.orphaned(projectId),
      });
      qc.invalidateQueries({ queryKey: queryKeys.databases.all(projectId) });
      qc.invalidateQueries({ queryKey: queryKeys.projects.detail(projectId) });
      // The reclaimed backups rejoin the project's main backup listing, which
      // joins databases — without this the panel empties and the list below it
      // still does not show them.
      qc.invalidateQueries({ queryKey: queryKeys.projectBackups(projectId) });
    },
  });
}

export function useDeleteOrphanedBackup(projectId: string) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (backupId: string) =>
      databasesApi.deleteOrphanedBackup(projectId, backupId),
    onSuccess: () => {
      qc.invalidateQueries({
        queryKey: queryKeys.databases.orphaned(projectId),
      });
      qc.invalidateQueries({ queryKey: queryKeys.projectBackups(projectId) });
    },
  });
}
