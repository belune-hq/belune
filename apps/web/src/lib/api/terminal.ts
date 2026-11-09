import { api } from "./client";

export function createTerminalSession(
  projectId: string,
  applicationId: string,
  shell: string,
  // Opens the shell in that process type's container; omitted = the routable one.
  processType?: string,
): Promise<{ session_id: string }> {
  const qs = processType
    ? `?process_type=${encodeURIComponent(processType)}`
    : "";
  return api.post(
    `/projects/${projectId}/applications/${applicationId}/terminal${qs}`,
    { shell },
  );
}
