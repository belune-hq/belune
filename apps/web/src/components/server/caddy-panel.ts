import type { StatusTone } from "@/components/ui/status-pill";
import type { CaddyPanel } from "@/lib/types";

/**
 * What the Caddy panel says, as data. Kept out of the component so the one rule
 * that matters can be tested without a DOM: a value we could not determine is
 * "unknown", never "absent". jsdom cannot tell us a row is *visible*, but it can
 * tell us what the row *claims*.
 */
export interface CaddyRow {
  key: "container" | "admin" | "version" | "image" | "modules" | "access_log";
  label: string;
  tone: StatusTone;
  value: string;
  hint?: string;
}

/** Mirrors logtailer.missingLogGrace: Caddy creates the log on the first
 *  request, so a missing file is only a fault after this long. */
export const MISSING_LOG_GRACE_MS = 60_000;

const UNKNOWN = "Unknown";

export function describeCaddy(caddy: CaddyPanel, now: number): CaddyRow[] {
  return [
    containerRow(caddy),
    adminRow(caddy),
    versionRow(caddy),
    imageRow(caddy),
    modulesRow(caddy),
    accessLogRow(caddy, now),
  ];
}

function containerRow(c: CaddyPanel): CaddyRow {
  const name = c.discovery.container_name;
  const how =
    c.discovery.source === "env"
      ? "set by CADDY_CONTAINER_NAME"
      : c.discovery.source === "label"
        ? "found by its belune-system=caddy label"
        : c.discovery.source === "assumed"
          ? "assumed — nothing verified this name"
          : undefined;

  switch (c.container.state) {
    case "found":
      if (c.container.status && c.container.status !== "running") {
        return {
          key: "container",
          label: "Container",
          tone: "error",
          value: `${name} is ${c.container.status}`,
          hint: "Every app domain will fail until Caddy is running.",
        };
      }
      return {
        key: "container",
        label: "Container",
        // An assumed name that happens to exist is fine, but it is luck.
        tone: c.discovery.source === "assumed" ? "building" : "ready",
        value: name,
        hint: how,
      };
    case "not_found":
      return {
        key: "container",
        label: "Container",
        // Naming it (or having found it by label) and then not finding it is a
        // fault. Merely assuming it is the topology with Caddy outside Docker,
        // which is supported, so it must not read as an error.
        tone: c.discovery.source === "assumed" ? "building" : "error",
        value: `No container named ${name}`,
        hint:
          c.discovery.source === "assumed"
            ? "No container carries belune-system=caddy. If Caddy runs inside Docker, app domains will return 502 until it does; set CADDY_CONTAINER_NAME to name it. If it runs outside Docker, this is expected."
            : "App domains will return 502 until it exists.",
      };
    default:
      return {
        key: "container",
        label: "Container",
        tone: "neutral",
        value: name || UNKNOWN,
        hint: "Docker could not be asked, so whether this container exists is not known.",
      };
  }
}

function adminRow(c: CaddyPanel): CaddyRow {
  const url = c.admin.url;
  switch (c.admin.state) {
    case "healthy":
      return {
        key: "admin",
        label: "Admin API",
        tone: "ready",
        value: "Reachable",
        hint: url,
      };
    case "unreachable":
      return {
        key: "admin",
        label: "Admin API",
        tone: "error",
        value: "Not answering",
        hint: `${url} — Belune cannot change routes until this answers. Check CADDY_ADMIN_URL.`,
      };
    case "lost_config":
      return {
        key: "admin",
        label: "Admin API",
        tone: "building",
        value: "Answering, but holds none of Belune's routes",
        hint: "Caddy likely restarted; routes are re-applied automatically.",
      };
    default:
      return {
        key: "admin",
        label: "Admin API",
        tone: "neutral",
        value: "Not checked yet",
        hint: url,
      };
  }
}

function versionRow(c: CaddyPanel): CaddyRow {
  const v = c.version;
  if (v.value === null) {
    return {
      key: "version",
      label: "Version",
      tone: "neutral",
      value: UNKNOWN,
      hint: "Could not be read from the image labels or by asking Caddy.",
    };
  }
  return {
    key: "version",
    label: "Version",
    tone: "ready",
    value: v.value,
    hint:
      v.upstream && v.upstream !== v.value
        ? `Caddy ${v.upstream}`
        : v.source === "exec"
          ? "Read from the running Caddy"
          : undefined,
  };
}

