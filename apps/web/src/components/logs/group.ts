import type { LogEntry } from "@/components/logs/parse";

// Which deployment, and which container, each line belongs to -- the model behind the
// viewer's dividers and its source column.
//
// ⛔ The old model keyed a divider on the CONTAINER and emitted one on every change of key.
// With two containers writing at once the key alternates on nearly every line, so the view
// got one header per line. Filtering to one process type does not help: a container is not
// a type, so `web` at 3 replicas is still three interleaved containers. The fix is the
// model, not the filter:
//
//   - group by DEPLOYMENT (a database has none, so it falls back to the container),
//   - a divider on a group's FIRST sighting only,
//   - a per-line source label saying which container the line is.

/** Dividers read "Earlier logs" for lines collected before containers were recorded. */
const EARLIER = "Earlier logs";

/** The short container id `docker logs` and `docker ps` both show. */
const SHORT_ID = 8;

/** Replicas at or above this length are truncated, so the column has a bounded width. */
export const MAX_SOURCE_WIDTH = 30;

/**
 * The group a line belongs to: its deployment, or -- for a database, or an application
 * line from before the column existed -- its container, or null for the unassigned bucket.
 * One expression gets the database case right for free: a database line never has a
 * deployment, so it keeps a divider per container run (a major-version upgrade replaces
 * the container and those runs do need separating).
 */
export function groupKey(e: LogEntry): string | null {
  return e.deploymentId ?? e.sessionId ?? null;
}

/** "web-0", "web" when the replica is unknown, or null when the type is unknown too. */
function baseSource(e: LogEntry): string | null {
  if (!e.processType) return null;
  return e.instanceIdx != null && e.instanceIdx >= 0
    ? `${e.processType}-${e.instanceIdx}`
    : e.processType;
}

/** `base` within `room` characters, shortening the type name and keeping `-{idx}`. */
function fitBase(e: LogEntry, base: string, room: number): string {
  if (base.length <= room || !e.processType) return base;
  const tail = base.slice(e.processType.length); // "-0", or "" when the idx is unknown
  const keep = Math.max(room - tail.length - 1, 1);
  return `${e.processType.slice(0, keep)}…${tail}`;
}

function dividerLabel(
  e: LogEntry,
  labels: ReadonlyMap<string, string>,
): string {
  const key = groupKey(e);
  if (key === null) return EARLIER;
  // Enriched from the sessions list when it knows the group. That list refetches on a
  // timer, so during a roll a new container's lines arrive long before it does: the
  // label must not depend on it, and the id on the line always names the deployment.
  const known = labels.get(key);
  if (known) return known;
  return e.deploymentId
    ? `#${e.deploymentId.slice(0, 7)}`
    : `Run · ${key.slice(0, 12)}`;
}

/**
 * Interleaves group dividers into a chronological list and fills each line's `source`.
 *
 * `labels` maps a group key to its full divider label (from the sessions list); a group
 * it does not know is labelled from the line itself.
 *
 * ⚠️ A divider is emitted on a group's FIRST sighting, not on every change: a rolling
 * deploy runs two generations at once, so "divider on change" would bring the one-header-
 * per-line thrash straight back in the window you most want to read. The cost, which is
 * why every line also names its own container: a group that appears, is interrupted and
 * resumes gets no second divider, so lines from deployment A can sit below B's header.
 */
export function groupEntries(
  entries: readonly LogEntry[],
  labels: ReadonlyMap<string, string>,
): LogEntry[] {
  const groups = new Set<string | null>();
  for (const e of entries) groups.add(groupKey(e));
  const dividers = groups.size >= 2; // counts GROUPS: two replicas of one deploy need none

  // Source labels over the whole view.
  const containersByLabel = new Map<string, Set<string>>();
  const labelsSeen = new Set<string>();
  for (const e of entries) {
    const base = baseSource(e);
    if (base === null) continue;
    labelsSeen.add(base);
    if (e.sessionId) {
      const set = containersByLabel.get(base) ?? new Set<string>();
      set.add(e.sessionId);
      containersByLabel.set(base, set);
    }
  }
  // A label two containers share cannot tell them apart: `idx` is the very thing the old
  // and new container of a roll have in common (both are web-0), so only the container id
  // can. It is shown ONLY on those lines.
  const ambiguous = new Set<string>();
  for (const [label, ids] of containersByLabel)
    if (ids.size > 1) ambiguous.add(label);
  // The column appears when there is something to tell apart: more than one label, or one
  // label that is ambiguous (a roll of a single replica). A database or a lone container
  // therefore reads exactly as it always did.
  //
  // Known narrow gap, considered and left: if the OLD container of a roll predates the
  // instance-idx label (0.3.2 already writes it, so only a roll across that upgrade) its
  // lines read "web" while the new one's read "web-0". Two labels, so neither is marked
  // ambiguous and neither gets an id or `(old)`. Graceful, and not worth machinery.
  const showSource = labelsSeen.size > 1 || ambiguous.size > 0;

  // The newest deployment in view is the last one first seen (the list is chronological).
  // `(old)` goes only on lines that already carry an id: two opaque hexes say nothing
  // about which is the new one, and marking every older deployment's lines would be noise
  // in a long history.
  let newest: string | null = null;
  const deploymentsSeen = new Set<string>();
  for (const e of entries) {
    if (e.deploymentId && !deploymentsSeen.has(e.deploymentId)) {
      deploymentsSeen.add(e.deploymentId);
      newest = e.deploymentId;
    }
  }

  const out: LogEntry[] = [];
  const seen = new Set<string | null>();
  for (const e of entries) {
    const key = groupKey(e);
    if (dividers && !seen.has(key)) {
      seen.add(key);
      out.push({
        id: `divider-${e.id}`,
        level: "info",
        message: "",
        divider: dividerLabel(e, labels),
      });
    }
    const base = showSource ? baseSource(e) : null;
    if (base === null) {
      out.push(e);
      continue;
    }
    let source = base;
    if (ambiguous.has(base) && e.sessionId) {
      let suffix = ` · ${e.sessionId.slice(0, SHORT_ID)}`;
      if (e.deploymentId && newest !== null && e.deploymentId !== newest)
        suffix += " (old)";
      // The suffix is what disambiguates, so it is kept whole and the TYPE gives way:
      // truncating the tail would drop `(old)` first, for any type name of 12+ chars.
      const room = MAX_SOURCE_WIDTH - suffix.length;
      source = fitBase(e, base, room) + suffix;
    }
    out.push({ ...e, source });
  }
  return out;
}

/** Longest source text in the view, capped: the column's width in characters. */
export function sourceWidth(entries: readonly LogEntry[]): number {
  let w = 0;
  for (const e of entries) if (e.source) w = Math.max(w, e.source.length);
  return Math.min(w, MAX_SOURCE_WIDTH);
}
