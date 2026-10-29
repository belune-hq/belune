import type {
  WafBlockingApplication,
  WafBlockingPage,
  WafClientsPage,
  WafExclusion,
  WafExclusionInput,
  WafExclusionsPage,
  WafEventsPage,
  WafStatus,
  WafWindow,
} from "@/lib/types";
import { api } from "./client";

export interface WafEventFilters {
  clientIp?: string;
  ruleId?: number;
  /** Scopes the list to a summary period; absent means everything still kept. */
  window?: WafWindow;
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
  // Omitted, the list covers everything still kept. Sent, it covers the same
  // period as the counts it is opened from.
  if (filters.window) q.set("window", filters.window);
  return api.get<WafEventsPage>(`/maintenance/proxy/waf/events?${q}`);
}

/** The window's detections grouped by client, loudest first. */
export function listWafClients(
  window: WafWindow,
  filters: WafEventFilters,
  limit = 50,
) {
  const q = new URLSearchParams({ window, limit: String(limit) });
  if (filters.clientIp) q.set("client_ip", filters.clientIp);
  if (filters.ruleId !== undefined) q.set("rule_id", String(filters.ruleId));
  return api.get<WafClientsPage>(`/maintenance/proxy/waf/clients?${q}`);
}

export function listWafExclusions() {
  return api.get<WafExclusionsPage>("/maintenance/proxy/waf/exclusions");
}

export function createWafExclusion(input: WafExclusionInput) {
  return api.post<WafExclusion>("/maintenance/proxy/waf/exclusions", input);
}

export function deleteWafExclusion(id: number) {
  return api.delete<void>(`/maintenance/proxy/waf/exclusions/${id}`);
}

export function listWafBlocking() {
  return api.get<WafBlockingPage>("/maintenance/proxy/waf/blocking");
}

export function setWafBlocking(input: {
  applicationId: string;
  enabled: boolean;
}) {
  return api.put<WafBlockingApplication>(
    `/maintenance/proxy/waf/blocking/${input.applicationId}`,
    { enabled: input.enabled },
  );
}
