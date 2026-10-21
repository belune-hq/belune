import type { RequestLog } from "@/lib/types";
import { api } from "./client";

export interface RequestLogFilters {
  limit?: number;
  offset?: number;
  application_id?: string;
  status_min?: number;
  status_max?: number;
  search?: string;
  from?: string;
  to?: string;
}

export interface RequestSummary {
  total: number;
  class_counts: { c2xx: number; c3xx: number; c4xx: number; c5xx: number };
  p50_ms: number;
  p95_ms: number;
  error_rate: number;
  per_minute: { ts: string; count: number }[];
}

function toQuery(params?: RequestLogFilters): string {
  const query = new URLSearchParams();
  if (params?.limit) query.set("limit", String(params.limit));
  if (params?.offset) query.set("offset", String(params.offset));
  if (params?.application_id) query.set("application_id", params.application_id);
  if (params?.status_min != null)
    query.set("status_min", String(params.status_min));
  if (params?.status_max != null)
    query.set("status_max", String(params.status_max));
  if (params?.search) query.set("search", params.search);
  if (params?.from) query.set("from", params.from);
  if (params?.to) query.set("to", params.to);
  const qs = query.toString();
  return qs ? `?${qs}` : "";
}

export function listRequestLogs(params?: RequestLogFilters) {
  return api.get<RequestLog[]>(`/requests${toQuery(params)}`);
}

export function getRequestSummary(params?: RequestLogFilters) {
  return api.get<RequestSummary>(`/requests/summary${toQuery(params)}`);
}
