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
