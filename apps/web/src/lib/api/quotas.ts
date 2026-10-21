import { api } from "./client";

export type QuotaScope = "user" | "project";

export interface QuotaLimits {
  max_applications: number | null;
  max_cpu: number | null;
  max_memory_mb: number | null;
}

export interface QuotaUsage {
  applications: number;
  cpu: number;
  memory_bytes: number;
}

export interface QuotaView {
  scope: QuotaScope;
  scope_id: string;
  limits: QuotaLimits;
  usage: QuotaUsage;
  meta?: Record<string, string | null | undefined>;
}

export function listQuotas() {
  return api.get<QuotaView[]>("/quotas");
}

export function getQuota(scope: QuotaScope, scopeId: string) {
  return api.get<QuotaView>(`/quotas/${scope}/${scopeId}`);
}

export function upsertQuota(scope: QuotaScope, scopeId: string, limits: QuotaLimits) {
  return api.put<QuotaView>(`/quotas/${scope}/${scopeId}`, limits);
}

export function deleteQuota(scope: QuotaScope, scopeId: string) {
  return api.delete<void>(`/quotas/${scope}/${scopeId}`);
}
