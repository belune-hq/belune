import type { ProcessInstance } from "@/lib/types";

/**
 * The short label for a container on the Processes tab: `{type}-{idx}` plus the platform's
 * random suffix when it has one ("web-1-db3df3b2").
 *
 * Built from the type and index, NOT by taking the last N dash-separated parts of the name:
 * a type may itself contain a dash (`worker-1`), so "the last three" of
 * `app-worker-1-1-2f267ca6` would be `1-1-2f267ca6`. The suffix is the name's last segment
 * when it is the 8 hex characters a roll or an API-minted name ends in; a legacy name (just
 * the slug) has none, so it reads `web-1`. The full name stays available as the row's title
 * and in the actions menu's accessible name.
 */
export function shortContainerName(
  typeName: string,
  inst: Pick<ProcessInstance, "idx" | "container_name">,
): string {
  const last = inst.container_name.split("-").at(-1) ?? "";
  const suffix = /^[0-9a-f]{8}$/.test(last) ? `-${last}` : "";
  return `${typeName}-${inst.idx}${suffix}`;
}
