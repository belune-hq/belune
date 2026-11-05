import type { LogEntry } from "@/components/logs/parse";

// Merges the two sources the container log viewer reads from — the persisted
// history query and the live WebSocket buffer — into one chronological list
// with no line shown twice.
//
// Why this is not a concatenation: the collector publishes a line to Redis only
// AFTER its row is durable (`logcollector/collector.go`, "so a live viewer never
// sees a line that was not stored"). Every live line therefore already exists in
// the database, so any refetch of history — and history inherits the global 60s
// `staleTime` from `main.tsx`, so a tab-focus is enough — returns a second copy
// of lines the live buffer is still holding.
//
// The two copies cannot be matched on identity:
//
//   - not by `id`: history carries the row's UUID, while the live buffer mints
//     synthetic `live-N` ids that never equal one.
//   - not by `recordedAt` as a string: live publishes RFC3339Nano taken from
//     Docker's own nanosecond clock, while the stored value has been truncated
//     to Postgres's microsecond resolution and comes back re-formatted. The same
//     line reads `…123456789Z` live and `…123457Z` from history.
//
// So a line is identified by its content at millisecond resolution, which is
// the precision both representations survive down to. The opposite trade — a
// high-water mark on the newest historical timestamp — would instead discard
// *different* text that happened to land on the boundary instant, which is a
// silent loss rather than a cosmetic one.
function dedupeKey(entry: LogEntry): string | null {
  // A divider is positional furniture rather than a log line; it is inserted
  // after merging and must never be collapsed against anything.
  if (entry.divider !== undefined) return null;
  if (!entry.recordedAt) return null;
  const ms = Date.parse(entry.recordedAt);
  if (Number.isNaN(ms)) return null;
  // The container is part of a line's identity: two containers (a web and a
  // queue worker, or two replicas) can print the same text in the same
  // millisecond, and those are two lines. `sessionId` is the container id on both
  // sides -- history carries the row's, live carries the payload's, and the
  // collector sets it from Docker's own container id, so it is present on every
  // collected line. A line with none (the unassigned bucket) keeps the previous
  // key, so it gains no protection and loses none.
  return `${ms}|${entry.stream ?? ""}|${entry.sessionId ?? ""}|${entry.message}`;
}

export function mergeLogEntries(
  historical: LogEntry[],
  live: LogEntry[],
): LogEntry[] {
  // ⚠️ CROSS-SOURCE only: `seen` is seeded from history, and only `live` is
  // filtered against it.
  //
  // Deduping the two concatenated would also collapse two identical lines that
  // HISTORY ITSELF returned — a container in a retry loop printing the same
  // message several times inside one millisecond — and for a repeated error the
  // count is the information. Nothing about a history refetch justifies that:
  // the duplicate this exists to remove only ever arises one way round, because
  // the collector persists a line before it publishes it, so a live line is
  // already in the database and it is the refetch that returns the second copy.
  //
  // History is kept whole and first, so when a line is present in both, the
  // copy that survives is the persisted one — it carries the real row id, which
  // is what React keys on.
  const seen = new Set<string>();
  for (const entry of historical) {
    const key = dedupeKey(entry);
    if (key !== null) seen.add(key);
  }

  const unique: LogEntry[] = [...historical];
  for (const entry of live) {
    const key = dedupeKey(entry);
    if (key !== null && seen.has(key)) continue;
    unique.push(entry);
  }

  // Dedupe alone usually restores order, because a live line that history does
  // not contain must be newer than everything history returned. That invariant
  // is not worth depending on: it breaks under clock skew between the collector
  // and the database, and under any filter mismatch between the server-side and
  // client-side passes. Sorting explicitly costs one pass and removes the
  // question.
  //
  // An entry with no parseable timestamp inherits the last one seen rather than
  // sorting to an edge, so it stays next to the line it arrived with instead of
  // being hoisted to the top of the view.
  let lastKnown = Number.NEGATIVE_INFINITY;
  const keyed = unique.map((entry, index) => {
    const parsed = entry.recordedAt ? Date.parse(entry.recordedAt) : Number.NaN;
    if (!Number.isNaN(parsed)) lastKnown = parsed;
    return { entry, index, at: lastKnown };
  });

  // Index breaks ties so the result is deterministic and history keeps its
  // place ahead of a live line recorded in the same millisecond.
  keyed.sort((a, b) => (a.at === b.at ? a.index - b.index : a.at - b.at));

  return keyed.map((k) => k.entry);
}
