import { describeModuleTab } from "@/components/proxy/proxy-traffic";
import type {
  WafEvent,
  WafRecorderStats,
  WafRequestDetail,
  WafSeverity,
  WafStatus,
  WafWindow,
} from "@/lib/types";

/**
 * Tone to badge variant. Lives here rather than in a component so the table and
 * the detection panel cannot drift into showing the same severity two ways.
 */
export const BADGE_VARIANT = {
  error: "destructive",
  attention: "secondary",
  neutral: "outline",
  ready: "light",
} as const;

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
          // Amber, not grey. The operator asked for the WAF and is not getting
          // it: the switch is on and nothing on this box is being inspected.
          // Grey reads as a resting state, which is how a gap between what was
          // asked for and what is running goes unnoticed.
          // "Waiting for a request" would be the wrong promise: with no
          // hostname in the route, a request arriving now is not inspected at
          // all. This state is about having nothing to watch, not about the
          // absence of traffic.
          pill: { label: "Nothing to inspect", tone: "attention" },
          // Said in the present tense, and only about now. The first wording
          // ("It starts as soon as an application has a domain") sat directly
          // above a table of detections once the drill application was removed,
          // which reads as though nothing had ever been inspected.
          body:
            s.summary.total > 0
              ? "The WAF is on, but no application has a domain to inspect right now, so nothing is being watched. What it recorded earlier is still listed below."
              : "The WAF is on. It starts as soon as an application has a domain to inspect.",
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
  "The rule, severity, client address and host are stored. The URL, method and content type are shown from the request log beside them, for as long as that keeps them.",
  "The matched text is never stored, and neither are the request body or headers beyond its content type.",
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

export interface EventVerdict {
  label: string;
  tone: WafTone;
  /** Hover text; null when the badge needs no explaining. */
  title: string | null;
  /**
   * Whether this outranks the severities around it, and must not render at the
   * same weight. A verdict on the whole request sits in a column of per-rule
   * severities, and "critical" is a common one — so the error tone alone leaves
   * the verdict indistinguishable from the noise it is a verdict about.
   */
  strong: boolean;
}

/**
 * The badge on a detection row.
 *
 * Rule 949110 is the anomaly-score summary — the one rule that states a verdict
 * on the whole request — and Coraza reports it with NO severity, because the rule
 * declares none. Rendering that verbatim puts the only row that means anything in
 * the faintest badge on the page, while the benign `text/plain` content-type match
 * beside it reads "critical" in red. So the verdict is named instead.
 */
export function describeEventVerdict(
  e: Pick<WafEvent, "rule_id" | "severity">,
): EventVerdict {
  if (e.rule_id === ANOMALY_SUMMARY_RULE) {
    return {
      label: "Would block",
      tone: "error",
      title:
        "This request's anomaly score passed the threshold, so a blocking WAF would have refused it. Nothing was blocked.",
      strong: true,
    };
  }
  return {
    label: e.severity,
    tone: severityTone(e.severity),
    title: null,
    strong: false,
  };
}

/** The windows the summary offers, in the order they are shown. */
export const WAF_WINDOWS: readonly { value: WafWindow; label: string }[] = [
  { value: "1h", label: "1h" },
  { value: "24h", label: "24h" },
  { value: "7d", label: "7d" },
  { value: "all", label: "All kept" },
] as const;

/** The window as a phrase, for use inside a sentence. */
export function windowPhrase(w: WafWindow): string {
  switch (w) {
    case "1h":
      return "the last hour";
    case "24h":
      return "the last 24 hours";
    case "7d":
      return "the last 7 days";
    case "all":
      return "everything still kept";
  }
}

export function eventHost(e: Pick<WafEvent, "host">): string {
  return e.host ?? "unknown host";
}

/**
 * "attack-xss" → "xss". Lowercase and verbatim on purpose: these are the rule
 * set's own tag names, they are what the autoban policy's checkboxes are
 * labelled with (crowdsec-tab), and what a ban reason records in the audit log
 * ("belune-waf: attack-sqli: 14 requests in 10m"). Prettifying them here would
 * make the WAF tab name the same thing differently from the tab that acts on it.
 */
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

/**
 * Why a detection can have no request on record. Worth saying rather than
 * leaving a blank cell: the detection is not broken and nothing failed, the
 * evidence simply has its own retention.
 */
export const REQUEST_DETAIL_RETENTION =
  "Request logs have their own retention, so a detection outlives its request once that window is shorter — and a detection recorded before Belune kept request detail never had one.";

