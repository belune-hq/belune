import { describeModuleTab } from "@/components/proxy/proxy-traffic";
import type {
  CrowdSecAutobanStatus,
  CrowdSecLocalApi,
  CrowdSecPolicy,
  CrowdSecSource,
  CrowdSecStatus,
} from "@/lib/types";

export type CrowdSecTone = "neutral" | "attention" | "ready" | "error";

export interface CrowdSecDescription {
  pill: { label: string; tone: CrowdSecTone };
  body: string;
  /** A condition to flag beside the state, e.g. a refusal from Caddy. */
  notice: { tone: CrowdSecTone; text: string } | null;
  /** Whether the switch may be turned ON. Turning it off is always allowed. */
  canEnable: boolean;
}

/** Longer than this without the bouncer fetching the ban list and enforcement
 *  is stale. The bouncer asks every 5 seconds, so this is many missed asks. */
export const BOUNCER_STALE_SECONDS = 90;

/**
 * What the CrowdSec card says. The capability has THREE answers and they are
 * three different messages (see describeWaf); this adds a fourth condition that
 * is about the CrowdSec container and not about Caddy.
 */
export function describeCrowdSec(
  s: CrowdSecStatus,
  now: Date = new Date(),
): CrowdSecDescription {
  const cap = s.capability.state;

  if (cap === "unavailable") {
    const d = describeModuleTab("crowdsec", "unavailable");
    return {
      pill: { label: d.title, tone: "attention" },
      body: d.body,
      notice: null,
      canEnable: false,
    };
  }

  // Active is a fact about Caddy, so it wins over the module probe.
  if (s.state.active) {
    const stale = bouncerNotice(s.local_api, now);
    return {
      pill: stale
        ? { label: "Not enforcing", tone: "error" }
        : { label: "Enforcing", tone: "ready" },
      body: `Banned addresses are turned away with a 403 on ${plural(s.state.hosts, "application hostname")}. This dashboard is not covered, so a ban can always be lifted from here.`,
      notice:
        stale ??
        (s.state.reason === "lapi_unavailable"
          ? {
              tone: "attention",
              text: "The CrowdSec container is not answering. Caddy keeps enforcing the bans it already holds, but will not learn of new ones or lifted ones until it is back.",
            }
          : s.state.reason === "modules_unknown"
            ? {
                tone: "neutral",
                text: "Belune cannot list Caddy's modules right now, so it cannot confirm the module is still there. CrowdSec is left as it is.",
              }
            : null),
      canEnable: true,
    };
  }

  if (s.enabled) {
    if (s.state.error) {
      return {
        pill: { label: "Caddy refused it", tone: "error" },
        body: "CrowdSec is switched on, but Caddy did not accept the config. Belune keeps trying every 30 seconds.",
        notice: { tone: "error", text: s.state.error },
        canEnable: true,
      };
    }
    switch (s.state.reason) {
      case "lapi_unavailable":
        return {
          pill: { label: "Local API down", tone: "attention" },
          body: "CrowdSec is switched on, but the crowdsec container is not answering, so nothing is enforced yet. If you have just updated Belune, run the host update (docker compose up -d) so the crowdsec service exists.",
          notice: s.local_api.error
            ? { tone: "neutral", text: s.local_api.error }
            : null,
          canEnable: true,
        };
      case "modules_unknown":
        return {
          pill: { label: "Cannot verify", tone: "neutral" },
          body: describeModuleTab("crowdsec", "unknown").body,
          notice: null,
          canEnable: true,
        };
      case "module_absent":
        return {
          pill: { label: "Not active", tone: "attention" },
          body: describeModuleTab("crowdsec", "unavailable").body,
          notice: null,
          canEnable: false,
        };
      case "no_applications":
        return {
          pill: { label: "Waiting for an application", tone: "neutral" },
          body: "CrowdSec is on. It starts as soon as an application has a domain to protect.",
          notice: null,
          canEnable: true,
        };
      default:
        return {
          pill: { label: "Starting", tone: "attention" },
          body: "CrowdSec is switched on and is being applied to the proxy.",
          notice: null,
          canEnable: true,
        };
    }
  }

  if (cap === "unknown") {
    return {
      pill: { label: "Cannot tell yet", tone: "neutral" },
      body: describeModuleTab("crowdsec", "unknown").body,
      notice: null,
      // Allowed: the switch is saved and takes effect once Belune can ask.
      canEnable: true,
    };
  }

  return {
    pill: { label: "Off", tone: "neutral" },
    body: "Turn it on to be able to ban addresses from your applications. Nothing is banned until you, or a policy you choose, bans it.",
    notice: null,
    canEnable: true,
  };
}

/**
 * Whether the bouncer is actually fetching the ban list. Caddy fails open: a
 * bouncer that cannot authenticate admits everyone and says nothing, so
 * "configured" is not "enforcing" and the page has to tell them apart.
 */
export function bouncerNotice(
  api: CrowdSecLocalApi,
  now: Date,
): { tone: CrowdSecTone; text: string } | null {
  if (!api.reachable) return null;
  if (!api.bouncer_registered) {
    return {
      tone: "error",
      text: "Caddy's bouncer is not registered with CrowdSec, so no ban is enforced. Belune registers it again on its next pass.",
    };
  }
  if (!api.last_pull) {
    return {
      tone: "attention",
      text: "Caddy's bouncer has not fetched the ban list yet.",
    };
  }
  const age = (now.getTime() - new Date(api.last_pull).getTime()) / 1000;
  if (age > BOUNCER_STALE_SECONDS) {
    return {
      tone: "error",
      text: `Caddy's bouncer last fetched the ban list ${ago(age)} ago, so new bans are not being enforced. If it keeps happening, rotate its key.`,
    };
  }
  return null;
}

