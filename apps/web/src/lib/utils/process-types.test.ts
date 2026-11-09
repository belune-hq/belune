import { describe, expect, it } from "vitest";
import {
  PROCESS_TYPE_NAME,
  allowedHealthKinds,
  validateLimit,
  validateMemoryMb,
  validateReplicas,
} from "./process-types";

describe("allowedHealthKinds", () => {
  it("offers http only on the routable type", () => {
    expect(allowedHealthKinds(true)).toContain("http");
    expect(allowedHealthKinds(false)).not.toContain("http");
    expect(allowedHealthKinds(false)).toEqual(["command", "none"]);
  });
});

describe("PROCESS_TYPE_NAME", () => {
  it("matches the server's pattern", () => {
    for (const ok of ["web", "worker", "q-1", "a".repeat(20)]) {
      expect(PROCESS_TYPE_NAME.test(ok)).toBe(true);
    }
    for (const bad of ["", "Worker", "1w", "has_under", "a".repeat(21)]) {
      expect(PROCESS_TYPE_NAME.test(bad)).toBe(false);
    }
  });
});

describe("validateReplicas", () => {
  const worker = { routable: false, hasVolumes: false };
  const web = { routable: true, hasVolumes: false };

  it("accepts whole numbers from 0 to 20 on a worker, 0 being a pause", () => {
    for (const n of ["0", "1", "5", "20"])
      expect(validateReplicas(n, worker)).toBeUndefined();
  });

  it("refuses non-numbers, fractions and out-of-range values", () => {
    expect(validateReplicas("", worker)).toBeTruthy();
    expect(validateReplicas("abc", worker)).toBeTruthy();
    expect(validateReplicas("1.5", worker)).toBeTruthy();
    expect(validateReplicas("-1", worker)).toBeTruthy();
    expect(validateReplicas("21", worker)).toBeTruthy();
  });

  it("refuses 0 on the routable type, in the operator's terms", () => {
    expect(validateReplicas("0", web)).toMatch(/cannot be paused/);
    expect(validateReplicas("1", web)).toBeUndefined();
    expect(validateReplicas("3", web)).toBeUndefined();
  });

  it("refuses more than one replica of ANY type when the application has volumes", () => {
    for (const routable of [true, false]) {
      expect(validateReplicas("2", { routable, hasVolumes: true })).toMatch(
        /volume/,
      );
      expect(
        validateReplicas("1", { routable, hasVolumes: true }),
      ).toBeUndefined();
    }
    expect(
      validateReplicas("0", { routable: false, hasVolumes: true }),
    ).toBeUndefined();
  });
});

describe("limits", () => {
  it("treats blank as inherit and rejects non-positive or non-numeric values", () => {
    expect(validateLimit("", "CPU")).toBeUndefined();
    expect(validateLimit("  ", "CPU")).toBeUndefined();
    expect(validateLimit("0.5", "CPU")).toBeUndefined();
    expect(validateLimit("0", "CPU")).toBeTruthy();
    expect(validateLimit("-1", "CPU")).toBeTruthy();
    expect(validateLimit("x", "CPU")).toBeTruthy();
  });

  it("holds memory to Docker's floor", () => {
    expect(validateMemoryMb("")).toBeUndefined();
    expect(validateMemoryMb("128")).toBeUndefined();
    expect(validateMemoryMb("2")).toMatch(/at least/);
  });
});
