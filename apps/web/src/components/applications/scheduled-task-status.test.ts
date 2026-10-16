import { describe, expect, it } from "vitest";
import { runLabel, runTone } from "./scheduled-task-status";

describe("scheduled task run status", () => {
  it("never paints a skip as a failure or as in-progress", () => {
    expect(runTone("skipped")).toBe("text-text-faint");
    expect(runTone("skipped")).not.toBe(runTone("failed"));
    expect(runTone("skipped")).not.toBe(runTone("running"));
  });

  it("treats a timeout as an error", () => {
    expect(runTone("timed_out")).toBe(runTone("failed"));
  });

  it("labels a timeout with its limit, never the bare exit code 124", () => {
    const label = runLabel(
      { status: "timed_out", exit_code: 124 },
      { timeout_seconds: 300 },
    );
    expect(label).toBe("Timed out (300s)");
    expect(label).not.toContain("124");
  });

  it("shows the exit code on an ordinary failure", () => {
    expect(
      runLabel({ status: "failed", exit_code: 3 }, { timeout_seconds: 300 }),
    ).toBe("Failed (exit 3)");
  });
});
