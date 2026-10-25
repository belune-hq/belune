import { describe, expect, it } from "vitest";
import type {
  CrowdSecAutobanStatus,
  CrowdSecLocalApi,
  CrowdSecPolicy,
  CrowdSecStatus,
} from "@/lib/types";
import {
  autobanEnablePoints,
  BOUNCER_STALE_SECONDS,
  bouncerNotice,
  crowdSecEnablePoints,
  describeAutoban,
  describeCrowdSec,
  isBanTarget,
  reasonText,
  sourceLabel,
  timeLeft,
} from "./crowdsec-view";

const NOW = new Date("2026-10-25T12:00:00Z");
const secondsAgo = (s: number) =>
  new Date(NOW.getTime() - s * 1000).toISOString();

const api = (over: Partial<CrowdSecLocalApi> = {}): CrowdSecLocalApi => ({
  checked_at: NOW.toISOString(),
  reachable: true,
  bouncer_registered: true,
  last_pull: secondsAgo(3),
  ...over,
});

const policy = (over: Partial<CrowdSecPolicy> = {}): CrowdSecPolicy => ({
  enabled: false,
  threshold: 10,
  window_minutes: 10,
  duration_minutes: 60,
  max_active: 25,
  categories: ["attack-sqli"],
  ...over,
});

const run = (
  over: Partial<CrowdSecAutobanStatus> = {},
): CrowdSecAutobanStatus => ({
  active: 0,
  banned: 0,
  skipped: 0,
  capped: false,
  ...over,
});

const status = (over: Partial<CrowdSecStatus> = {}): CrowdSecStatus => ({
  capability: { module: "http.handlers.crowdsec", state: "available" },
  enabled: false,
  state: { active: false, hosts: 0 },
  local_api: api(),
  your_ip: "203.0.113.50",
  autoban: { policy: policy(), status: run(), categories: ["attack-sqli"] },
  ...over,
});

describe("describeCrowdSec", () => {
  // The test that matters: the capability has three answers. If `unknown`
  // rendered as `unavailable`, someone whose Caddy was only restarting would be
  // told their image lacks the module.
  it("keeps the three capability answers apart", () => {
    const unknown = describeCrowdSec(
      status({ capability: { module: "m", state: "unknown" } }),
      NOW,
    );
    const absent = describeCrowdSec(
      status({ capability: { module: "m", state: "unavailable" } }),
      NOW,
    );
    const off = describeCrowdSec(status(), NOW);

    expect(unknown.pill.label).toBe("Cannot tell yet");
    expect(unknown.body).not.toMatch(/does not include/);
    expect(unknown.canEnable).toBe(true);

    expect(absent.body).toMatch(/does not include the CrowdSec module/);
    expect(absent.canEnable).toBe(false);

    expect(off.pill.label).toBe("Off");
    expect(off.canEnable).toBe(true);
  });

  it("says a working bouncer is enforcing, and what it does not cover", () => {
    const d = describeCrowdSec(
      status({ enabled: true, state: { active: true, hosts: 3 } }),
      NOW,
    );
    expect(d.pill).toEqual({ label: "Enforcing", tone: "ready" });
    expect(d.body).toMatch(/3 application hostnames/);
    expect(d.body).toMatch(/dashboard is not covered/);
    expect(d.notice).toBeNull();
  });

  // Caddy fails open: a bouncer that cannot authenticate admits everyone and says
  // nothing. "Configured" must not be shown as "enforcing".
  it("does not say Enforcing when the bouncer has stopped pulling", () => {
    const d = describeCrowdSec(
      status({
        enabled: true,
        state: { active: true, hosts: 1 },
        local_api: api({ last_pull: secondsAgo(BOUNCER_STALE_SECONDS + 60) }),
      }),
      NOW,
    );
    expect(d.pill).toEqual({ label: "Not enforcing", tone: "error" });
    expect(d.notice?.text).toMatch(/rotate its key/);
  });

  it("explains a local API that is down, and keeps a working route described as working", () => {
    const down = describeCrowdSec(
      status({
        enabled: true,
        state: { active: false, hosts: 0, reason: "lapi_unavailable" },
        local_api: api({
          reachable: false,
          error: "the CrowdSec container is not running",
        }),
      }),
      NOW,
    );
    expect(down.pill.label).toBe("Local API down");
    expect(down.body).toMatch(/docker compose up -d/);
    expect(down.notice?.text).toMatch(/not running/);

    const blip = describeCrowdSec(
      status({
        enabled: true,
        state: { active: true, hosts: 2, reason: "lapi_unavailable" },
        local_api: api({ reachable: false }),
      }),
      NOW,
    );
    expect(blip.pill.label).toBe("Enforcing");
    expect(blip.notice?.text).toMatch(
      /keeps enforcing the bans it already holds/,
    );
  });

  it("reports a refusal from Caddy", () => {
    const d = describeCrowdSec(
      status({
        enabled: true,
        state: { active: false, hosts: 1, error: "HTTP 500: nope" },
      }),
      NOW,
    );
    expect(d.pill.tone).toBe("error");
    expect(d.notice).toEqual({ tone: "error", text: "HTTP 500: nope" });
  });

  it("covers the waiting states without alarm", () => {
    const wait = describeCrowdSec(
      status({
        enabled: true,
        state: { active: false, hosts: 0, reason: "no_applications" },
      }),
      NOW,
    );
    expect(wait.pill.tone).toBe("neutral");
    const unknown = describeCrowdSec(
      status({
        enabled: true,
        state: { active: false, hosts: 1, reason: "modules_unknown" },
      }),
      NOW,
    );
    expect(unknown.pill.label).toBe("Cannot verify");
    const starting = describeCrowdSec(status({ enabled: true }), NOW);
    expect(starting.pill.label).toBe("Starting");
  });
});

