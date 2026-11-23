import { api } from "./client";

export interface CAPISharing {
  share_manual_decisions: boolean;
  share_custom: boolean;
  share_tainted: boolean;
  share_context: boolean;
}
export interface CAPIChange {
  restart_console?: boolean;
  enabled?: boolean;
  sharing?: CAPISharing;
}
export interface CAPIConnectionCheck {
  state: "connected" | "failed" | "rate_limited" | "unavailable";
  reason: string;
  checked_at?: string;
}
export interface CAPIEnrollment {
  state: "not_enrolled" | "awaiting_approval" | "enrolled";
  outcome?: "already_enrolled" | "link_unreadable" | "unknown";
  reason?: string;
  requested_at?: string;
  checked_at?: string;
  restart_required: boolean;
  url?: string;
}
export interface CAPIEnrollRequest {
  mode: "quick" | "key";
  key?: string;
  overwrite: boolean;
  consent: boolean;
  sharing: CAPISharing;
}
export interface CAPIStatus {
  enrollment?: CAPIEnrollment;
  connection?: CAPIConnectionCheck;
  state:
    "not_registered" | "enabled" | "failed_reverted" | "failed" | "unavailable";
  busy: boolean;
  phase: string;
  reason?: string;
  health: string;
  sharing: CAPISharing;
  sharing_available: boolean;
}
const PATH = "/maintenance/proxy/crowdsec/capi";
export const getCAPIStatus = () => api.get<CAPIStatus>(PATH);
export const changeCAPI = (change: CAPIChange) =>
  api.put<{ status: string }>(PATH, change);

export const checkCAPIConnection = () =>
  api.post<CAPIConnectionCheck>(`${PATH}/check`);

export const enrollCAPI = (request: CAPIEnrollRequest) =>
  api.post<CAPIEnrollment>(`${PATH}/enroll`, request);
export const checkCAPIEnrollment = () =>
  api.post<CAPIEnrollment>(`${PATH}/enrollment/check`);
