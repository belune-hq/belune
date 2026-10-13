import type { Application, DeployHook } from "@/lib/types";
import { api } from "./client";

export function listApplications(projectId: string) {
  return api.get<Application[]>(`/projects/${projectId}/applications`);
}

export function getApplication(projectId: string, applicationId: string) {
  return api.get<Application>(
    `/projects/${projectId}/applications/${applicationId}`,
  );
}

export function createApplication(
  projectId: string,
  data: {
    name: string;
    slug?: string;
    type: "git" | "image";
    source_repo?: string;
    source_image?: string;
    dockerfile_path?: string;
    build_type?: string;
    git_integration_id?: string;
    /** PAT for a private repo cloned by URL; omitted/empty = public repo. */
    git_token?: string;
    /** Ref to build; empty/omitted = the repository's default ref. */
    branch?: string;
    /** Subdirectory to build from; empty/omitted = the repo root. */
    root_directory?: string;
  },
) {
  return api.post<Application>(`/projects/${projectId}/applications`, data);
}

export function updateApplication(
  projectId: string,
  applicationId: string,
  data: {
    name?: string;
    source_repo?: string;
    source_image?: string;
    dockerfile_path?: string;
    build_type_override?: string;
    builder_image?: string;
    git_token?: string;
    /** Ref to build; empty string clears it back to the repository default. */
    branch?: string;
    git_integration_id?: string;
    /** Subdirectory to build from; empty string clears it back to the repo root. */
    root_directory?: string;
    // Note: resource limits (setResources) and health config (setHealthCheck)
    // have their own endpoints. This update preserves them, so they are not
    // accepted here — sending them would be silently ignored.
  },
) {
  return api.put<Application>(
    `/projects/${projectId}/applications/${applicationId}`,
    data,
  );
}

export function updateApplicationRuntime(
  projectId: string,
  applicationId: string,
  data: { readonly_rootfs: boolean; container_caps: "minimal" | "standard" },
) {
  return api.put<{ readonly_rootfs: boolean; container_caps: string }>(
    `/projects/${projectId}/applications/${applicationId}/runtime`,
    data,
  );
}

export function deleteApplication(projectId: string, applicationId: string) {
  return api.delete<void>(
    `/projects/${projectId}/applications/${applicationId}`,
  );
}

export function deployApplication(projectId: string, applicationId: string) {
  return api.post<{ message: string }>(
    `/projects/${projectId}/applications/${applicationId}/deploy`,
  );
}

export function stopApplication(projectId: string, applicationId: string) {
  return api.post<Application>(
    `/projects/${projectId}/applications/${applicationId}/stop`,
  );
}

export function startApplication(projectId: string, applicationId: string) {
  return api.post<Application>(
    `/projects/${projectId}/applications/${applicationId}/start`,
  );
}

export function restartApplication(projectId: string, applicationId: string) {
  return api.post<Application>(
    `/projects/${projectId}/applications/${applicationId}/restart`,
  );
}

// Recreate the container from the current image (skip build) to apply config
// changes — volumes, file mounts, env, resource limits — without pulling new code.
export function reloadApplication(projectId: string, applicationId: string) {
  return api.post<{ message: string }>(
    `/projects/${projectId}/applications/${applicationId}/reload`,
  );
}

// Rebuild the currently-deployed commit (git apps only), not branch HEAD.
export function rebuildApplication(projectId: string, applicationId: string) {
  return api.post<{ message: string }>(
    `/projects/${projectId}/applications/${applicationId}/rebuild`,
  );
}

export interface BuildCacheInfo {
  build_cache_bytes: number;
  launch_cache_bytes: number;
  total_bytes: number;
}

export function getBuildCache(projectId: string, applicationId: string) {
  return api.get<BuildCacheInfo>(
    `/projects/${projectId}/applications/${applicationId}/cache`,
  );
}

