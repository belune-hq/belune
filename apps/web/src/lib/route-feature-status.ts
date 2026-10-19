import type { ProxyCapabilities, RouteFeature } from "@/lib/types";

/** Why a stored feature is not doing anything. A row can have both. */
export type InertReason =
  | { kind: "module"; module: string }
  // Stored before rate limiting moved to {window, max_events}: the API now
  // refuses to build it, so it is inert on any Caddy until reconfigured.
  | { kind: "legacy-config" };

export interface FeatureStatus {
  /** enabled and nothing stops it applying */
  active: boolean;
  /** enabled in the database but not being applied; empty when it is */
  inert: InertReason[];
}

/**
 * Whether a stored route feature is actually applied.
 *
 * A switch that reads ON for a feature nothing enforces is the original
 * "looked rate-limited and was not" failure, moved into the UI. A disabled
 * feature is not inert — it is off, and says so by being off.
 *
 * Capabilities that are missing or "unknown" never produce a module reason:
 * the claim "your Caddy lacks this module" is only made when the probe saw it.
 */
export function featureStatus(
  feature: Pick<RouteFeature, "feature_type" | "config" | "enabled">,
  capabilities: ProxyCapabilities | undefined,
): FeatureStatus {
  if (!feature.enabled) return { active: false, inert: [] };

  const inert: InertReason[] = [];

  const need = capabilities?.features[feature.feature_type];
  if (need?.state === "unavailable") {
    inert.push({ kind: "module", module: need.module });
  }

  if (isLegacyRateLimit(feature)) inert.push({ kind: "legacy-config" });

  return { active: inert.length === 0, inert };
}

function isLegacyRateLimit(
  feature: Pick<RouteFeature, "feature_type" | "config">,
): boolean {
  if (feature.feature_type !== "rate_limit") return false;
  const c = feature.config;
  return typeof c.window !== "string" || typeof c.max_events !== "number";
}

/** One sentence per reason, for the row and the table badge tooltip. */
export function inertExplanation(reason: InertReason): string {
  switch (reason.kind) {
    case "module":
      return `Configured, not active — your Caddy does not have the ${reason.module} module.`;
    case "legacy-config":
      return 'Configured, not active — this uses an older format. Remove it and add it again as {"window":"1s","max_events":10}.';
  }
}
