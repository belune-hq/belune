import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { queryKeys } from "./query-keys";
import * as api from "@/lib/api/process-types";

export function useProcessTypes(projectId: string, applicationId: string) {
  return useQuery({
    queryKey: queryKeys.processTypes.all(projectId, applicationId),
    queryFn: () => api.listProcessTypes(projectId, applicationId),
    // Instance status is driven by container events, so keep it reasonably fresh
    // without a dedicated stream.
    refetchInterval: 10_000,
  });
}

// Every change stamps the config-changed marker server-side, and the header badge
// that reports it reads the application detail, so refresh both.
function useRefresh(projectId: string, applicationId: string) {
  const qc = useQueryClient();
  return () =>
    Promise.all([
      qc.invalidateQueries({
        queryKey: queryKeys.processTypes.all(projectId, applicationId),
      }),
      qc.invalidateQueries({
        queryKey: queryKeys.applications.detail(projectId, applicationId),
      }),
    ]);
}

export function useCreateProcessType(projectId: string, applicationId: string) {
  const refresh = useRefresh(projectId, applicationId);
  return useMutation({
    mutationFn: (data: api.CreateProcessType) =>
      api.createProcessType(projectId, applicationId, data),
    onSuccess: refresh,
  });
}

export function useUpdateProcessType(projectId: string, applicationId: string) {
  const refresh = useRefresh(projectId, applicationId);
  return useMutation({
    mutationFn: ({
      processTypeId,
      data,
    }: {
      processTypeId: string;
      data: api.UpdateProcessType;
    }) => api.updateProcessType(projectId, applicationId, processTypeId, data),
    onSuccess: refresh,
  });
}

export function useDeleteProcessType(projectId: string, applicationId: string) {
  const refresh = useRefresh(projectId, applicationId);
  return useMutation({
    mutationFn: (processTypeId: string) =>
      api.deleteProcessType(projectId, applicationId, processTypeId),
    onSuccess: refresh,
  });
}