export const REQUEST_DETAIL_ABSENT = `No request on record. ${REQUEST_DETAIL_RETENTION}`;

export interface RequestSummary {
  /** The request as one line: "POST /checkout?step=2". */
  line: string;
  /** The rest of what is kept, for the hover. */
  title: string;
  /** Null when the access entry carried a status outside 100-599. */
  status: number | null;
}

/**
 * The request behind a detection, as one readable line.
 *
 * Content-Type leads the hover because it is what makes the common false
 * positive legible: rule 920420 ("Request content type is not allowed by
 * policy") fires on a benign text/plain POST, and the content type is the whole
 * of the difference between that and an attack.
 */
export function describeRequestDetail(r: WafRequestDetail): RequestSummary {
  const parts: string[] = [];
  if (r.content_type) parts.push(`Content-Type: ${r.content_type}`);
  if (r.user_agent) parts.push(`User agent: ${r.user_agent}`);
  parts.push("The matched payload is not stored.");
  return {
    line: `${r.method} ${r.path}`,
    title: parts.join("\n"),
    status: r.status >= 100 && r.status <= 599 ? r.status : null,
  };
}

/**
 * The flag on a client that tripped the anomaly-score rule at least once.
 *
 * It is the only column that separates an attack from rule noise: a client with
 * four hundred detections and no flag is usually one benign application posting
 * the wrong content type, while one flagged request is a request the CRS would
 * have refused outright.
 */
export const CLIENT_WOULD_BLOCK =
  "At least one request from this client passed the anomaly-score threshold, so a blocking WAF would have refused it. Nothing was blocked.";

export interface UrlCount {
  /** What the cell says: a number, or a dash when there is nothing to count. */
  text: string;
  /** Why, when the number is not the whole story. Null when it is. */
  title: string | null;
}

/**
 * The URLs cell.
 *
 * A detection stores no URI of its own — the URL is read from the request log
 * through the uuid both carry — so this count is a floor, and falls to zero for
 * detections older than the request logs behind them. A bare "0" beside "12
 * requests" reads as a bug in the page, so the gap is named instead.
 */
export function describeUrlCount(urls: number, requests: number): UrlCount {
  if (urls === 0) {
    return {
      text: "—",
      title:
        requests > 0
          ? `No URL on record for ${requests === 1 ? "this request" : "any of these requests"}. ${REQUEST_DETAIL_RETENTION}`
          : REQUEST_DETAIL_ABSENT,
    };
  }
  if (urls < requests) {
    // The retention clause alone, not REQUEST_DETAIL_ABSENT: that one opens
    // "No request on record", which flatly contradicts the "at least 3 of 7"
    // this sentence just said. Found by reading the hover in a browser.
    return {
      text: urls.toLocaleString(),
      title: `At least ${urls.toLocaleString()} of ${requests.toLocaleString()} requests still have their URL on record; the rest have been swept. ${REQUEST_DETAIL_RETENTION}`,
    };
  }
  return { text: urls.toLocaleString(), title: null };
}

/**
 * What the grouped table is showing, in words, including when it is not showing
 * everything. The list is capped rather than paged: under a distributed scan the
 * client count runs to thousands and the loudest are the actionable set, so the
 * way through is a narrower period or a filter, not a next page.
 */
export function describeClientList(
  shown: number,
  hasMore: boolean,
  window: WafWindow,
): string | null {
  if (!hasMore) return null;
  return `Showing the ${shown.toLocaleString()} clients with the most detections in ${windowPhrase(window)}. More matched — narrow the period, or filter by address.`;
}

export interface DetectionRequest {
  /** The method, or null when the request behind this detection is gone. */
  method: string | null;
  /** The status, or null when there is none worth showing. */
  status: number | null;
  /** The host. Always present: it comes from the detection, not the request log. */
  host: string;
  title: string;
  /** Whether the request behind the detection is on record at all. */
  kept: boolean;
}

/**
 * The request a detection was for, as the table shows it: method, status, host.
 *
 * No path. The path is the long field — a query string carrying an injection
 * attempt runs past any column width — so it lives in the detail panel, which
 * can show it whole, and the column keeps what can be read at a glance. The
 * distinct-path count is still on the client row above.
 *
 * The host comes from the detection and the method and status from the request
 * log, so the host survives a sweep they do not. Showing it alone is better than
 * an empty cell: it still names the application that was targeted.
 */
