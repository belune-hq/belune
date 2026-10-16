import cronstrue from "cronstrue";

// Human-readable labels for the common cron presets exposed in the backup
// config forms. Falls back to the raw expression for custom schedules, and to
// "Manual only" for an empty schedule.
const SCHEDULE_LABELS: Record<string, string> = {
  "0 * * * *": "Every hour",
  "0 0 * * *": "Every day at midnight",
  "0 13 * * *": "Every day at 1:00 PM",
  "0 0 * * 0": "Every week (Sunday midnight)",
  "0 0 1 * *": "Every month (1st, midnight)",
};

export function humanizeSchedule(schedule: string): string {
  if (!schedule) return "Manual only";
  return SCHEDULE_LABELS[schedule] ?? schedule;
}

const DESCRIPTORS: Record<string, string> = {
  "@hourly": "Every hour",
  "@daily": "Every day at midnight",
  "@midnight": "Every day at midnight",
  "@weekly": "Every week (Sunday midnight)",
  "@monthly": "Every month (1st, midnight)",
  "@yearly": "Every year (1 January, midnight)",
  "@annually": "Every year (1 January, midnight)",
};

// describeCronOrNull is plain language for a cron expression, or null when it
// cannot say. cronstrue THROWS on malformed input and the task form calls this
// on every keystroke, so partial expressions ("0 3 * *") must land here as null,
// never as a thrown message in the UI. The backend also accepts descriptors
// (`@daily`, `@every 1h30m`), which cronstrue does not parse. Presets in
// SCHEDULE_LABELS win so the backup forms' wording does not change.
export function describeCronOrNull(expr: string): string | null {
  const e = expr.trim();
  if (!e) return null;
  const preset = SCHEDULE_LABELS[e] ?? DESCRIPTORS[e];
  if (preset) return preset;
  const every = /^@every\s+(\S+)$/.exec(e);
  if (every) return `Every ${every[1]}`;
  if (e.startsWith("@")) return null;
  try {
    return cronstrue.toString(e, { use24HourTimeFormat: true });
  } catch {
    return null;
  }
}

// describeCron falls back to the raw expression, for list displays where
// showing something beats showing nothing.
export function describeCron(expr: string): string {
  return describeCronOrNull(expr) ?? expr;
}

// Go-style duration ("1h30m", "90s", "500ms") in seconds, or null if malformed.
function everySeconds(d: string): number | null {
  const unit: Record<string, number> = {
    h: 3600,
    m: 60,
    s: 1,
    ms: 0.001,
    us: 1e-6,
    µs: 1e-6,
    ns: 1e-9,
  };
  const re = /(\d+(?:\.\d+)?)(ms|us|µs|ns|h|m|s)/y;
  let total = 0;
  let pos = 0;
  while (pos < d.length) {
    re.lastIndex = pos;
    const m = re.exec(d);
    if (!m) return null;
    total += Number(m[1]) * unit[m[2]];
    pos = re.lastIndex;
  }
  return pos > 0 ? total : null;
}

// cronError is the form-validation counterpart of describeCronOrNull: undefined
// when expr is acceptable (empty is left to the caller, which decides whether a
// schedule is required), otherwise the message to show. A shape check alone
// ("five tokens") let `0 99 * * *` through to fail only on Save. The server stays
// the authority; this just agrees with it. cronstrue also accepts 6/7-field
// forms the backend rejects, hence the explicit field count.
// minMinute mirrors the task schedule's "at least a minute apart" rule.
export function cronError(
  expr: string,
  opts: { minMinute?: boolean } = {},
): string | undefined {
  const e = expr.trim();
  if (!e) return undefined;
  if (e.startsWith("@")) {
    const every = /^@every\s+(\S+)$/.exec(e);
    if (every) {
      const secs = everySeconds(every[1]);
      if (secs === null) return "Not a valid duration, e.g. @every 1h30m";
      if (opts.minMinute && secs < 60)
        return "Schedules must be at least one minute apart";
      return undefined;
    }
    return describeCronOrNull(e) ? undefined : "Not a valid cron expression";
  }
  if (e.split(/\s+/).length !== 5) return "Use a 5-field cron expression";
  return describeCronOrNull(e) ? undefined : "Not a valid cron expression";
}
