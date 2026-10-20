import { describe, expect, it } from "vitest";
import { featureStatus, inertExplanation } from "./route-feature-status";
import type { ProxyCapabilities } from "./types";

const rateLimit = (config: Record<string, unknown>, enabled = true) => ({
  feature_type: "rate_limit" as const,
  config,
  enabled,
});
const current = { window: "1s", max_events: 10 };

const caps = (
  state: "available" | "unavailable" | "unknown",
): ProxyCapabilities => ({
  known: state !== "unknown",
  features: {
    rate_limit: { module: "http.handlers.rate_limit", state },
  },
});

describe("featureStatus", () => {
  it("is active when the module is present and the config is current", () => {
    expect(featureStatus(rateLimit(current), caps("available"))).toEqual({
      active: true,
      inert: [],
    });
  });

  it("is inert, naming the module, when the module is known absent", () => {
    const s = featureStatus(rateLimit(current), caps("unavailable"));
    expect(s.active).toBe(false);
    expect(s.inert).toEqual([
      { kind: "module", module: "http.handlers.rate_limit" },
    ]);
  });

  // The important one: not being able to ask is not the same as "missing".
  it("makes no module claim when the probe is unknown or has not loaded", () => {
    expect(featureStatus(rateLimit(current), caps("unknown")).active).toBe(
      true,
    );
    expect(featureStatus(rateLimit(current), undefined).active).toBe(true);
  });

  it("flags the old rate/burst shape on its own, even on a Caddy that has the module", () => {
    const s = featureStatus(
      rateLimit({ rate: "10/s", burst: 5 }),
      caps("available"),
    );
    expect(s.active).toBe(false);
    expect(s.inert).toEqual([{ kind: "legacy-config" }]);
  });

  it("keeps the two reasons apart when both apply", () => {
    const s = featureStatus(rateLimit({ rate: "10/s" }), caps("unavailable"));
    expect(s.inert.map((r) => r.kind)).toEqual(["module", "legacy-config"]);
  });

  it("does not call a disabled feature inert — it is simply off", () => {
    expect(
      featureStatus(rateLimit(current, false), caps("unavailable")),
    ).toEqual({ active: false, inert: [] });
  });

  it("never touches features that need no module", () => {
    const s = featureStatus(
      { feature_type: "basic_auth", config: {}, enabled: true },
      caps("unavailable"),
    );
    expect(s.active).toBe(true);
  });

  it("says something different for each reason", () => {
    const a = inertExplanation({ kind: "module", module: "m" });
    const b = inertExplanation({ kind: "legacy-config" });
    expect(a).toContain("does not have");
    expect(b).toContain("older format");
    expect(a).not.toEqual(b);
  });

  describe("not-applied (what the reconciler observed)", () => {
    const since = "2026-10-20T07:12:04Z";
    const seen = (reason: "module_absent" | "modules_unknown") => [
      { feature_type: "rate_limit", reason, since },
    ];

    // The state this exists for: the probe is unknown for ever, so without it the
    // row would render a plain ON toggle for a feature nothing enforces.
    it("marks a feature inert when the probe is unknown but the reconciler stripped it", () => {
      const s = featureStatus(
        rateLimit(current),
        caps("unknown"),
        seen("modules_unknown"),
      );
      expect(s.active).toBe(false);
      expect(s.inert).toEqual([
        { kind: "not-applied", reason: "modules_unknown", since },
      ]);
    });

    it("is suppressed when the probe's own module reason is present", () => {
      const s = featureStatus(
        rateLimit(current),
        caps("unavailable"),
        seen("module_absent"),
      );
      expect(s.inert.map((r) => r.kind)).toEqual(["module"]);
    });

    it("adds nothing when the reconciler reported nothing", () => {
      expect(
        featureStatus(rateLimit(current), caps("unknown"), []).active,
      ).toBe(true);
      expect(
        featureStatus(rateLimit(current), undefined, undefined).active,
      ).toBe(true);
    });

    it("ignores an entry for a different feature type", () => {
      const s = featureStatus(
        { feature_type: "basic_auth", config: {}, enabled: true },
        caps("unknown"),
        seen("modules_unknown"),
      );
      expect(s.active).toBe(true);
    });

    it("keeps it alongside a legacy-config reason", () => {
      const s = featureStatus(
        rateLimit({ rate: "10/s" }),
        caps("unknown"),
        seen("modules_unknown"),
      );
      expect(s.inert.map((r) => r.kind)).toEqual([
        "not-applied",
        "legacy-config",
      ]);
    });

    it("gives different advice for each reason", () => {
      const absent = inertExplanation({
        kind: "not-applied",
        reason: "module_absent",
        since,
      });
      const unknown = inertExplanation({
        kind: "not-applied",
        reason: "modules_unknown",
        since,
      });
      expect(absent).toContain("Belune Caddy image");
      expect(unknown).toContain("CADDY_CONTAINER_NAME");
      expect(absent).not.toEqual(unknown);
    });
  });
});
