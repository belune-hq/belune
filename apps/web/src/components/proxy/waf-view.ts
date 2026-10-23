import { describeModuleTab } from "@/components/proxy/proxy-traffic";
import type {
  WafEvent,
  WafRecorderStats,
  WafSeverity,
  WafStatus,
} from "@/lib/types";

export type WafTone = "neutral" | "attention" | "ready" | "error";

export interface WafDescription {
  pill: { label: string; tone: WafTone };
  /** Why the page is in this state, in words. Null when there is nothing to add. */
  body: string;
  /** A condition to flag beside the state, e.g. a refusal from Caddy. */
  notice: { tone: WafTone; text: string } | null;
  /** Whether the switch may be turned ON. Turning it off is always allowed. */
  canEnable: boolean;
}

/**
 * What the WAF card says. The capability has THREE answers and they are three
 * different messages: `unknown` means Belune could not list Caddy's modules
 * (restarting, or no exec access), and telling someone their image lacks the
 * module then sends them to rebuild an image that was fine.
 */
export function describeWaf(s: WafStatus): WafDescription {
  const cap = s.capability.state;

  if (cap === "unavailable") {
    const d = describeModuleTab("waf", "unavailable");
    return {
      pill: { label: d.title, tone: "attention" },
      body: d.body,
      notice: null,
      canEnable: false,
    };
  }

  // Active is a fact about Caddy, so it wins over the module probe: if the route
  // is in, the WAF is running whatever the probe says right now.
  if (s.state.active) {
    return {
      pill: { label: "Detecting", tone: "ready" },
      body: `Watching ${plural(s.state.hosts, "application hostname")}. Nothing is blocked.`,
      notice:
        s.state.reason === "modules_unknown"
          ? {
              tone: "neutral",
              text: "Belune cannot list Caddy's modules right now, so it cannot confirm the module is still there. The WAF is left as it is.",
            }
          : null,
      canEnable: true,
    };
  }

  if (s.enabled) {
    if (s.state.error) {
      return {
        pill: { label: "Caddy refused it", tone: "error" },
        body: "The WAF is switched on, but Caddy did not accept the route. Belune keeps trying every 30 seconds.",
        notice: { tone: "error", text: s.state.error },
        canEnable: true,
      };
    }
    switch (s.state.reason) {
      case "modules_unknown":
        return {
          pill: { label: "Cannot verify", tone: "neutral" },
          ...unknownCopy(),
          notice: null,
          canEnable: true,
        };
      case "module_absent":
        return {
          pill: { label: "Not active", tone: "attention" },
          body: describeModuleTab("waf", "unavailable").body,
          notice: null,
          canEnable: false,
        };
      case "no_applications":
        return {
          pill: { label: "Waiting for an application", tone: "neutral" },
          body: "The WAF is on. It starts as soon as an application has a domain to inspect.",
          notice: null,
          canEnable: true,
        };
      default:
        // Switched on, nothing reported yet: the next reconcile pass settles it.
        return {
          pill: { label: "Starting", tone: "attention" },
          body: "The WAF is switched on and is being applied to the proxy.",
          notice: null,
          canEnable: true,
        };
    }
  }

  if (cap === "unknown") {
    return {
      pill: { label: "Cannot tell yet", tone: "neutral" },
      ...unknownCopy(),
      notice: null,
      // Allowed: the switch is saved and takes effect once Belune can ask. Refusing
      // here would claim a module is missing that may well be there.
      canEnable: true,
    };
  }

  return {
    pill: { label: "Off", tone: "neutral" },
    body: "Turn it on to see which requests to your applications the OWASP Core Rule Set would block. It only records them.",
    notice: null,
    canEnable: true,
  };
}

function unknownCopy() {
  return { body: describeModuleTab("waf", "unknown").body };
}

function plural(n: number, noun: string): string {
  return `${n} ${noun}${n === 1 ? "" : "s"}`;
}

/** What turning it on means, said before it is turned on. */
export const WAF_ENABLE_POINTS = [
  "Nothing is blocked. The rules only record what they would have blocked.",
  "It inspects requests to your applications, not to this dashboard.",
  "Request bodies over 128 KiB, or of unknown length, are not inspected, so uploads are never touched. A payload inside one goes unseen.",
  "Only the rule, severity, client address and host are stored. The request itself, its URL and the matched text are not.",
  "One application's false positives share this log with every other application's.",
] as const;

export interface RecorderWarning {
  tone: WafTone;
  text: string;
}

/** The ways a detection can be lost without leaving a row, said out loud. */
export function describeRecorder(r: WafRecorderStats): RecorderWarning[] {
  const out: RecorderWarning[] = [];
  if (r.last_error) {
    out.push({
      tone: "error",
      text: `Could not save events: ${r.last_error}`,
    });
  }
  if (r.dropped > 0) {
    out.push({
      tone: "attention",
      text: `${r.dropped.toLocaleString()} events were dropped under load since Belune started, so the count below is a floor.`,
    });
  }
  if (r.malformed > 0) {
    out.push({
      tone: "attention",
      text: `${r.malformed.toLocaleString()} log lines claimed to be rule matches but did not have the expected format, and were not recorded.`,
    });
  }
  return out;
}

export function severityTone(sev: WafSeverity): WafTone {
  switch (sev) {
    case "emergency":
    case "alert":
    case "critical":
      return "error";
    case "error":
    case "warning":
      return "attention";
    default:
      return "neutral";
  }
}

/** The CRS rule that says a request would have been blocked. */
export const ANOMALY_SUMMARY_RULE = 949110;

export function eventHost(e: Pick<WafEvent, "host">): string {
  return e.host ?? "unknown host";
}

/** "attack-xss" → "XSS"-style label; anything else is shown as it is. */
export function categoryLabel(category: string | null): string | null {
  if (!category) return null;
  return category.replace(/^attack-/, "");
}

/** A loose check for the filter box; the server validates properly. */
export function isIpAddress(v: string): boolean {
  const s = v.trim();
  const v4 = s.match(/^(\d{1,3})\.(\d{1,3})\.(\d{1,3})\.(\d{1,3})$/);
  if (v4) return v4.slice(1).every((o) => Number(o) <= 255);
  return /^[0-9a-fA-F:.]+$/.test(s) && (s.match(/:/g)?.length ?? 0) >= 2;
}
