import type { GlobalDeployment } from "@/lib/types";
import { api } from "./client";

export interface GlobalDeploymentFilters {
  limit?: number;
  offset?: number;
  project_id?: string;
  application_id?: string;
  status?: string;
  search?: string;
  from?: string;
  to?: string;
}

export function listGlobalDeployments(params?: GlobalDeploymentFilters) {
  const query = new URLSearchParams();
  if (params?.limit) query.set("limit", String(params.limit));
  if (params?.offset) query.set("offset", String(params.offset));
  if (params?.project_id) query.set("project_id", params.project_id);
  if (params?.application_id) query.set("application_id", params.application_id);
  if (params?.status) query.set("status", params.status);
  if (params?.search) query.set("search", params.search);
  if (params?.from) query.set("from", params.from);
  if (params?.to) query.set("to", params.to);
  const qs = query.toString();
  return api.get<GlobalDeployment[]>(`/deployments${qs ? `?${qs}` : ""}`);
}
