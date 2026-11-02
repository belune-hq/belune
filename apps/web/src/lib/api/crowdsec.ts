import type {
  CrowdSecAllowlist,
  CrowdSecDecision,
  CrowdSecDecisions,
  CrowdSecPolicy,
  CrowdSecStatus,
} from "@/lib/types";
import { listAuditLogs } from "./audit-logs";
import { api } from "./client";

const BASE = "/maintenance/proxy/crowdsec";

export function getCrowdSecStatus() {
  return api.get<CrowdSecStatus>(BASE);
}

/** Returns the status the proxy is in once the switch has been applied. */
export function setCrowdSecEnabled(enabled: boolean) {
  return api.put<CrowdSecStatus>(BASE, { enabled });
}

export function listCrowdSecDecisions() {
  return api.get<CrowdSecDecisions>(`${BASE}/decisions`);
}

export interface BanRequest {
  /** An IP address or a CIDR range. */
  target: string;
  /** A number and m, h or d: "90m", "24h", "7d". */
  duration: string;
  reason?: string;
}

export function banAddress(req: BanRequest) {
  return api.post<CrowdSecDecision>(`${BASE}/decisions`, req);
}

export function liftBan(target: string) {
  return api.delete<void>(
    `${BASE}/decisions?target=${encodeURIComponent(target)}`,
  );
}

export function listCrowdSecAllowlist() {
  return api.get<CrowdSecAllowlist>(`${BASE}/allowlist`);
}

export interface AllowRequest {
  target: string;
  duration: string;
  note?: string;
}

export function allowAddress(req: AllowRequest) {
  return api.post<void>(`${BASE}/allowlist`, req);
}

export function removeAllowed(target: string) {
  return api.delete<void>(
    `${BASE}/allowlist?target=${encodeURIComponent(target)}`,
  );
}

export function setCrowdSecAutoban(policy: CrowdSecPolicy) {
  return api.put<CrowdSecStatus>(`${BASE}/autoban`, policy);
}

export function rotateCrowdSecKey() {
  return api.post<CrowdSecStatus>(`${BASE}/rotate-key`);
}

/**
 * What happened on this tab, newest first, from the audit log. Operator actions
 * and the automatic bans are both written there with resource type "crowdsec", so
 * the history needs no storage of its own.
 */
export function listCrowdSecHistory(limit = 25) {
  return listAuditLogs({ resource_type: "crowdsec", limit, offset: 0 });
}
