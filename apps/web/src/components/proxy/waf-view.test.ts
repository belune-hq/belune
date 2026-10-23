import { describe, expect, it } from "vitest";
import type { WafStatus } from "@/lib/types";
import {
  categoryLabel,
  describeRecorder,
  describeWaf,
  eventHost,
  isIpAddress,
  severityTone,
  WAF_ENABLE_POINTS,
} from "./waf-view";

const status = (over: Partial<WafStatus> = {}): WafStatus => ({
  capability: { module: "http.handlers.waf", state: "available" },
  enabled: false,
  mode: "detection_only",
  state: { active: false, hosts: 0 },
  recorder: {
    since: "2026-10-23T00:00:00Z",
    recorded: 0,
    dropped: 0,
    malformed: 0,
  },
  retention_days: 7,
  max_events: 200000,
  summary: {
    window_hours: 24,
    events: 0,
    requests: 0,
    clients: 0,
    top_rules: [],
    top_clients: [],
  },
  ...over,
});

describe("describeWaf", () => {
  // The test that matters: the three capability answers are three different
  // messages. If `unknown` rendered as `unavailable`, someone whose Caddy was merely
  // restarting would be told to rebuild an image that was fine.
  it("keeps unavailable and unknown apart", () => {
    const unavailable = describeWaf(
      status({ capability: { module: "m", state: "unavailable" } }),
    );
    const unknown = describeWaf(
      status({ capability: { module: "m", state: "unknown" } }),
    );

    expect(unavailable.pill.label).toBe("Not active");
    expect(unavailable.body).toMatch(/does not include the WAF module/);
    expect(unavailable.canEnable).toBe(false);

    expect(unknown.pill.label).toBe("Cannot tell yet");
    expect(unknown.body).toMatch(/could not list/i);
    expect(unknown.body).not.toMatch(/does not include/);
    expect(unknown.canEnable).toBe(true);
  });

  it("does not refuse the switch when it merely could not ask", () => {
    const d = describeWaf(
      status({ capability: { module: "m", state: "unknown" } }),
    );
    expect(d.canEnable).toBe(true);
  });

  it("an available module that is off says so, and says what it does", () => {
    const d = describeWaf(status());
    expect(d.pill).toEqual({ label: "Off", tone: "neutral" });
    expect(d.body).toMatch(/only records/);
    expect(d.canEnable).toBe(true);
  });

  it("reports it running, and that it is detection only", () => {
    const d = describeWaf(
      status({ enabled: true, state: { active: true, hosts: 3 } }),
    );
    expect(d.pill).toEqual({ label: "Detecting", tone: "ready" });
    expect(d.body).toMatch(/3 application hostnames/);
    expect(d.body).toMatch(/nothing is blocked/i);
  });

  it("singular hostname", () => {
    const d = describeWaf(
      status({ enabled: true, state: { active: true, hosts: 1 } }),
    );
    expect(d.body).toMatch(/1 application hostname\b(?!s)/);
  });

  it("a running WAF stays reported as running when the probe cannot answer", () => {
    const d = describeWaf(
      status({
        enabled: true,
        capability: { module: "m", state: "unknown" },
        state: { active: true, hosts: 2, reason: "modules_unknown" },
      }),
    );
    expect(d.pill.label).toBe("Detecting");
    expect(d.notice?.text).toMatch(/cannot list Caddy's modules/);
  });

  it("switched on but waiting for an application", () => {
    const d = describeWaf(
      status({
        enabled: true,
        state: { active: false, hosts: 0, reason: "no_applications" },
      }),
    );
    expect(d.pill.label).toBe("Waiting for an application");
  });

  it("surfaces a refusal from Caddy rather than hiding it", () => {
    const d = describeWaf(
      status({
        enabled: true,
        state: { active: false, hosts: 2, error: "HTTP 500: unknown module" },
      }),
    );
    expect(d.pill).toEqual({ label: "Caddy refused it", tone: "error" });
    expect(d.notice).toEqual({
      tone: "error",
      text: "HTTP 500: unknown module",
    });
  });

  it("switched on, module known to be absent, is not the same as could-not-ask", () => {
    const absent = describeWaf(
      status({
        enabled: true,
        state: { active: false, hosts: 2, reason: "module_absent" },
      }),
    );
    const unknown = describeWaf(
      status({
        enabled: true,
        state: { active: false, hosts: 2, reason: "modules_unknown" },
      }),
    );
    expect(absent.pill.label).toBe("Not active");
    expect(unknown.pill.label).toBe("Cannot verify");
    expect(absent.body).not.toBe(unknown.body);
  });
});

describe("describeRecorder", () => {
  const base = { since: "x", recorded: 5, dropped: 0, malformed: 0 };

  it("says nothing when nothing was lost", () => {
    expect(describeRecorder(base)).toEqual([]);
  });

  it("names every way a detection is lost without a row", () => {
    const w = describeRecorder({
      ...base,
      dropped: 1200,
      malformed: 3,
      last_error: "connection refused",
    });
    expect(w.map((x) => x.tone)).toEqual(["error", "attention", "attention"]);
    expect(w[0].text).toContain("connection refused");
    expect(w[1].text).toMatch(/1,200 events were dropped/);
    expect(w[1].text).toMatch(/floor/);
    expect(w[2].text).toMatch(/3 log lines/);
  });
});

describe("events", () => {
  it("rates severity", () => {
    expect(severityTone("critical")).toBe("error");
    expect(severityTone("emergency")).toBe("error");
    expect(severityTone("warning")).toBe("attention");
    expect(severityTone("notice")).toBe("neutral");
    // The anomaly summary declares no severity; it must not look alarming OR vanish.
    expect(severityTone("unknown")).toBe("neutral");
  });

  it("an event with no host is shown as unknown, never blank", () => {
    expect(eventHost({ host: null })).toBe("unknown host");
    expect(eventHost({ host: "shop.example.com" })).toBe("shop.example.com");
  });

  it("trims the attack- prefix from a category", () => {
    expect(categoryLabel("attack-xss")).toBe("xss");
    expect(categoryLabel("attack-sqli")).toBe("sqli");
    expect(categoryLabel(null)).toBeNull();
  });
});

describe("the points shown before it is turned on", () => {
  // These are the caveats the PR is honest about; the UI must say them too.
  it("covers what an operator would be surprised to learn later", () => {
    const text = WAF_ENABLE_POINTS.join(" ");
    expect(text).toMatch(/Nothing is blocked/);
    expect(text).toMatch(/not to this dashboard/);
    expect(text).toMatch(/128 KiB/);
    expect(text).toMatch(/not stored|are not\./);
    expect(text).toMatch(/false positives share/);
  });
});

describe("isIpAddress", () => {
  it("accepts addresses and rejects what the server would", () => {
    for (const ok of ["203.0.113.9", "2001:db8::1", "::1", " 10.0.0.1 "]) {
      expect(isIpAddress(ok), ok).toBe(true);
    }
    for (const bad of [
      "",
      "300.1.1.1",
      "1.2.3",
      "example.com",
      "12345",
      "a:b",
    ]) {
      expect(isIpAddress(bad), bad).toBe(false);
    }
  });
});
