import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { toast } from "sonner";
import { queryKeys } from "./query-keys";
import * as deploymentsApi from "@/lib/api/deployments";

export function useDeployments(projectId: string, applicationId: string) {
  return useQuery({
    queryKey: queryKeys.deployments.all(projectId, applicationId),
    queryFn: () => deploymentsApi.listDeployments(projectId, applicationId),
    refetchInterval: (query) => {
      const hasActive = query.state.data?.some((d) =>
        ["pending", "building", "deploying"].includes(d.status),
      );
      return hasActive ? 3000 : false;
    },
  });
}

export function useDeployment(
  projectId: string,
  applicationId: string,
  deploymentId: string,
) {
  return useQuery({
    queryKey: queryKeys.deployments.detail(projectId, applicationId, deploymentId),
    queryFn: () =>
      deploymentsApi.getDeployment(projectId, applicationId, deploymentId),
  });
}

export function useRollbackDeployment(projectId: string, applicationId: string) {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: (deploymentId: string) =>
      deploymentsApi.rollbackDeployment(projectId, applicationId, deploymentId),
    onSuccess: () => {
      queryClient.invalidateQueries({
        queryKey: queryKeys.deployments.all(projectId, applicationId),
      });
    },
    onError: (error) => toast.error(error.message),
  });
}
