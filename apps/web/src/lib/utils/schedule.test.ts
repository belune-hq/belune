import { describe, expect, it } from "vitest";
import { describeCron } from "./schedule";

describe("describeCron", () => {
  it.each([
    ["* * * * *", "Every minute"],
    ["*/5 * * * *", "Every 5 minutes"],
    ["15 * * * *", "Every hour at :15"],
    ["30 3 * * *", "Every day at 03:30"],
    ["0 0 * * *", "Every day at midnight"],
  ])("describes %s", (expr, want) => {
    expect(describeCron(expr)).toBe(want);
  });

  it("returns the raw expression rather than guess at an unfamiliar one", () => {
    expect(describeCron("0 3 * * 1-5")).toBe("0 3 * * 1-5");
    expect(describeCron("@daily")).toBe("@daily");
  });
});
