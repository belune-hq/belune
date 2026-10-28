import { isBanTarget } from "@/components/proxy/crowdsec-view";

// These mirror proxy.NormalizeTrustedProxies. The server is authoritative and
// says no with its own words; they are here so a pasted list is refused before
// the round trip, not so the two can drift apart quietly.
export const MAX_TRUSTED_PROXIES = 64;
const MIN_BITS_V4 = 8;
const MIN_BITS_V6 = 16;

/**
 * Reads a pasted list: one range per line, but commas and spaces work too, since
 * both are how published lists get copied around. Order is kept and repeats are
 * dropped, so what the operator sees is what is sent.
 */
export function parseRanges(text: string): string[] {
  const seen = new Set<string>();
  const out: string[] = [];
  for (const part of text.split(/[\s,]+/)) {
    if (part === "" || seen.has(part)) continue;
    seen.add(part);
    out.push(part);
  }
  return out;
}

/** One range per line, which is how the box shows a list. */
export function formatRanges(ranges: readonly string[]): string {
  return ranges.join("\n");
}

function clip(s: string): string {
  return s.length > 40 ? `${s.slice(0, 40)}…` : s;
}

/** The first thing wrong with a pasted list, or undefined. Empty is valid: it turns trusting off. */
export function validateRanges(text: string): string | undefined {
  const ranges = parseRanges(text);
  if (ranges.length > MAX_TRUSTED_PROXIES) {
    return `At most ${MAX_TRUSTED_PROXIES} ranges. Cloudflare's whole list is 22.`;
  }
  for (const r of ranges) {
    if (!isBanTarget(r)) {
      return `"${clip(r)}" is not an IP address or a CIDR range.`;
    }
    const bits = r.includes("/") ? Number(r.split("/")[1]) : undefined;
    if (bits === undefined) continue;
    const min = r.includes(":") ? MIN_BITS_V6 : MIN_BITS_V4;
    if (bits < min) {
      return `"${clip(r)}" is too wide: every address in it could claim to be any visitor. The shortest allowed is /${min}.`;
    }
  }
  return undefined;
}

/** The list with a preset's ranges added, keeping what was already there. */
export function addRanges(text: string, extra: readonly string[]): string {
  return formatRanges(parseRanges([text, ...extra].join("\n")));
}

/** Whether two lists name the same ranges, in any order. */
export function sameRanges(
  a: readonly string[],
  b: readonly string[],
): boolean {
  if (a.length !== b.length) return false;
  const set = new Set(a);
  return b.every((r) => set.has(r));
}

/** A line for how many ranges are trusted, or why none are. */
export function describeTrusted(count: number): string {
  if (count === 0) {
    return "None. Visitors are identified by the address that connects to Caddy.";
  }
  return `${count} ${count === 1 ? "range" : "ranges"} trusted.`;
}