/** What turning it on means, said before it is turned on. */
export function crowdSecEnablePoints(yourIp: string): string[] {
  return [
    "Banned addresses get a 403 from your applications. This dashboard is never behind it, so a ban can always be lifted from here.",
    `The address you are connected from (${yourIp || "unknown"}) is put on an allowlist now, and so is every address an admin logs in from. An allowlisted address cannot be banned, and logging in lifts a ban already on it.`,
    "Nothing is banned on its own. Automatic bans from WAF detections are a separate switch below, off until you turn it on.",
    "Nothing leaves your server: no community blocklist, no signals sent to CrowdSec.",
    "Behind a CDN or load balancer, Caddy sees that service's address and not your visitors'. Banning it would block everyone using it, so do not use this there.",
  ];
}

const TARGET_V4 = /^(\d{1,3})\.(\d{1,3})\.(\d{1,3})\.(\d{1,3})(\/(\d{1,2}))?$/;

/** A loose check for the ban and allowlist boxes; the server validates properly. */
export function isBanTarget(v: string): boolean {
  const s = v.trim();
  const v4 = s.match(TARGET_V4);
  if (v4) {
    return (
      v4.slice(1, 5).every((o) => Number(o) <= 255) &&
      (v4[6] === undefined || Number(v4[6]) <= 32)
    );
  }
  const [addr, bits, ...rest] = s.split("/");
  if (rest.length > 0) return false;
  if (bits !== undefined && !(/^\d{1,3}$/.test(bits) && Number(bits) <= 128)) {
    return false;
  }
  return /^[0-9a-fA-F:.]+$/.test(addr) && (addr.match(/:/g)?.length ?? 0) >= 2;
}

export const BAN_DURATIONS = [
  { value: "1h", label: "1 hour" },
  { value: "6h", label: "6 hours" },
  { value: "24h", label: "24 hours" },
  { value: "7d", label: "7 days" },
  { value: "30d", label: "30 days" },
] as const;

export const ALLOW_DURATIONS = [
  { value: "24h", label: "24 hours" },
  { value: "7d", label: "7 days" },
  { value: "30d", label: "30 days" },
  { value: "90d", label: "90 days" },
  { value: "365d", label: "1 year" },
] as const;

export function sourceLabel(source: CrowdSecSource): string {
  switch (source) {
    case "dashboard":
      return "Dashboard";
    case "waf":
      return "WAF policy";
    case "cli":
      return "cscli";
    default:
      return "CrowdSec";
  }
}

/** The part of a reason that is the operator's own words: Belune's prefix says
 *  where a ban came from and the Source column already does. */
export function reasonText(reason: string): string {
  return reason.replace(/^belune(-waf)?: /, "");
}

/** "59 minutes", "23 hours", "6 days": time until an expiry. */
export function timeLeft(expiresAt: string, now: Date = new Date()): string {
  const s = (new Date(expiresAt).getTime() - now.getTime()) / 1000;
  if (s <= 0) return "expiring";
  return span(s);
}

function ago(seconds: number): string {
  return span(seconds);
}

function span(s: number): string {
  if (s < 90) return "under a minute";
  const m = Math.round(s / 60);
  if (m < 90) return plural(m, "minute");
  const h = Math.round(s / 3600);
  if (h < 48) return plural(h, "hour");
  return plural(Math.round(s / 86400), "day");
}

function plural(n: number, noun: string): string {
  return `${n} ${noun}${n === 1 ? "" : "s"}`;
}

export interface AutobanSummary {
  /** What the policy does, in a sentence. */
  rule: string;
  warnings: { tone: CrowdSecTone; text: string }[];
}

/** The policy in words, and the two ways it can go wrong without anyone looking. */
export function describeAutoban(
  policy: CrowdSecPolicy,
  status: CrowdSecAutobanStatus,
  wafEnabled: boolean | null,
): AutobanSummary {
  const warnings: AutobanSummary["warnings"] = [];
  if (status.last_error) {
    warnings.push({
      tone: "error",
      text: `The last check failed: ${status.last_error}`,
    });
  }
  if (status.capped) {
    warnings.push({
      tone: "attention",
      text: `The limit of ${policy.max_active} automatic bans in force is reached, so some addresses over the threshold were left alone. Something is tripping rules at a rate worth looking at.`,
    });
  }
  if (policy.enabled && wafEnabled === false) {
    warnings.push({
      tone: "attention",
      text: "The WAF is off, so there are no detections to act on. Turn it on in the WAF tab.",
    });
  }
  return {
    rule: `Bans an address once ${policy.threshold} separate requests from it have tripped an attack rule within ${policy.window_minutes} minutes. The ban lasts ${spanMinutes(policy.duration_minutes)}.`,
    warnings,
  };
}

function spanMinutes(min: number): string {
  if (min < 120) return plural(min, "minute");
  if (min < 48 * 60) return plural(Math.round(min / 60), "hour");
  return plural(Math.round(min / 1440), "day");
}

/** What a policy change means, said before an automatic ban is first turned on. */
export function autobanEnablePoints(p: CrowdSecPolicy): string[] {
  return [
    `A real visitor can trip attack rules too: a form that contains SQL or script text is enough. Anyone who does it ${p.threshold} times in ${p.window_minutes} minutes will be banned from your applications for ${spanMinutes(p.duration_minutes)}.`,
    `At most ${p.max_active} automatic bans are in force at once; past that, more are not added and this page says so.`,
    "Only attack categories count. Protocol and request-format rules never lead to a ban.",
    "Addresses on the allowlist, private addresses, and addresses you lift a ban from are left alone. Every automatic ban expires, and each is in the audit log.",
  ];
}
