import { useQuery, useMutation, useQueryClient } from "@tanstack/react-query";
import { queryKeys } from "./query-keys";
import * as fileMountsApi from "@/lib/api/file-mounts";

export function useFileMounts(projectId: string, applicationId: string) {
  return useQuery({
    queryKey: queryKeys.fileMounts.all(projectId, applicationId),
    queryFn: () => fileMountsApi.listFileMounts(projectId, applicationId),
  });
}

export function useRevealFileMount(projectId: string, applicationId: string) {
  return useMutation({
    mutationFn: (fileMountId: string) =>
      fileMountsApi.revealFileMount(projectId, applicationId, fileMountId),
  });
}

export function useCreateFileMount(projectId: string, applicationId: string) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (data: Parameters<typeof fileMountsApi.createFileMount>[2]) =>
      fileMountsApi.createFileMount(projectId, applicationId, data),
    // Also refresh the application: saving stamps the config-changed marker
    // server-side, and the header badge that reports it reads the application
    // detail. Without this the badge would not appear until the next poll.
    onSuccess: () =>
      Promise.all([
        qc.invalidateQueries({
          queryKey: queryKeys.fileMounts.all(projectId, applicationId),
        }),
        qc.invalidateQueries({
          queryKey: queryKeys.applications.detail(projectId, applicationId),
        }),
      ]),
  });
}

export function useUpdateFileMount(projectId: string, applicationId: string) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: ({
      fileMountId,
      ...data
    }: {
      fileMountId: string;
    } & Parameters<typeof fileMountsApi.updateFileMount>[3]) =>
      fileMountsApi.updateFileMount(projectId, applicationId, fileMountId, data),
    // Also refresh the application: saving stamps the config-changed marker
    // server-side, and the header badge that reports it reads the application
    // detail. Without this the badge would not appear until the next poll.
    onSuccess: () =>
      Promise.all([
        qc.invalidateQueries({
          queryKey: queryKeys.fileMounts.all(projectId, applicationId),
        }),
        qc.invalidateQueries({
          queryKey: queryKeys.applications.detail(projectId, applicationId),
        }),
      ]),
  });
}

export function useDeleteFileMount(projectId: string, applicationId: string) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (fileMountId: string) =>
      fileMountsApi.deleteFileMount(projectId, applicationId, fileMountId),
    // Also refresh the application: saving stamps the config-changed marker
    // server-side, and the header badge that reports it reads the application
    // detail. Without this the badge would not appear until the next poll.
    onSuccess: () =>
      Promise.all([
        qc.invalidateQueries({
          queryKey: queryKeys.fileMounts.all(projectId, applicationId),
        }),
        qc.invalidateQueries({
          queryKey: queryKeys.applications.detail(projectId, applicationId),
        }),
      ]),
  });
}
