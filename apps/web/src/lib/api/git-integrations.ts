import { api } from "./client";
import type { GitProvider } from "./git-providers";

export interface GitIntegration {
  id: string;
  provider: GitProvider;
  base_url: string;
  account_login: string;
  created_at: string;
}

export interface AvailableProvider {
  provider: GitProvider;
  base_url: string;
  app_slug: string;
}

export function listGitIntegrations() {
  return api.get<GitIntegration[]>("/git/integrations");
}

export function listAvailableProviders() {
  return api.get<AvailableProvider[]>("/git/integrations/available");
}

export function startGitConnect(provider: GitProvider, baseUrl?: string) {
  const q = new URLSearchParams({ provider });
  if (baseUrl) q.set("base_url", baseUrl);
  return api.get<{ auth_url: string }>(`/git/integrations/connect?${q.toString()}`);
}

export function deleteGitIntegration(id: string) {
  return api.delete<{ status: string }>(`/git/integrations/${id}`);
}

export interface IntegrationRepo {
  full_name: string;
  clone_url: string;
  private: boolean;
  default_branch: string;
}

export interface IntegrationBranch {
  name: string;
}

export function listIntegrationRepos(id: string) {
  return api.get<IntegrationRepo[]>(`/git/integrations/${id}/repos`);
}

export function listIntegrationBranches(id: string, repo: string) {
  return api.get<IntegrationBranch[]>(
    `/git/integrations/${id}/branches?repo=${encodeURIComponent(repo)}`,
  );
}
