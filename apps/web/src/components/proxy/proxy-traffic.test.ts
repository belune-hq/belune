import { describe, expect, it } from "vitest";
import type { ProxyTraffic } from "@/lib/types";
import {
  describeModuleTab,
  describeTraffic,
  formatSeconds,
} from "./proxy-traffic";

const base: ProxyTraffic = {
  state: "ok",
  in_flight: 2,
  proxied_total: 1000,
  window_seconds: 60,
  requests_per_second: 12.5,
  server_errors_per_second: 0,
  mean_seconds: 0.08,
  p50_seconds: 0.05,
  p95_seconds: 0.9,
  status_classes: { "2xx": 990, "5xx": 10 },
};

const tile = (t: ProxyTraffic, key: string) =>
  describeTraffic(t).tiles.find((x) => x.key === key)!;

describe("describeTraffic", () => {
  it("formats an ok reading", () => {
    expect(tile(base, "rate").value).toBe("12.5 /s");
    expect(tile(base, "mean").value).toBe("80 ms");
    expect(tile(base, "p95").value).toBe("900 ms");
  });

  it("an idle window has a rate of zero but NO latency — a dash, not 0 ms", () => {
    const idle = {
      ...base,
      requests_per_second: 0,
      mean_seconds: null,
      p50_seconds: null,
      p95_seconds: null,
    };
    expect(tile(idle, "rate").value).toBe("0 /s");
    expect(tile(idle, "mean").value).toBe("—");
    expect(tile(idle, "p95").value).not.toMatch(/0 ms/);
  });

  it("while collecting, rates are unknown rather than zero", () => {
    const collecting: ProxyTraffic = {
      ...base,
      state: "collecting",
      requests_per_second: null,
      server_errors_per_second: null,
      mean_seconds: null,
      p50_seconds: null,
      p95_seconds: null,
      window_seconds: null,
    };
    expect(tile(collecting, "rate").value).toBe("—");
    expect(tile(collecting, "errors").value).toBe("—");
    expect(tile(collecting, "errors").tone).toBe("default");
    // What IS known is still shown.
    expect(tile(collecting, "in_flight").value).toBe("2");
  });

  it("highlights 5xx only when there are some", () => {
    expect(tile(base, "errors").tone).toBe("default");
    expect(
      tile({ ...base, server_errors_per_second: 0.4 }, "errors").tone,
    ).toBe("error");
  });

  it("explains every no-number state and shows no tiles", () => {
    for (const state of [
      "unreachable",
      "disabled",
      "no_data",
      "unknown",
    ] as const) {
      const d = describeTraffic({ ...base, state });
      expect(d.tiles).toEqual([]);
      expect(d.message?.text).toBeTruthy();
    }
  });

  it("does not call a quiet install broken or a broken one quiet", () => {
    const noData = describeTraffic({ ...base, state: "no_data" });
    const disabled = describeTraffic({ ...base, state: "disabled" });
    expect(noData.message?.tone).toBe("neutral");
    expect(disabled.message?.tone).toBe("attention");
    expect(noData.message?.text).not.toMatch(/not recording/i);
  });
});

describe("formatSeconds", () => {
  it("switches units at one second", () => {
    expect(formatSeconds(0.0004)).toBe("<1 ms");
    expect(formatSeconds(0.25)).toBe("250 ms");
    expect(formatSeconds(1.234)).toBe("1.23 s");
    expect(formatSeconds(12.34)).toBe("12.3 s");
  });
});

describe("describeModuleTab", () => {
  it("renders the three states distinctly", () => {
    const unknown = describeModuleTab("crowdsec", "unknown");
    const unavailable = describeModuleTab("crowdsec", "unavailable");
    const available = describeModuleTab("crowdsec", "available");
    expect(
      new Set([unknown.title, unavailable.title, available.title]).size,
    ).toBe(3);
  });

  // The whole discipline: a Caddy that could not be asked is not a Caddy that
  // lacks the module.
  it("never says 'not active' or 'does not include' for unknown", () => {
    const d = describeModuleTab("waf", "unknown");
    expect(d.title).not.toMatch(/not active/i);
    expect(d.body).not.toMatch(/does not include/i);
    expect(d.tone).toBe("neutral");
  });

  it("names the module and the remedy when it is genuinely absent", () => {
    const d = describeModuleTab("waf", "unavailable");
    expect(d.title).toBe("Not active");
    expect(d.body).toMatch(/WAF/);
    expect(d.body).toMatch(/Belune Caddy image/);
  });
});
