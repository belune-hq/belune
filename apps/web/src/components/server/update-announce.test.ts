import { describe, expect, it } from "vitest";
import type { SelfUpdateStatus } from "@/lib/api/maintenance";
import {
  INITIAL_ANNOUNCE_STATE,
  nextAnnounce,
  type AnnounceState,
} from "./update-announce";

/** Feeds a sequence of readings through the reducer the way the card's effect
 *  does, and reports which of them raised the toast. */
function announcements(readings: (SelfUpdateStatus | undefined)[]): number[] {
  let state: AnnounceState = INITIAL_ANNOUNCE_STATE;
  const fired: number[] = [];
  readings.forEach((reading, i) => {
    const result = nextAnnounce(state, reading);
    state = result.state;
    if (result.announce) fired.push(i);
  });
  return fired;
}

const idle: SelfUpdateStatus = { state: "idle" };
const running: SelfUpdateStatus = { state: "running", target: "0.1.99" };
const failed: SelfUpdateStatus = {
  state: "failed",
  target: "0.1.99",
  reason: "manifest has no 0.1.99",
};

describe("nextAnnounce", () => {
  it("announces a failure this page watched happen", () => {
    expect(announcements([idle, running, failed])).toEqual([2]);
  });

  it("stays silent about a failure that was already failed on arrival", () => {
    // An attempt from hours ago. The panel renders it on load; a toast would be
    // nagging about something the operator has already read.
    expect(announcements([failed])).toEqual([]);
  });

  it("announces once however many times the same failure is re-read", () => {
    // The status query keeps returning the same failed attempt on every poll
    // and every refetch, and the toast uses a fixed id — re-raising it would
    // keep resetting a toast the operator is trying to dismiss.
    expect(announcements([running, failed, failed, failed, failed])).toEqual([
      1,
    ]);
  });

  it("announces a second update that fails the same way", () => {
    // The signature alone would suppress this, since target and reason repeat.
    // A non-failed reading in between is what clears it.
    expect(announcements([running, failed, running, failed])).toEqual([1, 3]);
  });

  it("announces again when the same update fails for a new reason", () => {
    const other: SelfUpdateStatus = {
      state: "failed",
      target: "0.1.99",
      reason: "could not pull 0.1.99",
    };
    expect(announcements([running, failed, other])).toEqual([1, 2]);
  });

  it("distinguishes failures that differ only by target", () => {
    const otherTarget: SelfUpdateStatus = { ...failed, target: "0.1.98" };
    expect(announcements([running, failed, otherTarget])).toEqual([1, 2]);
  });

  it("ignores a reading with no state, including before the query resolves", () => {
    expect(announcements([undefined, running, failed])).toEqual([2]);
    expect(announcements([{} as SelfUpdateStatus, failed])).toEqual([]);
  });

  it("treats a failure with no reason as a distinct signature, not a repeat", () => {
    const reasonless: SelfUpdateStatus = { state: "failed", target: "0.1.99" };
    expect(announcements([running, reasonless, failed])).toEqual([1, 2]);
  });

  it("leaves the state object untouched when nothing is announced", () => {
    // The effect stores this back into a ref; returning a fresh object on every
    // poll would be harmless but makes the reducer's intent less clear.
    const state = INITIAL_ANNOUNCE_STATE;
    expect(nextAnnounce(state, failed).state).toBe(state);
    expect(nextAnnounce(state, undefined).state).toBe(state);
  });
});

// The logic as it was written inline in update-section.tsx before the
// extraction, transcribed verbatim from the two refs and the effect body. Kept
// so the refactor can be shown to decide identically rather than argued to.
function originalInline(readings: (SelfUpdateStatus | undefined)[]): number[] {
  let sawLiveAttempt = false;
  let announced = "";
  const fired: number[] = [];
  readings.forEach((attempt, i) => {
    if (!attempt?.state) return;
    if (attempt.state !== "failed") {
      sawLiveAttempt = true;
      announced = "";
      return;
    }
    if (!sawLiveAttempt) return;
    const signature = `${attempt.target}|${attempt.reason ?? ""}`;
    if (announced === signature) return;
    announced = signature;
    fired.push(i);
  });
  return fired;
}

describe("nextAnnounce is the inline logic it replaced", () => {
  it("decides identically for every reading sequence up to length 4", () => {
    const alphabet: (SelfUpdateStatus | undefined)[] = [
      undefined,
      { state: "idle" },
      { state: "running", target: "0.1.99" },
      { state: "failed", target: "0.1.99", reason: "manifest has no 0.1.99" },
      { state: "failed", target: "0.1.99", reason: "could not pull 0.1.99" },
      { state: "failed", target: "0.1.98" },
    ];

    let sequences: (SelfUpdateStatus | undefined)[][] = [[]];
    let checked = 0;
    for (let length = 1; length <= 4; length++) {
      sequences = sequences.flatMap((seq) =>
        alphabet.map((reading) => [...seq, reading]),
      );
      for (const seq of sequences) {
        expect(announcements(seq)).toEqual(originalInline(seq));
        checked++;
      }
    }

    // 6 + 36 + 216 + 1296
    expect(checked).toBe(1554);
  });
});
