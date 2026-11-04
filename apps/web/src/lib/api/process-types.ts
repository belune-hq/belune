import type { ProcessType } from "@/lib/types";
import { api } from "./client";

function base(projectId: string, applicationId: string) {
  return `/projects/${projectId}/applications/${applicationId}/process-types`;
}

export function listProcessTypes(projectId: string, applicationId: string) {
  return api.get<ProcessType[]>(base(projectId, applicationId));
}

export interface CreateProcessType {
  name: string;
  command?: string;
  // "" = resolve by the rule.
  health_check_type?: string;
  health_check_command?: string;
}

export function createProcessType(
  projectId: string,
  applicationId: string,
  data: CreateProcessType,
) {
  return api.post<ProcessType>(base(projectId, applicationId), data);
}

// The API keeps an absent field and clears a health field sent as "", so a form
// that always sends all three says exactly what it means.
export interface UpdateProcessType {
  command?: string;
  health_check_type?: string;
  health_check_command?: string;
}

export function updateProcessType(
  projectId: string,
  applicationId: string,
  processTypeId: string,
  data: UpdateProcessType,
) {
  return api.put<ProcessType>(
    `${base(projectId, applicationId)}/${processTypeId}`,
    data,
  );
}

export function deleteProcessType(
  projectId: string,
  applicationId: string,
  processTypeId: string,
) {
  return api.delete<{ status: string; warning?: string }>(
    `${base(projectId, applicationId)}/${processTypeId}`,
  );
}
