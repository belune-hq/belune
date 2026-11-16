import { describe, expect, it } from "vitest";
import { groupEntries, groupKey, sourceWidth } from "./group";
import type { LogEntry } from "./parse";

let n = 0;
function line(over: Partial<LogEntry>): LogEntry {
  n += 1;
  return {
    id: `l${n}`,
    level: "info",
    message: `m${n}`,
    recordedAt: new Date(Date.UTC(2026, 10, 16, 12, 0, n)).toISOString(),
    ...over,
  };
}

const none = new Map<string, string>();
const dividersOf = (out: LogEntry[]) =>
  out.filter((e) => e.divider !== undefined);
const lines = (out: LogEntry[]) => out.filter((e) => e.divider === undefined);

// The two containers' own ids, as the collector would carry them.
const A1 = "aaaaaaaa1111111111111111";
const A2 = "bbbbbbbb2222222222222222";
const DEP = "dddddddd-0000-0000-0000-000000000001";
const DEP2 = "eeeeeeee-0000-0000-0000-000000000002";

describe("groupEntries: dividers", () => {
  // The defect. Two containers of ONE deployment write alternately; the old model keyed
  // on the container and emitted a divider on every change of key -- one per line.
  it("emits NO divider for two interleaved containers of one deployment", () => {
    const e = [
      line({
        sessionId: A1,
        deploymentId: DEP,
        processType: "web",
        instanceIdx: 0,
      }),
      line({
        sessionId: A2,
        deploymentId: DEP,
        processType: "web",
        instanceIdx: 1,
      }),
      line({
        sessionId: A1,
        deploymentId: DEP,
        processType: "web",
        instanceIdx: 0,
      }),
      line({
        sessionId: A2,
        deploymentId: DEP,
        processType: "web",
        instanceIdx: 1,
      }),
      line({
        sessionId: A1,
        deploymentId: DEP,
        processType: "web",
        instanceIdx: 0,
      }),
    ];
    const out = groupEntries(e, none);
    expect(dividersOf(out)).toHaveLength(0);
    expect(lines(out)).toHaveLength(5);
  });

  // The roll: the new container and the draining old one both log, with different
  // deployment ids. Exactly two dividers, in first-sighting order, time order untouched.
  it("emits exactly one divider per deployment when two generations interleave", () => {
    const e = [
      line({
        sessionId: A1,
        deploymentId: DEP,
        processType: "web",
        instanceIdx: 0,
      }),
      line({
        sessionId: A2,
        deploymentId: DEP2,
        processType: "web",
        instanceIdx: 0,
      }),
      line({
        sessionId: A1,
        deploymentId: DEP,
        processType: "web",
        instanceIdx: 0,
      }),
      line({
        sessionId: A2,
        deploymentId: DEP2,
        processType: "web",
        instanceIdx: 0,
      }),
      line({
        sessionId: A1,
        deploymentId: DEP,
        processType: "web",
        instanceIdx: 0,
      }),
    ];
    const out = groupEntries(e, none);
    const ds = dividersOf(out);
    expect(ds.map((d) => d.divider)).toEqual([
      `#${DEP.slice(0, 7)}`,
      `#${DEP2.slice(0, 7)}`,
    ]);
    // Dividers are furniture: the lines keep strict input order.
    expect(lines(out).map((l) => l.id)).toEqual(e.map((l) => l.id));
    // And each divider sits directly before its group's first line.
    expect(out.indexOf(ds[0])).toBe(0);
    expect(out[out.indexOf(ds[1]) + 1].id).toBe(e[1].id);
  });

  it("keeps a per-container divider for a database, which has no deployment", () => {
    const e = [
      line({ sessionId: A1 }),
      line({ sessionId: A1 }),
      line({ sessionId: A2 }), // the container replaced by an upgrade
      line({ sessionId: A2 }),
    ];
    const out = groupEntries(
      e,
      new Map([
        [A1, "Run · 2h ago"],
        [A2, "Run · now"],
      ]),
    );
    expect(dividersOf(out).map((d) => d.divider)).toEqual([
      "Run · 2h ago",
      "Run · now",
    ]);
    expect(lines(out).every((l) => l.source === undefined)).toBe(true);
  });

  it("emits nothing when everything in view is one group", () => {
    const e = [line({ sessionId: A1 }), line({ sessionId: A1 })];
    expect(dividersOf(groupEntries(e, none))).toHaveLength(0);
  });

  it("labels from the line when the sessions list does not know the deployment yet", () => {
    const e = [
      line({ sessionId: A1, deploymentId: DEP }),
      line({ sessionId: A2, deploymentId: DEP2 }),
    ];
    const known = new Map([[DEP, "#dddddd0 · manual · 2h ago"]]);
    expect(dividersOf(groupEntries(e, known)).map((d) => d.divider)).toEqual([
      "#dddddd0 · manual · 2h ago",
      `#${DEP2.slice(0, 7)}`, // not in the sessions map: the line's own id
    ]);
  });

  it("falls back to the container for application lines from before the column existed", () => {
    expect(groupKey(line({ sessionId: A1 }))).toBe(A1);
    expect(groupKey(line({ sessionId: A1, deploymentId: DEP }))).toBe(DEP);
    expect(groupKey(line({ sessionId: null }))).toBeNull();
    const out = groupEntries(
      [line({ sessionId: null }), line({ sessionId: A1 })],
      none,
    );
    expect(dividersOf(out).map((d) => d.divider)).toEqual([
      "Earlier logs",
      `Run · ${A1.slice(0, 12)}`,
    ]);
  });

  it("gives a divider a stable id, so React does not re-key the list", () => {
    const e = [
      line({ sessionId: A1, deploymentId: DEP }),
      line({ sessionId: A2, deploymentId: DEP2 }),
    ];
    const a = groupEntries(e, none);
    const b = groupEntries(
      [...e, line({ sessionId: A1, deploymentId: DEP })],
      none,
    );
    expect(dividersOf(a).map((d) => d.id)).toEqual(
      dividersOf(b).map((d) => d.id),
    );
  });
});

