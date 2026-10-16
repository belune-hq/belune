import type { ScheduledTask, ScheduledTaskRun } from "@/lib/types";
import { api } from "./client";

export interface SaveScheduledTask {
  name?: string;
  command?: string;
  cron?: string;
  timezone?: string;
  enabled?: boolean;
  timeout_seconds?: number;
  keep_runs?: number;
}

function base(projectId: string, applicationId: string) {
  return `/projects/${projectId}/applications/${applicationId}/scheduled-tasks`;
}

export function listScheduledTasks(projectId: string, applicationId: string) {
  return api.get<ScheduledTask[]>(base(projectId, applicationId));
}

export function createScheduledTask(
  projectId: string,
  applicationId: string,
  data: SaveScheduledTask,
) {
  return api.post<ScheduledTask>(base(projectId, applicationId), data);
}

export function updateScheduledTask(
  projectId: string,
  applicationId: string,
  taskId: string,
  data: SaveScheduledTask,
) {
  return api.put<ScheduledTask>(
    `${base(projectId, applicationId)}/${taskId}`,
    data,
  );
}

export function deleteScheduledTask(
  projectId: string,
  applicationId: string,
  taskId: string,
) {
  return api.delete<void>(`${base(projectId, applicationId)}/${taskId}`);
}

export function runScheduledTask(
  projectId: string,
  applicationId: string,
  taskId: string,
) {
  return api.post<ScheduledTaskRun>(
    `${base(projectId, applicationId)}/${taskId}/run`,
    {},
  );
}

export function listScheduledTaskRuns(
  projectId: string,
  applicationId: string,
  taskId: string,
) {
  return api.get<ScheduledTaskRun[]>(
    `${base(projectId, applicationId)}/${taskId}/runs`,
  );
}
