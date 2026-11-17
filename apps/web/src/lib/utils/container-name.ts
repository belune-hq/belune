import type { ProcessInstance } from "@/lib/types";

/**
 * The short label for a container on the Processes tab: `{type}-{idx}` plus the platform's
 * random suffix when it has one ("web-1-db3df3b2").
 *
 * Built from the type and index, NOT by taking the last N dash-separated parts of the name:
 * a type may itself contain a dash (`worker-1`), so "the last three" of
 * `app-worker-1-1-2f267ca6` would be `1-1-2f267ca6`.
 *
 * The suffix counts only when the name ends `-{type}-{idx}-{8 hex}`, the shape a roll or an
 * API-minted name has. ⚠️ Not merely "the last segment is 8 hex characters": a legacy
 * container is named by the application's slug, which is `{project}-{name}-{first 8 of the
 * application id}`, so it ALSO ends in 8 hex characters, and that is not a container's own
 * token. Such a name reads `web-1`. The full name stays available as the row's title and in
 * the actions menu's accessible name.
 */
export function shortContainerName(
  typeName: string,
  inst: Pick<ProcessInstance, "idx" | "container_name">,
): string {
  const base = `${typeName}-${inst.idx}`;
  const tail = `-${base}-`;
  const at = inst.container_name.lastIndexOf(tail);
  if (at === -1) return base;
  const suffix = inst.container_name.slice(at + tail.length);
  return /^[0-9a-f]{8}$/.test(suffix) ? `${base}-${suffix}` : base;
}
