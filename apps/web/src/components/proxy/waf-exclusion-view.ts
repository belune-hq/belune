import type {
  WafEvent,
  WafExclusion,
  WafExclusionInput,
  WafExclusionShape,
} from "@/lib/types";

// These mirror proxy.ValidateWAFExclusion. The server is authoritative and answers
// in its own words; they exist so the dialog can say, before the trip, what is
// possible for THIS detection and why not when it is not. They are not a second
// opinion to be kept in step by hand: when they disagree, the server wins.
export const WAF_EXCLUSION_MIN_RULE = 910000;
export const WAF_EXCLUSION_MAX_RULE = 944999;
const TARGET_NAME_RE = /^[A-Za-z0-9_.-]{1,80}$/;
const PATH_RE = /^\/[A-Za-z0-9._~/-]{0,199}\*?$/;

/** Why a rule can never be excluded, or null when it can. */
export function ruleRefusal(ruleId: number): string | null {
  const verdict =
    ruleId === 949110 ||
    ruleId === 959100 ||
    ruleId === 980130 ||
    (ruleId >= 949000 && ruleId <= 949999) ||
    (ruleId >= 959000 && ruleId <= 959999) ||
    (ruleId >= 980000 && ruleId <= 980999);
  if (verdict) {
    return "This rule adds up the scores of the others and decides what would be blocked. It is the verdict, not a signature: excluding it would remove the verdict instead of quieting a false positive. Exclude the rule that scored.";
  }
  if (ruleId < WAF_EXCLUSION_MIN_RULE || ruleId > WAF_EXCLUSION_MAX_RULE) {
    return "This is not a request-side detection rule, so it cannot be excluded.";
  }
  return null;
}

export type TargetOffer =
  | { available: true; collection: string; name: string }
  | { available: false; reason: string };

/**
 * The field a detection matched in, as an exclusion could name it.
 *
 * Offered only when the name is one an exclusion can carry. A name outside the
 * allowed characters could end a quoted string in the rule it is written into, so
 * the server refuses it; refusing it here turns an error message into a decision
 * the operator can act on, in the one place they have the detection in front of
 * them.
 *
 * ⚠️ The matched target is a hint, not evidence: it is read from text a request can
 * influence. It only ever PREFILLS a form the operator reviews.
 */
export function targetOffer(
  matched: string | null,
  collections: readonly string[],
): TargetOffer {
  if (!matched) {
    return {
      available: false,
      reason: "Belune did not record which field this rule matched in.",
    };
  }
  const at = matched.indexOf(":");
  if (at < 0) {
    return {
      available: false,
      reason: `This rule matched the request as a whole (${matched}), which has no name to exclude. Exclude it for a path, or turn the rule off.`,
    };
  }
  const collection = matched.slice(0, at);
  const name = matched.slice(at + 1);
  if (!collections.includes(collection)) {
    return {
      available: false,
      reason: `${collection} is not a field an exclusion can name.`,
    };
  }
  if (!TARGET_NAME_RE.test(name)) {
    return {
      available: false,
      reason:
        "The field's name has characters that cannot be written safely into a rule (only letters, digits, '.', '_' and '-' are allowed). Exclude it for a path, or turn the rule off.",
    };
  }
  return { available: true, collection, name };
}

/** The path a request asked for, without its query, when it is one an exclusion can carry. */
export function pathOffer(event: WafEvent): string | null {
  const raw = event.request?.path;
  if (!raw) return null;
  const path = raw.split(/[?#]/)[0];
  return PATH_RE.test(path) ? path : null;
}

export interface ExclusionDraft {
  shape: WafExclusionShape;
  collection: string;
  targetName: string;
  pathPattern: string;
  scope: "install" | "application";
  note: string;
}

/** What the dialog starts from for this detection, or the reason there is nothing to start. */
export function draftFor(
  event: WafEvent,
  collections: readonly string[],
): { refusal: string } | { draft: ExclusionDraft; target: TargetOffer } {
  const refusal = ruleRefusal(event.rule_id);
  if (refusal) return { refusal };

  const target = targetOffer(event.matched_target, collections);
  const path = pathOffer(event);
  const shape: WafExclusionShape = target.available
    ? "target"
    : path
      ? "path"
      : "rule";
  return {
    target,
    draft: {
      shape,
      collection: target.available ? target.collection : "",
      targetName: target.available ? target.name : "",
      pathPattern: path ?? "",
      // Narrow by default: one application, when the detection names one.
      scope: event.application_id ? "application" : "install",
      note: "",
    },
  };
}

/** The first thing wrong with a draft, or undefined. */
export function validateDraft(
  ruleId: number,
  d: ExclusionDraft,
): string | undefined {
  const refusal = ruleRefusal(ruleId);
  if (refusal) return refusal;
  if (d.shape === "target") {
    if (!d.collection || !TARGET_NAME_RE.test(d.targetName)) {
      return "Pick the field to ignore: a name of letters, digits, '.', '_' and '-'.";
    }
  } else if (d.shape === "path") {
    if (!PATH_RE.test(d.pathPattern)) {
      return "A path starts with '/', uses letters, digits and . _ ~ / -, and may end with one '*' to mean 'begins with'.";
    }
    if (d.pathPattern === "/*") {
      return "'/*' is every path. Turn the rule off instead, and say why.";
    }
  } else if (d.note.trim() === "") {
    return "Say why this rule is being turned off. The reason should outlive whoever typed it.";
  }
  if (d.note.length > 500) return "The note can be at most 500 characters.";
  return undefined;
}

/** The request body for a draft. */
export function inputFor(
  event: WafEvent,
  d: ExclusionDraft,
): WafExclusionInput {
  const input: WafExclusionInput = { rule_id: event.rule_id };
  if (d.shape === "target") {
    input.collection = d.collection;
    input.target_name = d.targetName;
  } else if (d.shape === "path") {
    input.path_pattern = d.pathPattern;
  }
  if (d.scope === "application" && event.application_id) {
    input.application_id = event.application_id;
  }
  if (d.note.trim() !== "") input.note = d.note.trim();
  return input;
}

/** What an exclusion ignores, in the words the list uses. */
export function describeIgnored(e: WafExclusion): string {
  switch (e.shape) {
    case "target":
      return `${e.collection}:${e.target_name}`;
    case "path":
      return e.path_pattern.endsWith("*")
        ? `paths beginning ${e.path_pattern.slice(0, -1)}`
        : `the path ${e.path_pattern}`;
    default:
      return "the whole rule";
  }
}

/**
 * How far an exclusion reaches. Install-wide and turning a rule off are the two that
 * widen what goes unwatched, and the list says so in colour as well as in words.
 */
export function scopeBadge(e: WafExclusion): {
  label: string;
  tone: "warning" | "neutral" | "muted";
} {
  if (e.scope === "install")
    return { label: "Every application", tone: "warning" };
  if (e.inert)
    return { label: `${e.application_name} · no domains`, tone: "muted" };
  return { label: e.application_name, tone: "neutral" };
}

/** The line on the WAF card, or null when there is nothing silenced. */
export function exclusionsNotice(total: number, inert: number): string | null {
  if (total === 0) return null;
  const n = `${total} ${total === 1 ? "exclusion" : "exclusions"}`;
  const quiet = "Detections they cover are not recorded.";
  return inert > 0
    ? `${n} in place, ${inert} of them apply to nothing yet. ${quiet}`
    : `${n} in effect. ${quiet}`;
}