export function clearBuildCache(projectId: string, applicationId: string) {
  return api.delete<{ status: string }>(
    `/projects/${projectId}/applications/${applicationId}/cache`,
  );
}

export function updateWebhook(
  projectId: string,
  applicationId: string,
  data: {
    webhook_secret?: string;
    auto_deploy_branch?: string;
  },
) {
  return api.put<Application>(
    `/projects/${projectId}/applications/${applicationId}/webhook`,
    data,
  );
}

// Returns the plaintext webhook secret. Audited server-side, so call it only
// when the user asks to see it.
export function revealWebhookSecret(projectId: string, applicationId: string) {
  return api.get<{ webhook_secret: string }>(
    `/projects/${projectId}/applications/${applicationId}/webhook/reveal`,
  );
}

export function getDeployHook(projectId: string, applicationId: string) {
  return api.get<DeployHook>(
    `/projects/${projectId}/applications/${applicationId}/deploy-hook`,
  );
}

// Returns the stored token so the URL can be copied again later. Audited
// server-side, so only call it when the user asks to see the URL.
export function revealDeployHook(projectId: string, applicationId: string) {
  return api.get<DeployHook>(
    `/projects/${projectId}/applications/${applicationId}/deploy-hook/reveal`,
  );
}

// Generates or rotates the token. Rotating invalidates the previous URL
// immediately, so any CI still using it starts getting 404s.
export function generateDeployHook(projectId: string, applicationId: string) {
  return api.post<DeployHook>(
    `/projects/${projectId}/applications/${applicationId}/deploy-hook`,
    {},
  );
}

export function deleteDeployHook(projectId: string, applicationId: string) {
  return api.delete<DeployHook>(
    `/projects/${projectId}/applications/${applicationId}/deploy-hook`,
  );
}

// Swaps the application between git and image. An explicit action rather than
// part of updateApplication: several fields must move together, and everything
// that belongs to the application rather than its source — domains and their
// certificates, volumes and their data, file mounts, env vars, the deploy hook,
// deployment history — is preserved.
export function changeApplicationSource(
  projectId: string,
  applicationId: string,
  data: {
    type: "git" | "image";
    source_image?: string;
    source_repo?: string;
    branch?: string;
    build_type?: string;
    dockerfile_path?: string;
    root_directory?: string;
    git_integration_id?: string;
    git_token?: string;
  },
) {
  return api.post<Application>(
    `/projects/${projectId}/applications/${applicationId}/change-source`,
    data,
  );
}

// Configures how the application's health is checked. Type-specific: "http"
// uses path/expect_status, "command" uses command/interval/retries/start_period,
// both share timeout_seconds. The server clears the fields of the unselected
// mechanism, so the row never carries a stale command or path.
export function setHealthCheck(
  projectId: string,
  applicationId: string,
  data: {
    type: "none" | "http" | "command";
    path?: string;
    expect_status?: number;
    command?: string;
    interval_seconds?: number;
    retries?: number;
    start_period_seconds?: number;
    timeout_seconds?: number;
  },
) {
  return api.put<Application>(
    `/projects/${projectId}/applications/${applicationId}/health-check`,
    data,
  );
}

// Sets CPU and memory limits. Its own endpoint so the Resources card need not
// echo back every source field to avoid the general update's coherence check.
export function setResources(
  projectId: string,
  applicationId: string,
  data: { cpu_limit: number; memory_limit: number },
) {
  return api.put<Application>(
    `/projects/${projectId}/applications/${applicationId}/resources`,
    data,
  );
}

// Sets the port the container listens on; 0 clears it. Its own endpoint, not a
// field on the general update: that one rewrites every column, and a nullable
// port there could not tell "left out" from "clear it".
export function setContainerPort(
  projectId: string,
  applicationId: string,
  data: { container_port: number },
) {
  return api.put<Application>(
    `/projects/${projectId}/applications/${applicationId}/port`,
    data,
  );
}
