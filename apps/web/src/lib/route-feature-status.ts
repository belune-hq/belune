import type {
  NotAppliedFeature,
  ProxyCapabilities,
  RouteFeature,
} from "@/lib/types";

/** Why a stored feature is not doing anything. A row can have both. */
export type InertReason =
  | { kind: "module"; module: string }
  // Stored before rate limiting moved to {window, max_events}: the API now
  // refuses to build it, so it is inert on any Caddy until reconfigured.
  | { kind: "legacy-config" }
  // What the reconciler observed it had to strip. Covers the case the probe
  // cannot: a Caddy Belune cannot exec into reads "unknown" for ever, so no
  // module reason is ever produced there.
  | { kind: "not-applied"; reason: NotAppliedFeature["reason"]; since: string };

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
  notApplied?: NotAppliedFeature[],
): FeatureStatus {
  if (!feature.enabled) return { active: false, inert: [] };

  const inert: InertReason[] = [];

  const need = capabilities?.features[feature.feature_type];
  if (need?.state === "unavailable") {
    inert.push({ kind: "module", module: need.module });
  }

  // Suppressed whenever a module reason is present: the probe's "your Caddy
  // lacks X" is the more specific, already-shipped copy. That keeps this reason
  // to exactly the states that used to render a plain ON toggle.
  if (!inert.some((r) => r.kind === "module")) {
    const seen = notApplied?.find(
      (n) => n.feature_type === feature.feature_type,
    );
    if (seen) {
      inert.push({
        kind: "not-applied",
        reason: seen.reason,
        since: seen.since,
      });
    }
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
    case "not-applied": {
      const since = new Date(reason.since).toLocaleString();
      return reason.reason === "module_absent"
        ? `Configured, not active since ${since} — your Caddy is missing the module this needs. Use the Belune Caddy image.`
        : `Configured, not active since ${since} — Belune cannot check what your Caddy supports, so it serves this without the feature. Check CADDY_CONTAINER_NAME, and that the proxy is a container Belune can reach.`;
    }
    case "legacy-config":
      return 'Configured, not active — this uses an older format. Remove it and add it again as {"window":"1s","max_events":10}.';
  }
}
