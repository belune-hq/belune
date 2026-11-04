import type { ProcessType } from "@/lib/types";

export type HealthKind = "http" | "command" | "none";

export interface ResolvedHealth {
  kind: HealthKind;
  // "set" = the type chose it; "inherited" = the application's; "default" = the
  // rule's fallback for a non-routable type.
  source: "set" | "inherited" | "default";
}

/**
 * The health check a process type actually runs with. This MIRRORS the server's
 * rule (resolveHealthCheck in the deploy worker) and must stay in step with it:
 *
 *   the type sets one  -> use it
 *   else routable      -> inherit the application's
 *   else               -> none
 *
 * The last line is the load-bearing one: a non-routable type NEVER inherits http,
 * because a worker listens on nothing and would be permanently unhealthy.
 */
export function resolveHealth(
  pt: Pick<ProcessType, "routable" | "health_check_type">,
  appHealthType: HealthKind,
): ResolvedHealth {
  if (pt.health_check_type) {
    return { kind: pt.health_check_type, source: "set" };
  }
  if (pt.routable) return { kind: appHealthType, source: "inherited" };
  return { kind: "none", source: "default" };
}

/** Health kinds a type may be given: http needs a port, so only the routable one. */
export function allowedHealthKinds(routable: boolean): HealthKind[] {
  return routable ? ["http", "command", "none"] : ["command", "none"];
}

// Same pattern the server and the database enforce.
export const PROCESS_TYPE_NAME = /^[a-z][a-z0-9-]{0,19}$/;
