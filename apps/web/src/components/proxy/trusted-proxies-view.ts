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

interface Cidr {
  v6: boolean;
  value: bigint;
  bits: number;
}

/** Parses an address or CIDR; null for anything that is not one. IPv4-mapped IPv6 is the IPv4 range, as the server reads it. */
function parseCidr(s: string): Cidr | null {
  const slash = s.indexOf("/");
  const host = slash < 0 ? s : s.slice(0, slash);
  const prefixText = slash < 0 ? null : s.slice(slash + 1);
  if (prefixText !== null && !/^\d{1,3}$/.test(prefixText)) return null;

  let value: bigint | null;
  let v6: boolean;
  if (host.includes(":")) {
    value = parseV6(host);
    v6 = true;
  } else {
    value = parseV4(host);
    v6 = false;
  }
  if (value === null) return null;

  const bits = prefixText === null ? (v6 ? 128 : 32) : Number(prefixText);
  if (bits > (v6 ? 128 : 32)) return null;
  // ::ffff:a.b.c.d/N is the IPv4 range a.b.c.d/(N-96).
  if (v6 && value >> 32n === 0xffffn) {
    if (bits < 96) return null;
    return { v6: false, value: value & 0xffffffffn, bits: bits - 96 };
  }
  return { v6, value, bits };
}

function parseV4(s: string): bigint | null {
  const parts = s.split(".");
  if (parts.length !== 4) return null;
  let v = 0n;
  for (const p of parts) {
    if (!/^\d{1,3}$/.test(p) || Number(p) > 255) return null;
    v = (v << 8n) | BigInt(p);
  }
  return v;
}

function parseV6(s: string): bigint | null {
  if (s.includes("%")) return null;
  let head = s;
  let tail4: bigint | null = null;
  // A trailing dotted quad is two groups.
  const lastColon = s.lastIndexOf(":");
  if (s.slice(lastColon + 1).includes(".")) {
    tail4 = parseV4(s.slice(lastColon + 1));
    if (tail4 === null) return null;
    head = s.slice(0, lastColon + 1) + "0:0";
  }
  const halves = head.split("::");
  if (halves.length > 2) return null;
  const groups = (h: string) => (h === "" ? [] : h.split(":"));
  const a = groups(halves[0]);
  const b = halves.length === 2 ? groups(halves[1]) : [];
  const missing = 8 - a.length - b.length;
  if (halves.length === 2 ? missing < 1 : missing !== 0) return null;
  const all = [
    ...a,
    ...Array<string>(halves.length === 2 ? missing : 0).fill("0"),
    ...b,
  ];
  if (all.length !== 8) return null;
  let v = 0n;
  for (const g of all) {
    if (!/^[0-9a-fA-F]{1,4}$/.test(g)) return null;
    v = (v << 16n) | BigInt(`0x${g}`);
  }
  if (tail4 !== null) v = (v & ~0xffffffffn) | tail4;
  return v;
}

function overlaps(a: Cidr, b: Cidr): boolean {
  if (a.v6 !== b.v6) return false;
  const max = a.v6 ? 128 : 32;
  const n = Math.min(a.bits, b.bits);
  return a.value >> BigInt(max - n) === b.value >> BigInt(max - n);
}

/**
 * Which of these ranges overlap a network the proxy is attached to, worked out as the
 * operator types so the warning comes BEFORE the save. Anything this cannot parse
 * claims no overlap: it is a preview, the server's answer after the save is the
 * authoritative one, and a preview that guessed would warn about the wrong thing.
 */
export function overlapsOf(
  ranges: readonly string[],
  networks: readonly { name: string; subnet: string }[],
): { cidr: string; network: string; subnet: string }[] {
  const out: { cidr: string; network: string; subnet: string }[] = [];
  for (const n of networks) {
    const subnet = parseCidr(n.subnet);
    if (!subnet) continue;
    for (const r of ranges) {
      const range = parseCidr(r);
      if (range && overlaps(range, subnet)) {
        out.push({ cidr: r, network: n.name, subnet: n.subnet });
      }
    }
  }
  return out;
}
