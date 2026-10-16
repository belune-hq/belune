import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { queryKeys } from "./query-keys";
import * as api from "@/lib/api/scheduled-tasks";

export function useScheduledTasks(projectId: string, applicationId: string) {
  return useQuery({
    queryKey: queryKeys.scheduledTasks.all(projectId, applicationId),
    queryFn: () => api.listScheduledTasks(projectId, applicationId),
    // Keep the latest-run badge honest while a run is in flight.
    refetchInterval: (query) =>
      query.state.data?.some((t) => t.latest_run?.status === "running")
        ? 2000
        : false,
  });
}

export function useScheduledTaskRuns(
  projectId: string,
  applicationId: string,
  taskId: string,
  enabled = true,
) {
  return useQuery({
    queryKey: queryKeys.scheduledTasks.runs(projectId, applicationId, taskId),
    queryFn: () => api.listScheduledTaskRuns(projectId, applicationId, taskId),
    enabled,
    refetchInterval: (query) =>
      query.state.data?.some((r) => r.status === "running") ? 2000 : false,
  });
}

function useInvalidateTasks(projectId: string, applicationId: string) {
  const qc = useQueryClient();
  return () =>
    qc.invalidateQueries({
      queryKey: queryKeys.scheduledTasks.all(projectId, applicationId),
    });
}

export function useCreateScheduledTask(
  projectId: string,
  applicationId: string,
) {
  const invalidate = useInvalidateTasks(projectId, applicationId);
  return useMutation({
    mutationFn: (data: api.SaveScheduledTask) =>
      api.createScheduledTask(projectId, applicationId, data),
    onSuccess: invalidate,
  });
}

export function useUpdateScheduledTask(
  projectId: string,
  applicationId: string,
) {
  const invalidate = useInvalidateTasks(projectId, applicationId);
  return useMutation({
    mutationFn: ({
      taskId,
      data,
    }: {
      taskId: string;
      data: api.SaveScheduledTask;
    }) => api.updateScheduledTask(projectId, applicationId, taskId, data),
    onSuccess: invalidate,
  });
}

export function useDeleteScheduledTask(
  projectId: string,
  applicationId: string,
) {
  const invalidate = useInvalidateTasks(projectId, applicationId);
  return useMutation({
    mutationFn: (taskId: string) =>
      api.deleteScheduledTask(projectId, applicationId, taskId),
    onSuccess: invalidate,
  });
}

export function useRunScheduledTask(projectId: string, applicationId: string) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (taskId: string) =>
      api.runScheduledTask(projectId, applicationId, taskId),
    // The run row exists as soon as the request returns, so refetch directly.
    onSuccess: (_run, taskId) => {
      qc.invalidateQueries({
        queryKey: queryKeys.scheduledTasks.all(projectId, applicationId),
      });
      qc.invalidateQueries({
        queryKey: queryKeys.scheduledTasks.runs(
          projectId,
          applicationId,
          taskId,
        ),
      });
    },
  });
}