export function describeDetectionRequest(
  e: Pick<WafEvent, "host" | "request">,
): DetectionRequest {
  const host = eventHost(e);
  if (!e.request) {
    return {
      method: null,
      status: null,
      host,
      title: REQUEST_DETAIL_ABSENT,
      kept: false,
    };
  }
  const d = describeRequestDetail(e.request);
  return {
    method: e.request.method,
    status: d.status,
    host,
    title: d.title,
    kept: true,
  };
}

/** Status tone, so a 500 does not read as calmly as a 200. */
export function statusTone(status: number): WafTone {
  if (status >= 500) return "error";
  if (status >= 400) return "attention";
  return "neutral";
}

export interface DetectionHeader {
  name: string;
  value: string;
}

/**
 * The request headers kept for a detection, Content-Type first because it is
 * what makes the common false positive legible.
 *
 * Two, and that is the whole of it: headers are an ALLOWLIST (000073), so these
 * are stored by being named and nothing else is — not a custom X-Api-Token an
 * application invents, which a denylist would have carried into a table the
 * operator reads. An empty list is normal, not a failure: a GET carries no
 * content type, and a detection older than its request log has no detail at all.
 */
export function detectionHeaders(
  r: WafRequestDetail | null,
): DetectionHeader[] {
  if (!r) return [];
  const out: DetectionHeader[] = [];
  if (r.content_type) out.push({ name: "Content-Type", value: r.content_type });
  if (r.user_agent) out.push({ name: "User-Agent", value: r.user_agent });
  return out;
}

/**
 * Why the header list is two lines long. Said on the panel rather than left to
 * be inferred: an operator who reads "Headers" and sees two assumes the request
 * carried two, and then trusts the absence of a header as evidence.
 */
export const DETECTION_HEADERS_ALLOWLIST =
  "Belune stores two request headers by name. Keeping every header would mean keeping whatever an application invents, including its own credentials.";

/** What this panel deliberately cannot show, so its absence is not read as a gap. */
export const DETECTION_PAYLOAD_ABSENT =
  "The text that matched the rule is not stored, and neither is the request body. Invite and deploy-hook tokens are redacted out of the URL before it is written.";

export interface DetectionCategory {
  /** Badge text. Null when the rule declares no class at all, shown as a dash. */
  label: string | null;
  /** Hover, always present: an empty cell needs its reason more than a full one. */
  title: string;
}

/**
 * Said once on the column header rather than on all nineteen rows.
 *
 * The distinction is the whole value of the column: severity cannot carry it,
 * because 920420 reports `critical` exactly like a SQLi match does.
 */
export const CATEGORY_COLUMN_HELP =
  "The rule set's own classification of the rule, not Belune's. An injection or file-inclusion class means a payload matched an attack signature; the protocol class only means the request was not formed the way policy expects. CrowdSec's autoban acts on the injection classes alone.";

// Only where the class means something other than what its name suggests. A
// cell-level note for every category would be the column header repeated
// nineteen times, and a claim about which classes ban would be a copy of
// crowdsec.BanCategories that goes quietly stale when that list changes.
const CATEGORY_NOTES: Record<string, string> = {
  protocol:
    "Request-format policy, not an attack payload: the 920-series checks how a request is formed. 920420 fires on an ordinary text/plain POST, which is why a wall of these is usually one application, not an attacker.",
};

/** The Category cell: the rule's class, or why there is not one. */
export function describeCategory(
  e: Pick<WafEvent, "rule_id" | "category">,
): DetectionCategory {
  const label = categoryLabel(e.category);
  if (!label) {
    return {
      label: null,
      title:
        e.rule_id === ANOMALY_SUMMARY_RULE
          ? "The anomaly-score summary declares no attack class of its own. It is a verdict on the whole request rather than one signature, which is what the severity column says for it."
          : "This rule declares no attack class.",
    };
  }
  return {
    label,
    title:
      CATEGORY_NOTES[label] ??
      `The rule set's own classification: ${e.category}.`,
  };
}

/**
 * Why the panel names a field and not the text that matched it.
 *
 * Said on the panel because the absence is the surprising part: an operator who
 * sees where a rule matched will reasonably expect to see what it matched.
 */
export const MATCHED_TARGET_NOTE =
  "Where the rule matched, not what it matched. The text itself is never stored, and this name is read from the rule's own output, so treat it as a pointer rather than as proof.";

/** The label for a detection that reported no variable at all. */
export const MATCHED_TARGET_ABSENT =
  "This rule matched on the request as a whole rather than on one field. Detections recorded before Belune kept this also show no field.";
