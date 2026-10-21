import type { EnvVar, EnvVarInput } from "@/lib/types";
import { api } from "./client";

export function getEnvVars(projectId: string, applicationId: string) {
  return api.get<EnvVar[]>(`/projects/${projectId}/applications/${applicationId}/env`);
}

export function upsertEnvVars(
  projectId: string,
  applicationId: string,
  vars: EnvVarInput[],
) {
  return api.put<EnvVar[]>(`/projects/${projectId}/applications/${applicationId}/env`, {
    vars,
  });
}

export function revealEnvVar(
  projectId: string,
  applicationId: string,
  envVarId: string,
) {
  return api.get<{ value: string }>(
    `/projects/${projectId}/applications/${applicationId}/env/${envVarId}/reveal`,
  );
}
