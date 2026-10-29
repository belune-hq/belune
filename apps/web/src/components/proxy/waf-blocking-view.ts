import type { WafBlockingApplication } from "@/lib/types";

export interface BlockingImpact {
  /** Hostnames that will refuse requests once blocking is on. */
  enforced: string[];
  /** Hostnames that stay detection-only because another application, which has
   *  not opted in, also serves them. */
  detectionOnly: string[];
}

/**
 * What turning blocking on for this application does, by hostname.
 *
 * Said before the switch is flipped because the cost of a wrong guess is a real
 * visitor getting a 403: the WAF sees the whole host and not the path, so a host
 * an application shares with one that has not opted in is NOT enforced on, and an
 * operator who expected it to be needs to find out here.
 */
export function describeBlockingImpact(
  app: Pick<WafBlockingApplication, "hosts" | "shared_hosts">,
): BlockingImpact {
  const shared = new Set(app.shared_hosts);
  return {
    enforced: app.hosts.filter((h) => !shared.has(h)),
    detectionOnly: app.hosts.filter((h) => shared.has(h)),
  };
}

/** What the confirmation says. A refusal is a 403 to a real visitor. */
export const BLOCKING_CONFIRM_POINTS = [
  "Requests whose anomaly score passes the threshold are refused with a 403 instead of only being recorded.",
  "The rule set has false positives. Check the Detections above for this application first, and add exclusions for anything that is yours.",
  "This dashboard is never affected. Turning blocking off returns the hosts to detection-only within about 30 seconds.",
] as const;
