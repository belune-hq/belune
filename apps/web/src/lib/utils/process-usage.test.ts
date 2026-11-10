import { describe, expect, it } from "vitest";
import type {
  AppInstanceMetricPoint,
  AppMetricPoint,
  ProcessType,
} from "@/lib/types";
import {
  comparableCeiling,
  describeCoverage,
  sumAllContainers,
  summariseProcess,
} from "./process-usage";

const inst = (
  over: Partial<AppInstanceMetricPoint>,
): AppInstanceMetricPoint => ({
  instance_id: "i",
  process_type: "web",
  idx: 1,
  routable: true,
  cpu_percent: 10,
  memory_usage: 100,
  memory_limit: 0,
  network_rx_bytes: 1,
  network_tx_bytes: 2,
  ...over,
});

const point = (over: Partial<AppMetricPoint>): AppMetricPoint => ({
  cpu_percent: null,
  memory_usage: null,
  memory_limit: null,
  network_rx_bytes: null,
  network_tx_bytes: null,
  recorded_at: "2026-11-10T00:00:00Z",
  ...over,
});

describe("sumAllContainers", () => {
  it("sums web replicas AND workers, not just the routable type", () => {
    const p = point({
      cpu_percent: 20, // the routable total the server reports: NOT what we show
      instances: [
        inst({ idx: 1, cpu_percent: 10, memory_usage: 100 }),
        inst({ idx: 2, cpu_percent: 10, memory_usage: 100 }),
        inst({
          process_type: "queue",
          routable: false,
          cpu_percent: 40,
          memory_usage: 900,
        }),
      ],
    });
    const u = sumAllContainers(p);
    expect(u.cpu).toBe(60);
    expect(u.memory).toBe(1100);
    expect(u.rx).toBe(3);
    expect(u.tx).toBe(6);
  });

  it("is null, never 0, when there are no instance samples", () => {
    expect(sumAllContainers(point({}))).toEqual({
      cpu: null,
      memory: null,
      rx: null,
      tx: null,
    });
    expect(sumAllContainers(undefined).cpu).toBeNull();
    expect(sumAllContainers(point({ instances: [] })).memory).toBeNull();
  });

  it("keeps a genuine 0 as 0", () => {
    expect(
      sumAllContainers(point({ instances: [inst({ cpu_percent: 0 })] })).cpu,
    ).toBe(0);
  });
});

describe("describeCoverage", () => {
  it("says the sum is partial, and how partial", () => {
    const c = describeCoverage(point({ sampled: 3, expected: 4 }));
    expect(c).toEqual({
      partial: true,
      text: "Sum of 3 of 4 containers (1 not sampled)",
    });
  });

  it("labels a full multi-container sum as covering all processes", () => {
    expect(describeCoverage(point({ sampled: 4, expected: 4 }))).toEqual({
      partial: false,
      text: "All processes · 4 containers",
    });
  });

  it("says nothing for a single fully sampled container: the figure means what it always did", () => {
    expect(describeCoverage(point({ sampled: 1, expected: 1 }))).toBeNull();
  });

  it("says nothing when the counts are absent (history points carry none)", () => {
    expect(describeCoverage(point({}))).toBeNull();
    expect(describeCoverage(null)).toBeNull();
  });

  it("still reports partial for a single container that was expected but not sampled", () => {
    expect(describeCoverage(point({ sampled: 0, expected: 1 }))?.partial).toBe(
      true,
    );
  });
});

describe("comparableCeiling", () => {
  const web = (n: number) =>
    Array.from({ length: n }, (_, i) => inst({ idx: i + 1 }));

  it("shows the ceiling when usage and ceiling describe the same containers", () => {
    const p = point({
      memory_limit: 2048,
      routable_sampled: 2,
      routable_expected: 2,
      instances: web(2),
    });
    expect(comparableCeiling(p)).toBe(2048);
  });

  it("hides it when a worker is in the usage sum: different sets", () => {
    const p = point({
      memory_limit: 2048,
      routable_sampled: 2,
      routable_expected: 2,
      instances: [...web(2), inst({ process_type: "queue", routable: false })],
    });
    expect(comparableCeiling(p)).toBeNull();
  });

  it("hides it under partial routable coverage: the denominators would differ", () => {
    const p = point({
      memory_limit: 3072,
      routable_sampled: 2,
      routable_expected: 3,
      instances: web(2),
    });
    expect(comparableCeiling(p)).toBeNull();
  });

  it("hides it when a routable replica is unlimited (null ceiling)", () => {
    const p = point({
      memory_limit: null,
      routable_sampled: 1,
      routable_expected: 1,
      instances: web(1),
    });
    expect(comparableCeiling(p)).toBeNull();
  });
});

describe("summariseProcess", () => {
  const pt = (over: Partial<ProcessType>): ProcessType =>
    ({
      id: "x",
      name: "queue",
      routable: false,
      replicas: 1,
      instances: [],
      ...over,
    }) as ProcessType;
  const i = (status: string) => ({
    id: status,
    idx: 1,
    container_name: "c",
    container_id: null,
    status,
  });

  it("reports a worker that is down while web is up", () => {
    expect(summariseProcess(pt({ instances: [i("running")] }))).toEqual({
      label: "Running",
      tone: "ready",
    });
    expect(summariseProcess(pt({ instances: [i("stopped")] }))).toEqual({
      label: "Stopped",
      tone: "neutral",
    });
    expect(summariseProcess(pt({ instances: [i("error")] })).tone).toBe(
      "error",
    );
  });

  it("reports partial replicas", () => {
    const s = summariseProcess(
      pt({
        replicas: 3,
        instances: [i("running"), i("running"), i("stopped")],
      }),
    );
    expect(s).toEqual({ label: "2 of 3 running", tone: "building" });
  });

  it("calls a type with zero replicas paused and one with no rows not deployed", () => {
    expect(summariseProcess(pt({ replicas: 0 })).label).toBe("Paused");
    expect(summariseProcess(pt({ replicas: 1, instances: [] })).label).toBe(
      "Not deployed",
    );
  });
});
