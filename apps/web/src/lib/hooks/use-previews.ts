import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { toast } from "sonner";
import { queryKeys } from "./query-keys";
import * as previewsApi from "@/lib/api/previews";

export function usePreviews(projectId: string, applicationId: string) {
  return useQuery({
    queryKey: queryKeys.previews.all(projectId, applicationId),
    queryFn: () => previewsApi.listPreviews(projectId, applicationId),
    // Fast while a preview is deploying; slow floor otherwise so changes from
    // other sessions still surface without hammering the API.
    refetchInterval: (query) =>
      query.state.data?.previews?.some((p) =>
        ["pending", "building", "deploying"].includes(p.status),
      )
        ? 3000
        : 30000,
  });
}

export function useUpdatePreviewConfig(projectId: string, applicationId: string) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (data: Parameters<typeof previewsApi.updatePreviewConfig>[2]) =>
      previewsApi.updatePreviewConfig(projectId, applicationId, data),
    onSuccess: () => {
      qc.invalidateQueries({
        queryKey: queryKeys.applications.detail(projectId, applicationId),
      });
    },
    onError: (error) => toast.error(error.message),
  });
}

export function useDeletePreview(projectId: string, applicationId: string) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (previewId: string) =>
      previewsApi.deletePreview(projectId, applicationId, previewId),
    onSuccess: () => {
      qc.invalidateQueries({
        queryKey: queryKeys.previews.all(projectId, applicationId),
      });
    },
    onError: (error) => toast.error(error.message),
  });
}
