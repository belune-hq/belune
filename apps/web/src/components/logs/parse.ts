import { normalizeLevel, type LogLevel } from "@/lib/logs/level";

// A single renderable log line, normalized across every source (container logs
// from the DB, or NDJSON entries from a build/backup/restore run).
export interface LogEntry {
  id: string;
  level: LogLevel;
  message: string;
  stream?: string;
  recordedAt?: string | null;
  // The container generation (session) this line belongs to, when known. null
  // is the "earlier / unassigned" bucket; undefined means the source has no
  // sessions. Keyed by container rather than deployment so databases — which
  // have no deployment but are replaced on upgrade — get sessions too.
  sessionId?: string | null;
  // When set, this entry is a session divider rather than a log line; the string
  // is the label to render. Used to separate deployments in the merged view.
  divider?: string;
}

// Shape of one NDJSON log entry. joblog (build/backup/restore) writes `ts` as an
// ISO string; Caddy writes it as a numeric epoch-seconds float (e.g. 1699564800.1).
interface RawEntry {
  ts?: string | number;
  level?: string;
  msg?: string;
}

// Normalize a raw `ts` to an ISO string. A bare number is epoch seconds, which
// `new Date()` would otherwise read as milliseconds and render as 1970. Anything
// past ~2001 in real milliseconds is ≥ 1e12, so treat smaller numbers as seconds.
function normalizeTs(ts: string | number | undefined): string | null {
  if (ts == null) return null;
  if (typeof ts === "number") {
    const ms = ts < 1e12 ? ts * 1000 : ts;
    const d = new Date(ms);
    return Number.isNaN(d.getTime()) ? null : d.toISOString();
  }
  return ts;
}

// Parses an NDJSON log blob (build / backup / restore logs) — one JSON object
// per line: {"ts","level","msg"}. A line that isn't valid JSON falls back to a
// plain info entry so stray/legacy content still renders.
export function parseLogBlob(blob: string, idPrefix = "blob"): LogEntry[] {
  if (!blob) return [];
  const out: LogEntry[] = [];
  const lines = blob.replace(/\r\n/g, "\n").split("\n");
  for (let i = 0; i < lines.length; i++) {
    const line = lines[i].replace(/\r$/, "");
    if (!line) continue;

    // The console handler puts a record's attributes on an indented
    // continuation line. Fold it into the entry above rather than emitting a
    // second row, which would otherwise appear as a stray Info line detached
    // from the error it belongs to.
    const { rest } = splitDockerTimestamp(line);
    const prev = out[out.length - 1];
    if (prev && /^\s+\S/.test(rest)) {
      // Re-indent to a fixed two spaces rather than keeping the source's own
      // indent. The console handler pads continuation lines out to the width of
      // "<timestamp> <LEVEL> " so they line up in a terminal, where those are
      // part of the same text line — but here timestamp and level are separate
      // columns, so the message column starts at zero and that padding threw the
      // attributes ~26 characters to the right. Other producers indent by
      // different amounts again (postgres uses a tab), so normalising beats
      // preserving.
      prev.message += "\n  " + stripAnsi(rest.trim());
      continue;
    }

    out.push(parseLine(line, `${idPrefix}-${i}`));
  }
  return out;
}

// Docker prefixes each line with an RFC3339Nano timestamp when logs are
// requested with timestamps (the platform/Maintenance viewer does). Split it off
// so the remainder can still be parsed as JSON, and so services that print no
// timestamp of their own still get one. Mirrors splitTimestamp in the Go
// collector. Returns a null timestamp and the original line when there is no
// parseable prefix, which is what a stored blob log (build/backup) looks like.
function splitDockerTimestamp(line: string): {
  ts: string | null;
  rest: string;
} {
  const idx = line.indexOf(" ");
  if (idx > 0 && idx <= 36) {
    const candidate = line.slice(0, idx);
    // Cheap shape check before Date parsing, so ordinary words that happen to
    // sit at the start of a line are not mistaken for timestamps.
    if (/^\d{4}-\d{2}-\d{2}T/.test(candidate)) {
      const d = new Date(candidate);
      if (!Number.isNaN(d.getTime())) {
        return { ts: d.toISOString(), rest: line.slice(idx + 1) };
      }
    }
  }
  return { ts: null, rest: line };
}

// Belune's own console format, written by logger.ConsoleHandler:
//
//   2026-07-22 10:00:04 INFO  [worker.deploy] deploy finished
//
// Mirrors consoleRe in apps/api/internal/pkg/loglevel/loglevel.go. Without this
// branch every platform log line falls to the flat `level: "info"` default
// below — errors included — because the fallback does no detection at all.
const CONSOLE_RE =
  /^(\d{4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2}) +(DEBUG|INFO|WARN|WARNING|ERROR) +(.*)$/;

// SGR escape sequences. Colour is meant to be off wherever output is captured,
// but build tools (railpack, pack) colour their output regardless, and a forced
// LOG_COLOR=always would too. Stripping keeps "[32m" out of the rendered text
// and keeps CONSOLE_RE — which anchors on the timestamp — matching.
// eslint-disable-next-line no-control-regex
const ANSI_RE = /\x1b\[[0-9;]*m/g;

/**
 * Remove ANSI SGR (colour) escape sequences from a log line. Apps that colourise
 * their output — NestJS/winston, many CLIs — emit `\x1b[32m…\x1b[39m`; the ESC is
 * a non-printing control byte, so a viewer that prints the line verbatim shows
 * the leftover `[32m` bodies as garbage. Strip the whole sequence instead.
 */
export function stripAnsi(s: string): string {
  return s.replace(ANSI_RE, "");
}

function parseLine(line: string, id: string): LogEntry {
  const { ts: dockerTs, rest: raw } = splitDockerTimestamp(line);
  const rest = stripAnsi(raw);
  try {
    const obj = JSON.parse(rest) as RawEntry;
    if (obj && typeof obj.msg === "string") {
      return {
        id,
        level: normalizeLevel(obj.level ?? "info"),
        // Strip here, not only on `rest`: a build log is NDJSON, which escapes
        // the ESC as a six-character sequence the raw-string strip above cannot
        // match. JSON.parse has only now revived it to a real escape in obj.msg.
        message: stripAnsi(obj.msg),
        // Docker's timestamp wins when present: it is uniform across services,
        // whereas the in-payload field is whatever that particular logger chose.
        recordedAt: dockerTs ?? normalizeTs(obj.ts),
      };
    }
  } catch {
    // not JSON — fall through
  }

  const m = CONSOLE_RE.exec(rest);
  if (m) {
    return {
      id,
      level: normalizeLevel(m[2]),
      // Keep "[module] …" in the message: which subsystem spoke is the point
      // of that field, and the viewer has no column of its own for it.
      message: m[3],
      // Read as UTC when Docker gave us nothing better. Containers run UTC, and
      // the platform viewer always requests Docker timestamps, so this fallback
      // only applies to stored blobs.
      recordedAt:
        dockerTs ?? new Date(m[1].replace(" ", "T") + "Z").toISOString(),
    };
  }

  return { id, level: "info", message: rest, recordedAt: dockerTs };
}
