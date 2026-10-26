import type { WafEventsPage, WafStatus, WafWindow } from "@/lib/types";
import { api } from "./client";

export interface WafEventFilters {
  clientIp?: string;
  ruleId?: number;
}

export function getWafStatus(window?: WafWindow) {
  const q = window ? `?window=${window}` : "";
  return api.get<WafStatus>(`/maintenance/proxy/waf${q}`);
}

/** Returns the status the proxy is in once the switch has been applied. */
export function setWafEnabled(enabled: boolean) {
  return api.put<WafStatus>("/maintenance/proxy/waf", { enabled });
}

export function listWafEvents(
  filters: WafEventFilters,
  before: number | null,
  limit = 50,
) {
  const q = new URLSearchParams({ limit: String(limit) });
  if (before !== null) q.set("before", String(before));
  if (filters.clientIp) q.set("client_ip", filters.clientIp);
  if (filters.ruleId !== undefined) q.set("rule_id", String(filters.ruleId));
  return api.get<WafEventsPage>(`/maintenance/proxy/waf/events?${q}`);
}
