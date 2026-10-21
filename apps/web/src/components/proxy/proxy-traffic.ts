import type { FeatureAvailability, ProxyTraffic } from "@/lib/types";

/**
 * What the Traffic card and the module tabs say, as data — kept out of the
 * components so the rule that matters is testable without a DOM: a number we
 * could not determine is "—", never 0, and "unknown" is never "unavailable".
 */
export type Tone = "default" | "ready" | "attention" | "error";

export interface TrafficTile {
  key: string;
  label: string;
  value: string;
  hint?: string;
  tone: Tone;
}

export interface TrafficDescription {
  /** Set when there are no numbers to show, and says why. */
  message?: { tone: "neutral" | "attention" | "error"; text: string };
  tiles: TrafficTile[];
}

const UNKNOWN = "—";

export function formatSeconds(s: number): string {
  if (s < 0.001) return "<1 ms";
  if (s < 1) return `${Math.round(s * 1000)} ms`;
  return `${s.toFixed(s < 10 ? 2 : 1)} s`;
}

function formatRate(perSecond: number): string {
  if (perSecond === 0) return "0 /s";
  return perSecond < 10
    ? `${perSecond.toFixed(2)} /s`
    : `${perSecond.toFixed(1)} /s`;
}

export function describeTraffic(t: ProxyTraffic): TrafficDescription {
  switch (t.state) {
    case "unreachable":
      return {
        tiles: [],
        message: {
          tone: "error",
          text: "Caddy's admin API did not answer, so there are no traffic numbers.",
        },
      };
    case "disabled":
      return {
        tiles: [],
        message: {
          tone: "attention",
          text: "Caddy is not recording HTTP metrics. Belune's own Caddyfile turns them on with `servers { metrics }`; a Caddy you run yourself needs the same.",
        },
      };
    case "no_data":
      return {
        tiles: [],
        message: {
          tone: "neutral",
          text: "Metrics are on, but Caddy has not proxied a request to an app since it started.",
        },
      };
    case "unknown":
      return {
        tiles: [],
        message: {
          tone: "neutral",
          text: "Traffic numbers are not available right now.",
        },
      };
  }

  // collecting | ok. Rates need two scrapes; until then they are unknown, and a
  // dash says so where a zero would claim an idle proxy.
  const measuring = t.state === "collecting";
  const rate = (v: number | null) => (v === null ? UNKNOWN : formatRate(v));
  const latency = (v: number | null) =>
    v === null ? UNKNOWN : formatSeconds(v);
  const noRequests = measuring ? "measuring…" : "no requests in the window";

  return {
    tiles: [
      {
        key: "rate",
        label: "Requests",
        value: rate(t.requests_per_second),
        hint: measuring ? "measuring…" : windowHint(t),
        tone: "default",
      },
      {
        key: "in_flight",
        label: "In flight",
        value: t.in_flight === null ? UNKNOWN : String(t.in_flight),
        hint: "being proxied now",
        tone: "default",
      },
      {
        key: "errors",
        label: "5xx",
        value: rate(t.server_errors_per_second),
        hint: measuring ? "measuring…" : "responses from the proxy handler",
        tone:
          t.server_errors_per_second !== null && t.server_errors_per_second > 0
            ? "error"
            : "default",
      },
      {
        key: "mean",
        label: "Mean time",
        value: latency(t.mean_seconds),
        hint: t.mean_seconds === null ? noRequests : "per request",
        tone: "default",
      },
      {
        key: "p50",
        label: "Median",
        value: latency(t.p50_seconds),
        hint: t.p50_seconds === null ? noRequests : "estimated from buckets",
        tone: "default",
      },
      {
        key: "p95",
        label: "95th percentile",
        value: latency(t.p95_seconds),
        hint: t.p95_seconds === null ? noRequests : "estimated from buckets",
        tone: "default",
      },
    ],
  };
}

function windowHint(t: ProxyTraffic): string {
  return t.window_seconds === null
    ? "to apps"
    : `to apps · last ${Math.round(t.window_seconds)}s`;
}

export type ModuleTabName = "crowdsec" | "waf";

export interface ModuleTabDescription {
  tone: "neutral" | "attention" | "ready";
  title: string;
  body: string;
}

const MODULE_LABEL: Record<ModuleTabName, string> = {
  crowdsec: "CrowdSec",
  waf: "WAF",
};

/**
 * The three probe states, kept distinct. `unknown` means Belune could not list
 * Caddy's modules (restarting, or no exec access): telling someone their Caddy
 * lacks the module then would send them to rebuild an image that was fine.
 */
export function describeModuleTab(
  name: ModuleTabName,
  state: FeatureAvailability,
): ModuleTabDescription {
  const label = MODULE_LABEL[name];
  switch (state) {
    case "unknown":
      return {
        tone: "neutral",
        title: "Cannot tell yet",
        body: `Belune could not list this Caddy's modules (it may be restarting, or Belune cannot reach it), so it does not know whether ${label} is available. Nothing is wrong with your image on this evidence.`,
      };
    case "unavailable":
      return {
        tone: "attention",
        title: "Not active",
        body: `This Caddy does not include the ${label} module. The Belune Caddy image does; stock Caddy is still fully supported without it.`,
      };
    case "available":
      return {
        tone: "ready",
        title: "Module available",
        body: `This Caddy includes the ${label} module. Controls for it are not part of this release yet.`,
      };
  }
}
