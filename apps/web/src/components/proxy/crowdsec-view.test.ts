import { describe, expect, it } from "vitest";
import type {
  AuditLog,
  CrowdSecDecision,
  CrowdSecAutobanStatus,
  CrowdSecLocalApi,
  CrowdSecPolicy,
  CrowdSecStatus,
} from "@/lib/types";
import {
  BOUNCER_STALE_SECONDS,
  addedAt,
  autobanEnablePoints,
  bouncerNotice,
  crowdSecEnablePoints,
  describeAutoban,
  describeHistory,
  describeSkipped,
  summarizeCrowdSec,
  describeCrowdSec,
  describeYourAddress,
  isBanTarget,
  crowdSecShield,
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
  your_ip_protection: "allowlisted",
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

  it("flags having nothing to inspect, and holds off on what it cannot confirm", () => {
    const wait = describeCrowdSec(
      status({
        enabled: true,
        state: { active: false, hosts: 0, reason: "no_applications" },
      }),
      NOW,
    );
    // Switched on and protecting nothing is a gap, not a resting state. Same
    // amber as the WAF tab in the same state; its own verb, because this card
    // bans addresses rather than reading requests.
    expect(wait.pill.label).toBe("Nothing to protect");
    expect(wait.pill.tone).toBe("attention");
    expect(wait.pill.label).not.toMatch(/inspect/i);
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

// The copy keys on how the address stands, not on the switch. The old wording said
// "an address that cannot be banned" whenever the switch was on, which is false of a
// public address (it is safe only while its allowlist entry stands) and said an
// address "is allowlisted when you turn this on" of a private one (never added).
describe("describeYourAddress", () => {
  it("says a private address needs no allowlist entry, on or off", () => {
    for (const enabled of [true, false]) {
      const d = describeYourAddress("192.168.147.1", "unbannable", enabled);
      expect(d.text).toMatch(/private or loopback/);
      expect(d.text).toMatch(/needs no allowlist entry/);
      expect(d.text).not.toMatch(/when you turn this on/);
      expect(d.text).not.toMatch(/cannot be banned/);
    }
  });

  it("says a public allowlisted address is safe only while its entry stands", () => {
    const d = describeYourAddress("203.0.113.50", "allowlisted", true);
    expect(d.text).toMatch(/while that entry stands/);
    expect(d.text).not.toMatch(/an address that cannot be banned/);
  });

  it("warns, rather than reassures, about a public address that is on but not allowlisted", () => {
    const on = describeYourAddress("203.0.113.50", "exposed", true);
    expect(on.tone).toBe("attention");
    expect(on.text).toMatch(/can be banned/);
    expect(on.text).not.toMatch(/cannot be banned/);

    const off = describeYourAddress("203.0.113.50", "exposed", false);
    expect(off.text).toMatch(/added to the allowlist when you turn this on/);
  });

  it("makes no claim when the local API could not be asked", () => {
    const d = describeYourAddress("203.0.113.50", "unknown", true);
    expect(d.text).toBe("You are connecting from 203.0.113.50.");
  });
});

describe("enable confirmation", () => {
  const points = crowdSecEnablePoints("203.0.113.50");

  it("names the address that becomes unbannable", () => {
    expect(points.join(" ")).toContain("203.0.113.50");
  });

  it("does not promise a private address an allowlist entry", () => {
    const own = crowdSecEnablePoints("192.168.147.1", "unbannable").join(" ");
    expect(own).toMatch(/needs no allowlist entry/);
    expect(own).not.toMatch(/put on an allowlist now/);
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

describe("describeSkipped", () => {
  it("says nothing when nothing was left alone", () => {
    expect(describeSkipped(0)).toBeNull();
    expect(describeSkipped(-1)).toBeNull();
  });

  it("counts, in the right number", () => {
    expect(describeSkipped(1)).toMatch(
      /^1 address over the threshold was left alone/,
    );
    expect(describeSkipped(3)).toMatch(
      /^3 addresses over the threshold were left alone/,
    );
  });

  it("lists the possible reasons and claims none, because only a count is kept", () => {
    const s = describeSkipped(2)!;
    for (const reason of [
      /allowlist/,
      /lifted by an admin/,
      /cannot be banned/,
      /past the limit/,
    ]) {
      expect(s).toMatch(reason);
    }
    expect(s).toMatch(/does not record which/);
    // The cause is not asserted: "because", "protected" and "your admin" would each be a claim.
    expect(s).not.toMatch(/because|protected|your /i);
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

describe("crowdSecShield", () => {
  // Two icons were not enough and the gap showed: with the switch on and no
  // application running, turning CrowdSec on changed nothing on screen.
  it("separates off from on-but-covering-nothing", () => {
    expect(crowdSecShield(status({ enabled: false }))).toBe("off");
    expect(
      crowdSecShield(
        status({
          enabled: true,
          state: { active: false, hosts: 0, reason: "no_applications" },
        }),
      ),
    ).toBe("idle");
  });

  // ⛔ The tick is the one that may not be generous: out on the tab strip there
  // is no pill beside it to say that nothing is covered.
  it("keeps the tick for a hostname actually being covered", () => {
    expect(
      crowdSecShield(
        status({ enabled: true, state: { active: true, hosts: 2 } }),
      ),
    ).toBe("enforcing");
  });

  it("is off before the status has loaded", () => {
    expect(crowdSecShield(undefined)).toBe("off");
  });
});

describe("addedAt", () => {
  it("formats a timestamp CrowdSec gave", () => {
    expect(addedAt("2026-10-27T09:05:00Z")).toMatch(
      /^2026-10-2[67] \d\d:\d\d:\d\d$/,
    );
  });

  // The Go side discards the parse error, so a timestamp cscli did not give
  // arrives as the zero time, not as an absent field. Printed, it would read
  // "0001-01-01 00:00:00" — a date, and therefore a claim.
  it("says nothing rather than claiming the year one", () => {
    expect(addedAt("0001-01-01T00:00:00Z")).toBeNull();
    expect(addedAt("")).toBeNull();
    expect(addedAt(undefined)).toBeNull();
    expect(addedAt("not a date")).toBeNull();
  });
});

describe("summarizeCrowdSec", () => {
  const on = (over: Partial<CrowdSecStatus> = {}) =>
    status({ enabled: true, state: { active: true, hosts: 2 }, ...over });
  const tile = (stats: ReturnType<typeof summarizeCrowdSec>, key: string) =>
    stats.find((s) => s.key === key)!;
  const ban = (
    id: number,
    source: CrowdSecDecision["source"],
  ): CrowdSecDecision => ({
    id,
    value: `203.0.113.${id}`,
    scope: "ip",
    source,
    reason: "",
    created_at: secondsAgo(60),
    expires_at: secondsAgo(-3600),
  });

  it("says a working bouncer is connected, and when it last pulled", () => {
    const b = tile(summarizeCrowdSec(on(), [], NOW), "bouncer");
    expect(b.value).toBe("Connected");
    expect(b.tone).toBe("ready");
    expect(b.hint).toBe("Last pulled under a minute ago.");
  });

  it("keeps off, unreachable, stale and idle apart", () => {
    const v = (s: CrowdSecStatus) =>
      tile(summarizeCrowdSec(s, [], NOW), "bouncer").value;
    expect(v(status())).toBe("Off");
    expect(v(on({ local_api: api({ reachable: false }) }))).toBe("Unreachable");
    expect(v(on({ local_api: api({ last_pull: secondsAgo(600) }) }))).toBe(
      "Not pulling bans",
    );
    expect(v(on({ state: { active: false, hosts: 0 } }))).toBe("Idle");
  });

  it("counts the automatic bans among the bans in force", () => {
    const stats = summarizeCrowdSec(
      on(),
      [ban(1, "waf"), ban(2, "waf"), ban(3, "dashboard")],
      NOW,
    );
    expect(tile(stats, "bans").value).toBe("3");
    expect(tile(stats, "bans").hint).toBe(
      "2 automatic, 1 by an operator or cscli.",
    );
  });

  it("does not read an unreadable ban list as no bans", () => {
    const b = tile(summarizeCrowdSec(on(), undefined, NOW), "bans");
    expect(b.value).toBe("Unknown");
  });

  it("states the policy, and that the check is not running while it is off", () => {
    const off = summarizeCrowdSec(on(), [], NOW);
    expect(tile(off, "automatic").value).toBe("Off");
    expect(tile(off, "check").value).toBe("Not running");

    const p = on({
      autoban: {
        policy: policy({ enabled: true }),
        status: run({ last_run: secondsAgo(150) }),
        categories: [],
      },
    });
    const stats = summarizeCrowdSec(p, [], NOW);
    expect(tile(stats, "automatic").hint).toBe(
      "10 tripped requests in 10 minutes, banned for 60 minutes.",
    );
    expect(tile(stats, "check").value).toBe("3 minutes ago");
  });

  it("surfaces skipped, capped and a failed check instead of dropping them", () => {
    const base = { policy: policy({ enabled: true }), categories: [] };
    const skipped = summarizeCrowdSec(
      on({
        autoban: {
          ...base,
          status: run({ last_run: secondsAgo(5), skipped: 2 }),
        },
      }),
      [],
      NOW,
    );
    expect(tile(skipped, "check").hint).toBe("2 addresses left alone.");

    const capped = summarizeCrowdSec(
      on({
        autoban: {
          ...base,
          status: run({ last_run: secondsAgo(5), capped: true }),
        },
      }),
      [],
      NOW,
    );
    expect(tile(capped, "check").tone).toBe("attention");
    expect(tile(capped, "check").hint).toMatch(/limit of 25 bans/);

    const failed = summarizeCrowdSec(
      on({
        autoban: { ...base, status: run({ last_error: "database is locked" }) },
      }),
      [],
      NOW,
    );
    expect(tile(failed, "check").value).toBe("Failed");
    expect(tile(failed, "check").hint).toBe("database is locked");
  });
});

describe("describeHistory", () => {
  const log = (over: Partial<AuditLog>): AuditLog => ({
    id: "1",
    user_id: null,
    action: "crowdsec_ban",
    resource_type: "crowdsec",
    resource_id: "203.0.113.9",
    details: null,
    ip_address: null,
    created_at: secondsAgo(10),
    ...over,
  });

  it("attributes an automatic ban to Belune, not to a blank", () => {
    const e = describeHistory(
      log({
        action: "crowdsec_autoban",
        details: {
          requests: 14,
          categories: ["attack-sqli"],
          window_minutes: 10,
          duration: "1h0m0s",
        },
      }),
    );
    expect(e.actor).toEqual({ kind: "system", label: "Belune (automatic)" });
    expect(e.what).toBe("Banned automatically");
    expect(e.detail).toBe("14 requests tripped attack-sqli in 10 minutes");
  });

  it("does not call an operator whose account was deleted Belune", () => {
    const e = describeHistory(log({ action: "crowdsec_ban", user_id: null }));
    expect(e.actor).toEqual({ kind: "removed", label: "A removed user" });
  });

  it("names the operator, or the token when there is no session", () => {
    expect(
      describeHistory(log({ user_email: "a@example.com" })).actor.label,
    ).toBe("a@example.com");
    expect(describeHistory(log({ token_name: "ci" })).actor).toEqual({
      kind: "token",
      label: "ci (API token)",
    });
  });

  it("puts the duration and the operator's own words in the detail", () => {
    const e = describeHistory(
      log({
        user_email: "a@example.com",
        details: { duration: "24h", reason: "belune: scanning /admin" },
      }),
    );
    expect(e.what).toBe("Banned");
    expect(e.target).toBe("203.0.113.9");
    expect(e.detail).toBe("for 24h; scanning /admin");
  });

  it("falls back to the raw action rather than hiding an unknown one", () => {
    expect(
      describeHistory(
        log({ action: "crowdsec_future_thing", user_email: "a@b.c" }),
      ).what,
    ).toBe("crowdsec_future_thing");
  });
});
