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

const pad = (n: string) => n.padStart(2, "0");

// describeCron renders the handful of shapes people actually write in plain
// language, and falls back to the raw expression for anything else — a wrong
// description is worse than none. Presets in SCHEDULE_LABELS win.
export function describeCron(expr: string): string {
  const preset = SCHEDULE_LABELS[expr.trim()];
  if (preset) return preset;
  const f = expr.trim().split(/\s+/);
  if (f.length !== 5) return expr;
  const [min, hour, dom, mon, dow] = f;
  const num = /^\d+$/;
  const everyDay = dom === "*" && mon === "*" && dow === "*";
  if (everyDay && min === "*" && hour === "*") return "Every minute";
  const step = /^\*\/(\d+)$/.exec(min);
  if (everyDay && step && hour === "*") return `Every ${step[1]} minutes`;
  if (everyDay && num.test(min) && hour === "*")
    return `Every hour at :${pad(min)}`;
  if (everyDay && num.test(min) && num.test(hour))
    return `Every day at ${pad(hour)}:${pad(min)}`;
  return expr;
}