function imageRow(c: CaddyPanel): CaddyRow {
  return {
    key: "image",
    label: "Image",
    tone: c.image === null ? "neutral" : "ready",
    value: c.image ?? UNKNOWN,
    hint:
      c.image === null
        ? "Docker did not report a tag — it may have moved since this container started."
        : undefined,
  };
}

function modulesRow(c: CaddyPanel): CaddyRow {
  if (!c.modules.known) {
    return {
      key: "modules",
      label: "Modules",
      tone: "neutral",
      value: UNKNOWN,
      // Saying "none" here would send someone to rebuild an image that was fine.
      hint: "Belune could not list this Caddy's modules (it is restarting, or has no exec access). Nothing is known about what it lacks.",
    };
  }
  const lacking = Object.entries(c.features)
    .filter(([, f]) => f.state === "unavailable")
    .map(([name]) => name.replace(/_/g, " "));
  return {
    key: "modules",
    label: "Modules",
    tone: "ready",
    value: `${c.modules.ids.length} loaded`,
    hint:
      lacking.length > 0
        ? `Not available here: ${lacking.join(", ")}. Use the Belune Caddy image to enable them.`
        : undefined,
  };
}

function accessLogRow(c: CaddyPanel, now: number): CaddyRow {
  const a = c.access_log;
  const where = a.resolved_path || a.path;
  const base = { key: "access_log", label: "Request log" } as const;

  switch (a.state) {
    case "ingesting":
      return {
        ...base,
        tone: "ready",
        value: `${a.rows_inserted.toLocaleString()} requests recorded`,
        hint: where,
      };
    case "no_app_traffic":
      return {
        ...base,
        tone: "ready",
        value: "Reading, none for an app domain yet",
        hint: `${a.lines_read.toLocaleString()} requests logged, all for other hosts. ${where}`,
      };
    case "no_traffic":
      return {
        ...base,
        tone: "ready",
        value: "Watching, no requests yet",
        hint: `Counting since this Belune started. ${where}`,
      };
    case "starting":
      return { ...base, tone: "neutral", value: "Starting", hint: where };
    case "missing": {
      const waited = now - Date.parse(a.since);
      if (Number.isFinite(waited) && waited < MISSING_LOG_GRACE_MS) {
        return {
          ...base,
          tone: "neutral",
          value: "Waiting for Caddy to create it",
          hint: where,
        };
      }
      return {
        ...base,
        tone: "error",
        value: "File does not exist",
        hint: `Request logging is recording nothing. Check ACCESS_LOG_PATH and that Caddy's log volume is mounted here. Looking at ${where}`,
      };
    }
    case "unreadable":
      return {
        ...base,
        tone: "error",
        value: "File cannot be read",
        hint: `Check its permissions. ${where}`,
      };
    case "error":
      return {
        ...base,
        tone: "error",
        value: "Ingestion is failing",
        hint: a.last_error,
      };
    default:
      return {
        ...base,
        tone: "neutral",
        value: UNKNOWN,
        hint: "Request-log status is not available.",
      };
  }
}

/** The pill in the panel header: the worst thing any row says. Unknown rows
 *  never read as a fault — but they never read as "OK" either. */
export function summarize(rows: CaddyRow[]): {
  tone: StatusTone;
  label: string;
} {
  const errors = rows.filter((r) => r.tone === "error").length;
  if (errors > 0) {
    return {
      tone: "error",
      label: errors === 1 ? "1 problem" : `${errors} problems`,
    };
  }
  if (rows.some((r) => r.tone === "building")) {
    return { tone: "building", label: "Check details" };
  }
  // The image row is excluded: a moved tag makes it unknown on a perfectly
  // healthy install, and a standing "partly unknown" would train people to
  // ignore the pill.
  if (rows.some((r) => r.tone === "neutral" && r.key !== "image")) {
    return { tone: "neutral", label: "Partly unknown" };
  }
  return { tone: "ready", label: "OK" };
}
