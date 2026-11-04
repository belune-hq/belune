import { describe, expect, it } from "vitest";
import {
  PROCESS_TYPE_NAME,
  allowedHealthKinds,
  resolveHealth,
} from "./process-types";

// This mirrors the server's resolveHealthCheck. The branch that matters is the last:
// a non-routable type must never inherit the application's http check, because a
// worker listens on nothing and would be permanently unhealthy.
describe("resolveHealth", () => {
  it("uses the type's own check when it sets one", () => {
    expect(
      resolveHealth({ routable: false, health_check_type: "command" }, "http"),
    ).toEqual({ kind: "command", source: "set" });
    expect(
      resolveHealth({ routable: true, health_check_type: "none" }, "http"),
    ).toEqual({ kind: "none", source: "set" });
  });

  it("lets the routable type inherit the application's", () => {
    expect(
      resolveHealth({ routable: true, health_check_type: null }, "http"),
    ).toEqual({ kind: "http", source: "inherited" });
    expect(
      resolveHealth({ routable: true, health_check_type: null }, "command"),
    ).toEqual({ kind: "command", source: "inherited" });
  });

  it("NEVER lets a non-routable type inherit http", () => {
    for (const app of ["http", "command", "none"] as const) {
      expect(
        resolveHealth({ routable: false, health_check_type: null }, app),
      ).toEqual({ kind: "none", source: "default" });
    }
  });
});

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
