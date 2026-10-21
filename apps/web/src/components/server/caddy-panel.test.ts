import { describe, expect, it } from "vitest";
import type { CaddyPanel } from "@/lib/types";
import {
  describeCaddy,
  MISSING_LOG_GRACE_MS,
  summarize,
  type CaddyRow,
} from "./caddy-panel";

const NOW = Date.parse("2026-10-20T12:00:00Z");

/** A healthy Belune-image install; each test overrides one corner of it. */
function healthy(over: Partial<CaddyPanel> = {}): CaddyPanel {
  return {
    discovery: { source: "label", container_name: "belune-caddy-1" },
    admin: { url: "http://caddy:2019", state: "healthy" },
    container: { state: "found", status: "running" },
    version: { value: "2.11.7-3", upstream: "2.11.7", source: "label" },
    image: "ghcr.io/belune-hq/belune-caddy:2",
    modules: { known: true, ids: ["http", "layer4"] },
    features: {
      rate_limit: { module: "http.handlers.rate_limit", state: "available" },
    },
    access_log: {
      path: "/var/log/caddy/access.log",
      resolved_path: "/var/log/caddy/access.log",
      file: "present",
      state: "ingesting",
      since: "2026-10-20T10:00:00Z",
      lines_read: 12,
      rows_inserted: 12,
    },
    ...over,
  };
}

function row(caddy: CaddyPanel, key: CaddyRow["key"]): CaddyRow {
  const r = describeCaddy(caddy, NOW).find((x) => x.key === key);
  if (!r) throw new Error(`no ${key} row`);
  return r;
}

describe("describeCaddy", () => {
  it("is all green for a healthy install", () => {
    const rows = describeCaddy(healthy(), NOW);
    expect(rows.map((r) => r.tone)).toEqual(Array(6).fill("ready"));
    expect(summarize(rows)).toEqual({ tone: "ready", label: "OK" });
  });

  // The rule the panel exists for. If any of these rendered as "none"/"absent"
  // an operator would be told their Caddy lacks something we never checked.
  describe("could not determine is not absent", () => {
    it("unknown modules never claim a count or a missing feature", () => {
      const r = row(
        healthy({
          modules: { known: false, ids: [] },
          features: {
            rate_limit: { module: "m", state: "unknown" },
          },
        }),
        "modules",
      );
      expect(r.tone).toBe("neutral");
      expect(r.value).toBe("Unknown");
      expect(r.value).not.toMatch(/0 loaded|none/i);
      expect(r.hint).not.toMatch(/Not available here/);
    });

    it("an unreadable version is Unknown, not blank or 'none'", () => {
      const r = row(
        healthy({ version: { value: null, upstream: null, source: null } }),
        "version",
      );
      expect(r.value).toBe("Unknown");
      expect(r.tone).toBe("neutral");
    });

    it("an unaskable Docker is not 'no container'", () => {
      const r = row(healthy({ container: { state: "unknown" } }), "container");
      expect(r.tone).toBe("neutral");
      expect(r.value).not.toMatch(/No container/);
    });

    it("an unprobed admin API is not 'unreachable'", () => {
      const r = row(
        healthy({ admin: { url: "http://c:2019", state: "unknown" } }),
        "admin",
      );
      expect(r.tone).toBe("neutral");
      expect(r.value).toBe("Not checked yet");
    });

    it("unknown rows keep the summary from saying OK", () => {
      const rows = describeCaddy(
        healthy({ modules: { known: false, ids: [] } }),
        NOW,
      );
      expect(summarize(rows).label).toBe("Partly unknown");
    });
  });

  describe("a module that is known to be missing", () => {
    it("is named, and is not an error (stock Caddy is supported)", () => {
      const r = row(
        healthy({
          features: {
            rate_limit: { module: "m", state: "unavailable" },
          },
        }),
        "modules",
      );
      expect(r.tone).toBe("ready");
      expect(r.hint).toMatch(/Not available here: rate limit/);
    });
  });

  describe("container", () => {
    it("flags a missing container that was named or found by label", () => {
      for (const source of ["env", "label"] as const) {
        const r = row(
          healthy({
            discovery: { source, container_name: "x" },
            container: { state: "not_found" },
          }),
          "container",
        );
        expect(r.tone).toBe("error");
      }
    });

    it("does not call an ASSUMED name that is missing an error — Caddy may run outside Docker", () => {
      const r = row(
        healthy({
          discovery: { source: "assumed", container_name: "infra-caddy-1" },
          container: { state: "not_found" },
        }),
        "container",
      );
      expect(r.tone).toBe("building");
      expect(r.hint).toMatch(/outside Docker/);
    });

    it("says so when the name was only assumed, even if the container exists", () => {
      const r = row(
        healthy({ discovery: { source: "assumed", container_name: "c" } }),
        "container",
      );
      expect(r.tone).toBe("building");
      expect(r.hint).toMatch(/assumed/);
    });

    it("flags a stopped container", () => {
      const r = row(
        healthy({ container: { state: "found", status: "exited" } }),
        "container",
      );
      expect(r.tone).toBe("error");
    });
  });

  describe("admin API", () => {
    it("shows the URL when unreachable, so a wrong one is visible", () => {
      const r = row(
        healthy({ admin: { url: "http://nope:2019", state: "unreachable" } }),
        "admin",
      );
      expect(r.tone).toBe("error");
      expect(r.hint).toContain("http://nope:2019");
    });
  });

  describe("request log keeps quiet and broken apart", () => {
    const log = (over: Partial<CaddyPanel["access_log"]>): CaddyPanel =>
      healthy({ access_log: { ...healthy().access_log, ...over } });

    it("no traffic yet is healthy", () => {
      const r = row(
        log({ state: "no_traffic", lines_read: 0, rows_inserted: 0 }),
        "access_log",
      );
      expect(r.tone).toBe("ready");
    });

    it("a missing file is an error once the grace has passed", () => {
      const since = new Date(NOW - MISSING_LOG_GRACE_MS - 1000).toISOString();
      const r = row(
        log({ state: "missing", file: "missing", since }),
        "access_log",
      );
      expect(r.tone).toBe("error");
      expect(r.hint).toMatch(/ACCESS_LOG_PATH/);
    });

    it("a missing file just after start is not yet a fault", () => {
      const since = new Date(NOW - 5000).toISOString();
      const r = row(
        log({ state: "missing", file: "missing", since }),
        "access_log",
      );
      expect(r.tone).toBe("neutral");
    });

    it("shows where it is actually looking (a dev path is relative)", () => {
      const r = row(
        log({
          path: "../../infra/caddy/logs/access.log",
          resolved_path: "/repo/infra/caddy/logs/access.log",
          state: "no_traffic",
        }),
        "access_log",
      );
      expect(r.hint).toContain("/repo/infra/caddy/logs/access.log");
    });

    it("surfaces the last error", () => {
      const r = row(
        log({ state: "error", last_error: "insert request log: boom" }),
        "access_log",
      );
      expect(r.tone).toBe("error");
      expect(r.hint).toBe("insert request log: boom");
    });

    it("an unwired status is Unknown", () => {
      const r = row(log({ state: "unknown", file: "" }), "access_log");
      expect(r.tone).toBe("neutral");
    });
  });
});