describe("groupEntries: source labels", () => {
  const web = (idx: number | null, sessionId: string, deploymentId = DEP) =>
    line({ sessionId, deploymentId, processType: "web", instanceIdx: idx });

  it("labels each line {type}-{idx} when the view holds more than one source", () => {
    const out = lines(groupEntries([web(0, A1), web(1, A2)], none));
    expect(out.map((l) => l.source)).toEqual(["web-0", "web-1"]);
  });

  it("renders no column for a single container, so it reads as it always did", () => {
    const out = lines(groupEntries([web(0, A1), web(0, A1)], none));
    expect(out.every((l) => l.source === undefined)).toBe(true);
  });

  it("degrades to the type, then to nothing, for rows with unknown labels", () => {
    const out = lines(
      groupEntries(
        [
          web(null, A1), // idx unknown: permanent for a container created before the label
          line({
            sessionId: A2,
            deploymentId: DEP,
            processType: "worker",
            instanceIdx: 0,
          }),
          line({ sessionId: "ccccccc3", deploymentId: DEP }), // type unknown too
        ],
        none,
      ),
    );
    expect(out.map((l) => l.source)).toEqual(["web", "worker-0", undefined]);
  });

  it("treats the sessions list's -1 as unknown, never as replica -1", () => {
    const out = lines(groupEntries([web(-1, A1), web(1, A2)], none));
    expect(out.map((l) => l.source)).toEqual(["web", "web-1"]);
  });

  it("adds the container id to exactly the lines whose label is shared", () => {
    // web-0 is held by TWO containers (a roll); web-1 by one.
    const out = lines(
      groupEntries(
        [web(0, A1), web(0, A2, DEP2), web(1, "ffffffffffff", DEP2)],
        none,
      ),
    );
    expect(out[0].source).toMatch(/^web-0 · aaaaaaaa/);
    expect(out[1].source).toMatch(/^web-0 · bbbbbbbb/);
    expect(out[2].source).toBe("web-1");
  });

  it("shows no ids when one container holds the label, however many lines it wrote", () => {
    const out = lines(groupEntries([web(0, A1), web(1, A2), web(0, A1)], none));
    expect(out.map((l) => l.source)).toEqual(["web-0", "web-1", "web-0"]);
  });

  it("shows the column for a rolled single replica: one label, two containers", () => {
    const out = lines(groupEntries([web(0, A1), web(0, A2, DEP2)], none));
    expect(out.map((l) => l.source)).toEqual([
      "web-0 · aaaaaaaa (old)",
      "web-0 · bbbbbbbb",
    ]);
  });

  it("marks (old) only where the deployment is not the newest in view", () => {
    const out = lines(
      groupEntries(
        [web(0, A1), web(0, A2, DEP2), web(0, A1), web(0, A2, DEP2)],
        none,
      ),
    );
    expect(out.map((l) => l.source?.includes("(old)"))).toEqual([
      true,
      false,
      true,
      false,
    ]);
  });

  it("orders generations by first sighting, whatever order they resume in", () => {
    // DEP2 is first seen last, so it is the newest even though DEP writes after it.
    const out = lines(
      groupEntries([web(0, A1), web(0, A2, DEP2), web(0, A1)], none),
    );
    expect(out[1].source).not.toMatch(/old/);
    expect(out[2].source).toMatch(/old/);
  });
});

describe("sourceWidth", () => {
  it("is the longest source text, capped", () => {
    const long = line({ source: "x".repeat(80) });
    expect(
      sourceWidth([line({ source: "web-0" }), line({ source: "worker-12" })]),
    ).toBe(9);
    expect(sourceWidth([long])).toBe(30);
    expect(sourceWidth([line({})])).toBe(0);
  });
});
