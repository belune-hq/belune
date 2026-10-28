import { describe, expect, it } from "vitest";
import {
  MAX_TRUSTED_PROXIES,
  addRanges,
  describeTrusted,
  formatRanges,
  overlapsOf,
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

describe("overlapsOf", () => {
  const nets = [
    { name: "belune-proj-a", subnet: "172.18.0.0/16" },
    { name: "belune-default", subnet: "192.168.215.0/24" },
    { name: "belune-v6", subnet: "fd00:dead:beef::/48" },
  ];
  const names = (r: string[]) =>
    overlapsOf(r, nets).map((o) => `${o.cidr}>${o.network}`);

  it("finds a range that contains a network, equals it, or sits inside it", () => {
    expect(names(["172.16.0.0/12"])).toEqual(["172.16.0.0/12>belune-proj-a"]);
    expect(names(["172.18.0.0/16"])).toEqual(["172.18.0.0/16>belune-proj-a"]);
    expect(names(["172.18.5.0/24"])).toEqual(["172.18.5.0/24>belune-proj-a"]);
    expect(names(["192.168.215.7"])).toEqual(["192.168.215.7>belune-default"]);
  });

  it("does not warn about a range that touches nothing, including one next door", () => {
    expect(
      names([
        "10.0.0.0/8",
        "203.0.113.0/24",
        "172.19.0.0/16",
        "192.168.216.0/24",
      ]),
    ).toEqual([]);
  });

  it("works for IPv6, and never mixes the families", () => {
    expect(names(["fd00::/8"])).toEqual(["fd00::/8>belune-v6"]);
    expect(names(["fd00:dead:beef:1::/64"])).toEqual([
      "fd00:dead:beef:1::/64>belune-v6",
    ]);
    expect(names(["2606:4700::/32"])).toEqual([]);
    // 0.0.0.0/8 and ::/8 are different families, whatever their bits say.
    expect(overlapsOf(["::/16"], [{ name: "x", subnet: "0.0.0.0/8" }])).toEqual(
      [],
    );
  });

  it("reads an IPv4-mapped range as the IPv4 range, as the server does", () => {
    expect(names(["::ffff:172.18.0.0/112"])).toEqual([
      "::ffff:172.18.0.0/112>belune-proj-a",
    ]);
    expect(names(["::ffff:10.0.0.0/104"])).toEqual([]);
  });

  it("claims nothing about what it cannot parse, rather than guessing", () => {
    for (const bad of [
      "cloudflare",
      "172.18.0.0/99",
      "172.18.0",
      "172.18.0.256",
      "fe80::1%eth0",
      "1:2:3:4:5:6:7:8:9",
      "::/abc",
      "",
    ]) {
      expect(names([bad]), bad).toEqual([]);
    }
    expect(
      overlapsOf(["172.18.0.0/16"], [{ name: "x", subnet: "not-a-cidr" }]),
    ).toEqual([]);
  });

  it("agrees with the server's own answer for the cases the handler test pins", () => {
    // TestTrustedProxies_SaysWhenARangeOverlapsANetworkCaddyIsOn
    const server = [{ name: "belune-proj-a", subnet: "172.18.0.0/16" }];
    expect(overlapsOf(["172.16.0.0/12"], server)).toEqual([
      {
        cidr: "172.16.0.0/12",
        network: "belune-proj-a",
        subnet: "172.18.0.0/16",
      },
    ]);
    expect(overlapsOf(["10.0.0.0/8"], server)).toEqual([]);
  });
});
