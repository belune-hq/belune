import { describe, expect, it } from "vitest";
import { mergeLogEntries } from "./merge";
import type { LogEntry } from "./parse";

// The same physical line as the two sources actually represent it. These are not
// invented shapes: history is a row read back through Postgres (UUID id,
// microsecond-truncated timestamp), live is the WebSocket payload built in
// `logcollector/collector.go` (synthetic id, Docker's nanosecond clock).
function historical(
  message: string,
  recordedAt: string,
  id = `row-${message}`,
): LogEntry {
  return { id, level: "info", stream: "stdout", message, recordedAt };
}

function liveLine(
  message: string,
  recordedAt: string,
  id = `live-${message}`,
): LogEntry {
  return { id, level: "info", stream: "stdout", message, recordedAt };
}

describe("mergeLogEntries", () => {
  it("shows a line held by both sources only once", () => {
    // The bug this function exists for. The collector persists before it
    // publishes, so once history refetches it returns the row for a line the
    // live buffer is still holding.
    const result = mergeLogEntries(
      [historical("listening on port 3000", "2026-10-09T06:00:00.123457Z")],
      [liveLine("listening on port 3000", "2026-10-09T06:00:00.123456789Z")],
    );

    expect(result).toHaveLength(1);
    expect(result[0].message).toBe("listening on port 3000");
  });

  it("matches the two copies despite neither id nor timestamp string agreeing", () => {
    // Guards the two reasons the obvious implementations do not work: the ids
    // are from different namespaces, and the timestamps differ in precision
    // because one has been through Postgres and the other has not.
    const fromHistory = historical(
      "ready",
      "2026-10-09T06:00:00.123457Z",
      "8f3b1c22-0000-4000-8000-000000000001",
    );
    const fromSocket = liveLine(
      "ready",
      "2026-10-09T06:00:00.123456789Z",
      "live-7",
    );

    expect(fromHistory.id).not.toBe(fromSocket.id);
    expect(fromHistory.recordedAt).not.toBe(fromSocket.recordedAt);
    expect(mergeLogEntries([fromHistory], [fromSocket])).toHaveLength(1);
  });

  it("keeps the persisted copy, so the surviving row carries the real id", () => {
    // React keys on `id`. Keeping the live copy would make the key change under
    // the component the moment history caught up with it.
    const result = mergeLogEntries(
      [historical("boot", "2026-10-09T06:00:00.100000Z", "real-uuid")],
      [liveLine("boot", "2026-10-09T06:00:00.100000Z", "live-1")],
    );

    expect(result).toHaveLength(1);
    expect(result[0].id).toBe("real-uuid");
  });

  it("keeps two distinct lines that share a millisecond", () => {
    // The reason this is content-keyed rather than a high-water mark on the
    // newest historical timestamp: a busy container emits several different
    // lines per millisecond, and a boundary cut would silently drop them.
    const result = mergeLogEntries(
      [historical("GET /a 200", "2026-10-09T06:00:00.500000Z")],
      [
        liveLine("GET /b 200", "2026-10-09T06:00:00.500000Z"),
        liveLine("GET /c 500", "2026-10-09T06:00:00.500000Z"),
      ],
    );

    expect(result.map((e) => e.message)).toEqual([
      "GET /a 200",
      "GET /b 200",
      "GET /c 500",
    ]);
  });

  it("collapses a cross-source repeat within one millisecond — the accepted trade", () => {
    // The one case that loses a line: history and live disagree by less than a
    // millisecond about the same text, so they cannot be told apart and the
    // live copy goes. Both render identically, so the loss is cosmetic.
    const result = mergeLogEntries(
      [historical("tick", "2026-10-09T06:00:00.200000Z", "row-1")],
      [liveLine("tick", "2026-10-09T06:00:00.200000Z", "live-1")],
    );

    expect(result).toHaveLength(1);
    expect(result[0]?.id).toBe("row-1");
  });

  it("keeps a line history returned twice, because the count is the message", () => {
    // ⚠️ Dedupe is cross-source ONLY. A container in a retry loop prints the
    // same text several times inside one millisecond, and history returns every
    // copy — collapsing those would throw away the one thing a repeated error
    // tells the reader, which is how often it happened. Caught in review: the
    // first version of this module deduped the concatenated list and lost them.
    const result = mergeLogEntries(
      [
        historical("connection refused", "2026-10-09T06:00:00.200111Z", "r-1"),
        historical("connection refused", "2026-10-09T06:00:00.200222Z", "r-2"),
        historical("connection refused", "2026-10-09T06:00:00.200333Z", "r-3"),
      ],
      [],
    );

    expect(result).toHaveLength(3);
    expect(result.map((e) => e.id)).toEqual(["r-1", "r-2", "r-3"]);
  });

  it("keeps a repeat the live socket delivers twice, for the same reason", () => {
    const result = mergeLogEntries(
      [],
      [
        liveLine("tick", "2026-10-09T06:00:00.200000Z", "live-1"),
        liveLine("tick", "2026-10-09T06:00:00.200000Z", "live-2"),
      ],
    );

    expect(result).toHaveLength(2);
  });

  it("separates the same message on different streams", () => {
    const result = mergeLogEntries(
      [],
      [
        {
          ...liveLine("done", "2026-10-09T06:00:00.300000Z", "live-1"),
          stream: "stdout",
        },
        {
          ...liveLine("done", "2026-10-09T06:00:00.300000Z", "live-2"),
          stream: "stderr",
        },
      ],
    );

    expect(result).toHaveLength(2);
  });

  it("orders the result chronologically rather than history-then-live", () => {
    const result = mergeLogEntries(
      [
        historical("second", "2026-10-09T06:00:02.000000Z"),
        historical("fourth", "2026-10-09T06:00:04.000000Z"),
      ],
      [
        liveLine("fifth", "2026-10-09T06:00:05.000000Z"),
        // Older than history's newest line — clock skew between the collector
        // and the database, or a filter mismatch, can produce this.
        liveLine("third", "2026-10-09T06:00:03.000000Z"),
      ],
    );

    expect(result.map((e) => e.message)).toEqual([
      "second",
      "third",
      "fourth",
      "fifth",
    ]);
  });

  it("puts history ahead of a live line recorded in the same millisecond", () => {
    const result = mergeLogEntries(
      [historical("from history", "2026-10-09T06:00:00.000000Z")],
      [liveLine("from socket", "2026-10-09T06:00:00.000000Z")],
    );

    expect(result.map((e) => e.message)).toEqual([
      "from history",
      "from socket",
    ]);
  });

  it("never collapses two dividers, which are positional furniture", () => {
    // Dividers are interleaved after merging, but a merge must not treat two of
    // them as the same line if one is ever passed through.
    const divider: LogEntry = {
      id: "divider-1",
      level: "info",
      message: "",
      divider: "Earlier logs",
    };
    const result = mergeLogEntries(
      [divider, { ...divider, id: "divider-2" }],
      [],
    );

    expect(result).toHaveLength(2);
  });

  it("leaves an entry with no timestamp beside the line it arrived with", () => {
    // It must not sort to an edge: hoisting an undated line to the top of the
    // view would misattribute it to the start of the session.
    const result = mergeLogEntries(
      [
        historical("first", "2026-10-09T06:00:01.000000Z"),
        {
          id: "no-ts",
          level: "info",
          message: "continuation",
          recordedAt: null,
        },
        historical("last", "2026-10-09T06:00:09.000000Z"),
      ],
      [],
    );

    expect(result.map((e) => e.message)).toEqual([
      "first",
      "continuation",
      "last",
    ]);
  });

  it("passes a single source through unchanged", () => {
    const only = [
      historical("a", "2026-10-09T06:00:01.000000Z"),
      historical("b", "2026-10-09T06:00:02.000000Z"),
    ];

    expect(mergeLogEntries(only, [])).toEqual(only);
    expect(mergeLogEntries([], only)).toEqual(only);
    expect(mergeLogEntries([], [])).toEqual([]);
  });

  describe("container identity", () => {
    const at = "2026-10-09T06:00:00.123Z";
    const line = (
      sessionId: string | null | undefined,
      id: string,
    ): LogEntry => ({
      id,
      level: "info",
      stream: "stdout",
      message: "tick",
      recordedAt: at,
      sessionId,
    });

    it("keeps the same line from two containers: they are two lines", () => {
      const result = mergeLogEntries(
        [line("web-ctr", "a")],
        [line("queue-ctr", "b")],
      );
      expect(result).toHaveLength(2);
    });

    it("still collapses the same line from the same container across sources", () => {
      const result = mergeLogEntries(
        [line("web-ctr", "a")],
        [line("web-ctr", "b")],
      );
      expect(result).toHaveLength(1);
      expect(result[0].id).toBe("a");
    });

    it("keeps the previous behaviour for lines with no container (unassigned)", () => {
      // No protection and no regression: both null -> collapsed as before.
      const result = mergeLogEntries([line(null, "a")], [line(null, "b")]);
      expect(result).toHaveLength(1);
    });
  });
});
