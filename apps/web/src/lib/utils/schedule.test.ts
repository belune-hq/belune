import { describe, expect, it } from "vitest";
import { cronError, describeCron, describeCronOrNull } from "./schedule";

describe("describeCron", () => {
  it.each([
    ["* * * * *", "Every minute"],
    ["*/5 * * * *", "Every 5 minutes"],
    ["15 * * * *", "At 15 minutes past the hour"],
    ["30 3 * * *", "At 03:30"],
    ["0 0 * * *", "Every day at midnight"],
  ])("describes %s", (expr, want) => {
    expect(describeCron(expr)).toBe(want);
  });

  it("describes shapes the old lookup could not", () => {
    expect(describeCron("0 3 * * 1-5")).toBe("At 03:00, Monday through Friday");
  });

  it("understands the descriptors the backend accepts", () => {
    expect(describeCron("@daily")).toBe("Every day at midnight");
    expect(describeCron("@every 1h30m")).toBe("Every 1h30m");
  });

  it("returns null for partial or malformed input instead of throwing", () => {
    for (const partial of [
      "",
      "0 3 * *",
      "0 3",
      "nonsense",
      "@bogus",
      "* * * * * *",
    ]) {
      expect(() => describeCronOrNull(partial)).not.toThrow();
    }
    expect(describeCronOrNull("0 3 * *")).toBeNull();
    expect(describeCronOrNull("@bogus")).toBeNull();
    expect(describeCron("0 3 * *")).toBe("0 3 * *");
  });
});

describe("cronError", () => {
  it.each([
    "99 99 99 99 99",
    "a b c d e",
    "* * * * bogus",
    "0 99 * * *",
    "0 3 * *",
    "* * * * * *",
    "@bogus",
  ])("rejects %s", (expr) => {
    expect(cronError(expr)).toBeDefined();
  });

  it.each([
    "0 3 * * *",
    "*/5 * * * 1-5",
    "@daily",
    "@every 1h30m",
    "@every 90s",
  ])("accepts %s", (expr) => {
    expect(cronError(expr, { minMinute: true })).toBeUndefined();
  });

  it("treats empty as the caller's business", () => {
    expect(cronError("  ")).toBeUndefined();
  });

  it("applies the one-minute floor only when asked, mirroring ParseMinimumMinute", () => {
    expect(cronError("@every 30s", { minMinute: true })).toMatch(/one minute/);
    expect(cronError("@every 1m30s", { minMinute: true })).toBeUndefined();
    expect(cronError("@every 30s")).toBeUndefined();
    expect(cronError("@every soon")).toMatch(/duration/);
  });
});
