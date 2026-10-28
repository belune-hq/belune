import { describe, expect, it } from "vitest";
import {
  MAX_TRUSTED_PROXIES,
  addRanges,
  describeTrusted,
  formatRanges,
  parseRanges,
  sameRanges,
  validateRanges,
} from "./trusted-proxies-view";

describe("parseRanges", () => {
  it("reads lines, commas and spaces, keeping order and dropping repeats", () => {
    expect(
      parseRanges("10.0.0.0/8\n192.168.0.0/16, 10.0.0.0/8  172.16.0.0/12\n"),
    ).toEqual(["10.0.0.0/8", "192.168.0.0/16", "172.16.0.0/12"]);
    expect(parseRanges("  \n ,, ")).toEqual([]);
  });
});

describe("validateRanges", () => {
  it("accepts nothing, which turns trusting off", () => {
    expect(validateRanges("")).toBeUndefined();
    expect(validateRanges("  \n")).toBeUndefined();
  });

  it("accepts addresses and ranges of both families", () => {
    expect(
      validateRanges("10.0.0.0/8\n172.20.0.5\n2606:4700::/32"),
    ).toBeUndefined();
  });

  it("names the first thing that is not a range", () => {
    expect(validateRanges("10.0.0.0/8\ncloudflare")).toMatch(
      /"cloudflare" is not an IP/,
    );
    expect(validateRanges("https://x.example")).toMatch(/not an IP/);
  });

  it("refuses ranges too wide to trust, with the floor in the message", () => {
    expect(validateRanges("0.0.0.0/0")).toMatch(/too wide.*\/8/);
    expect(validateRanges("0.0.0.0/7")).toMatch(/too wide/);
    expect(validateRanges("::/0")).toMatch(/too wide.*\/16/);
    expect(validateRanges("fd00::/8")).toMatch(/too wide/);
    expect(validateRanges("10.0.0.0/8")).toBeUndefined();
    expect(validateRanges("fd00::/16")).toBeUndefined();
  });

  it("bounds the list and how much of a bad entry it echoes", () => {
    const many = Array.from(
      { length: MAX_TRUSTED_PROXIES + 1 },
      () => "10.0.0.1",
    );
    // Repeats collapse, so build distinct ones.
    const distinct = many.map(
      (_, i) => `10.0.${Math.floor(i / 250)}.${(i % 250) + 1}`,
    );
    expect(validateRanges(distinct.join("\n"))).toMatch(/At most 64/);
    expect(validateRanges("x".repeat(500))!.length).toBeLessThan(120);
  });
});

describe("addRanges", () => {
  it("adds a preset to what is there without repeating anything", () => {
    expect(addRanges("10.0.0.0/8", ["173.245.48.0/20", "10.0.0.0/8"])).toBe(
      "10.0.0.0/8\n173.245.48.0/20",
    );
    expect(addRanges("", ["173.245.48.0/20"])).toBe("173.245.48.0/20");
  });
});

describe("sameRanges", () => {
  it("ignores order and nothing else", () => {
    expect(sameRanges(["a", "b"], ["b", "a"])).toBe(true);
    expect(sameRanges(["a"], ["a", "b"])).toBe(false);
    expect(sameRanges(["a", "b"], ["a", "c"])).toBe(false);
    expect(sameRanges([], [])).toBe(true);
  });
});

describe("describeTrusted / formatRanges", () => {
  it("says what none means, and counts", () => {
    expect(describeTrusted(0)).toMatch(/address that connects to Caddy/);
    expect(describeTrusted(1)).toBe("1 range trusted.");
    expect(describeTrusted(3)).toBe("3 ranges trusted.");
    expect(formatRanges(["a", "b"])).toBe("a\nb");
  });
});