describe("bouncerNotice", () => {
  it("is silent for a bouncer that pulled recently", () => {
    expect(bouncerNotice(api(), NOW)).toBeNull();
    expect(
      bouncerNotice(api({ last_pull: secondsAgo(BOUNCER_STALE_SECONDS) }), NOW),
    ).toBeNull();
  });

  it("flags every way enforcement can be absent without anything looking wrong", () => {
    expect(bouncerNotice(api({ bouncer_registered: false }), NOW)?.tone).toBe(
      "error",
    );
    expect(bouncerNotice(api({ last_pull: undefined }), NOW)?.text).toMatch(
      /not fetched/,
    );
    expect(
      bouncerNotice(
        api({ last_pull: secondsAgo(BOUNCER_STALE_SECONDS + 1) }),
        NOW,
      )?.tone,
    ).toBe("error");
  });

  it("does not guess when the local API cannot be asked", () => {
    expect(
      bouncerNotice(api({ reachable: false, bouncer_registered: false }), NOW),
    ).toBeNull();
  });
});

describe("enable confirmation", () => {
  const points = crowdSecEnablePoints("203.0.113.50");

  it("names the address that becomes unbannable", () => {
    expect(points.join(" ")).toContain("203.0.113.50");
  });

  it("says the things a person should know before an action that blocks traffic", () => {
    const all = points.join("\n");
    expect(all).toMatch(/403/);
    expect(all).toMatch(/never behind it, so a ban can always be lifted/);
    expect(all).toMatch(/Nothing is banned on its own/);
    expect(all).toMatch(/Nothing leaves your server/);
    expect(all).toMatch(/CDN or load balancer/);
  });
});

describe("isBanTarget", () => {
  it.each([
    "203.0.113.9",
    "203.0.113.0/24",
    "2001:db8::1",
    "2001:db8::/48",
    " 203.0.113.9 ",
  ])("accepts %s", (v) => expect(isBanTarget(v)).toBe(true));
  it.each([
    "",
    "nope",
    "999.1.1.1",
    "1.2.3",
    "1.2.3.4/33",
    "1.2.3.4/",
    "2001:db8::/129",
    "1.2.3.4/8/8",
    "1.2.3.4 --x",
  ])("rejects %s", (v) => expect(isBanTarget(v)).toBe(false));
});

describe("timeLeft", () => {
  it("reads as a span", () => {
    const at = (s: number) => new Date(NOW.getTime() + s * 1000).toISOString();
    expect(timeLeft(at(30), NOW)).toBe("under a minute");
    expect(timeLeft(at(59 * 60), NOW)).toBe("59 minutes");
    expect(timeLeft(at(3600), NOW)).toBe("60 minutes");
    expect(timeLeft(at(23 * 3600), NOW)).toBe("23 hours");
    expect(timeLeft(at(7 * 86400), NOW)).toBe("7 days");
    expect(timeLeft(at(-5), NOW)).toBe("expiring");
  });
});

describe("sources and reasons", () => {
  it("labels where a ban came from", () => {
    expect(sourceLabel("dashboard")).toBe("Dashboard");
    expect(sourceLabel("waf")).toBe("WAF policy");
    expect(sourceLabel("cli")).toBe("cscli");
    expect(sourceLabel("crowdsec")).toBe("CrowdSec");
  });

  it("drops Belune's prefix, which the Source column already says", () => {
    expect(reasonText("belune: port scanner")).toBe("port scanner");
    expect(reasonText("belune-waf: attack-sqli: 14 requests in 10m")).toBe(
      "attack-sqli: 14 requests in 10m",
    );
    expect(reasonText("by hand")).toBe("by hand");
  });
});

describe("describeAutoban", () => {
  it("states the rule in words", () => {
    const s = describeAutoban(policy({ enabled: true }), run(), true);
    expect(s.rule).toBe(
      "Bans an address once 10 separate requests from it have tripped an attack rule within 10 minutes. The ban lasts 60 minutes.",
    );
    expect(s.warnings).toEqual([]);
  });

  it("warns about the three ways it can quietly not work", () => {
    const s = describeAutoban(
      policy({ enabled: true }),
      run({ capped: true, last_error: "database is locked" }),
      false,
    );
    const text = s.warnings.map((w) => w.text).join("\n");
    expect(text).toMatch(/last check failed: database is locked/);
    expect(text).toMatch(/limit of 25 automatic bans/);
    expect(text).toMatch(/WAF is off/);
  });

  it("does not nag about the WAF when the policy is off or the WAF state is unknown", () => {
    expect(
      describeAutoban(policy({ enabled: false }), run(), false).warnings,
    ).toEqual([]);
    expect(
      describeAutoban(policy({ enabled: true }), run(), null).warnings,
    ).toEqual([]);
  });

  it("says before it is turned on that a real visitor can be banned", () => {
    const text = autobanEnablePoints(
      policy({ threshold: 20, window_minutes: 5, duration_minutes: 180 }),
    ).join("\n");
    expect(text).toMatch(/real visitor can trip attack rules too/);
    expect(text).toMatch(/20 times in 5 minutes/);
    expect(text).toMatch(/3 hours/);
    expect(text).toMatch(
      /Protocol and request-format rules never lead to a ban/,
    );
    expect(text).toMatch(/Every automatic ban expires/);
  });
});
