import { describe, expect, it } from "vitest";
import { PROCESS_TYPE_NAME, allowedHealthKinds } from "./process-types";

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
