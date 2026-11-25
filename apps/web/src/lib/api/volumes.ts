import type { ApplicationVolume } from "@/lib/types";
import { api } from "./client";

export function listVolumes(projectId: string, applicationId: string) {
  return api.get<ApplicationVolume[]>(
    `/projects/${projectId}/applications/${applicationId}/volumes`,
  );
}

export function createVolume(
  projectId: string,
  applicationId: string,
  data: { name: string; mount_path: string },
) {
  return api.post<ApplicationVolume>(
    `/projects/${projectId}/applications/${applicationId}/volumes`,
    data,
  );
}

export function deleteVolume(
  projectId: string,
  applicationId: string,
  volumeId: string,
  deleteData: boolean,
) {
  const query = deleteData ? "?delete_data=true" : "";
  return api.delete<{ status: string; warning?: string }>(
    `/projects/${projectId}/applications/${applicationId}/volumes/${volumeId}${query}`,
  );
}

export function adoptVolume(
  projectId: string,
  applicationId: string,
  data: { name: string; mount_path: string; external_name: string },
) {
  return api.post<ApplicationVolume>(
    `/projects/${projectId}/applications/${applicationId}/volumes/adopt`,
    data,
  );
}
