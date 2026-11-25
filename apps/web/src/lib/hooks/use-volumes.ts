import { useQuery, useMutation, useQueryClient } from "@tanstack/react-query";
import { queryKeys } from "./query-keys";
import * as volumesApi from "@/lib/api/volumes";

export function useVolumes(projectId: string, applicationId: string) {
  return useQuery({
    queryKey: queryKeys.volumes.all(projectId, applicationId),
    queryFn: () => volumesApi.listVolumes(projectId, applicationId),
  });
}

export function useCreateVolume(projectId: string, applicationId: string) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (data: { name: string; mount_path: string }) =>
      volumesApi.createVolume(projectId, applicationId, data),
    // Also refresh the application: saving stamps the config-changed marker
    // server-side, and the header badge that reports it reads the application
    // detail. Without this the badge would not appear until the next poll.
    onSuccess: () =>
      Promise.all([
        qc.invalidateQueries({
          queryKey: queryKeys.volumes.all(projectId, applicationId),
        }),
        qc.invalidateQueries({
          queryKey: queryKeys.applications.detail(projectId, applicationId),
        }),
        qc.invalidateQueries({ queryKey: queryKeys.docker.volumes }),
      ]),
  });
}

export function useDeleteVolume(projectId: string, applicationId: string) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: ({
      volumeId,
      deleteData,
    }: {
      volumeId: string;
      deleteData: boolean;
    }) =>
      volumesApi.deleteVolume(projectId, applicationId, volumeId, deleteData),
    // Also refresh the application: saving stamps the config-changed marker
    // server-side, and the header badge that reports it reads the application
    // detail. Without this the badge would not appear until the next poll.
    onSuccess: () =>
      Promise.all([
        qc.invalidateQueries({
          queryKey: queryKeys.volumes.all(projectId, applicationId),
        }),
        qc.invalidateQueries({
          queryKey: queryKeys.applications.detail(projectId, applicationId),
        }),
        qc.invalidateQueries({ queryKey: queryKeys.docker.volumes }),
      ]),
  });
}

export function useAdoptVolume(projectId: string, applicationId: string) {
  const qc = useQueryClient();
  return useMutation({
    retry: false,
    mutationFn: (data: {
      name: string;
      mount_path: string;
      external_name: string;
    }) => volumesApi.adoptVolume(projectId, applicationId, data),
    onSuccess: async () => {
      // An initial inventory may still carry a pre-adoption snapshot. Cancel
      // it even when it has no cached data, then request the committed rows.
      await qc.cancelQueries({
        queryKey: queryKeys.volumes.all(projectId, applicationId),
      });
      await Promise.all([
        qc.invalidateQueries({
          queryKey: queryKeys.volumes.all(projectId, applicationId),
        }),
        qc.invalidateQueries({
          queryKey: queryKeys.applications.detail(projectId, applicationId),
        }),
        qc.invalidateQueries({ queryKey: queryKeys.docker.volumes }),
      ]);
    },
  });
}
