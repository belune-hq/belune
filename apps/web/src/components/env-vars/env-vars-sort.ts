import type { EnvVar } from "@/lib/types";
import type { DraftEnvRow } from "./use-env-var-draft";

export type EnvVarSortKey = "name" | "created" | "updated";

// Both saved rows always have a real timestamp — this is just a defensive
// fallback, never expected to see an actual missing value in practice.
function compareTimestamps(
  a: string | undefined,
  b: string | undefined,
): number {
  if (!a && !b) return 0;
  if (!a) return -1;
  if (!b) return 1;
  return b.localeCompare(a);
}

export function sortDraftRows(
  rows: DraftEnvRow[],
  sortKey: EnvVarSortKey,
): DraftEnvRow[] {
  const sorted = [...rows];
  sorted.sort((a, b) => {
    // A row not yet saved (no server id) has no real key or timestamp to
    // sort by yet — an empty key would otherwise sort first alphabetically,
    // landing a freshly-added row at the top of the group instead of the
    // bottom where it was just added. Pin unsaved rows to the end instead,
    // in the order they were added, regardless of the chosen sort.
    if (!a.id && !b.id) return 0;
    if (!a.id) return 1;
    if (!b.id) return -1;

    if (sortKey === "name") return a.key.localeCompare(b.key);
    return compareTimestamps(
      sortKey === "created" ? a.createdAt : a.updatedAt,
      sortKey === "created" ? b.createdAt : b.updatedAt,
    );
  });
  return sorted;
}

export function sortEnvVars(vars: EnvVar[], sortKey: EnvVarSortKey): EnvVar[] {
  const sorted = [...vars];
  sorted.sort((a, b) => {
    if (sortKey === "name") return a.key.localeCompare(b.key);
    return compareTimestamps(
      sortKey === "created" ? a.created_at : a.updated_at,
      sortKey === "created" ? b.created_at : b.updated_at,
    );
  });
  return sorted;
}
