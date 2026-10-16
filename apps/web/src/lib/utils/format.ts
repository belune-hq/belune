const MONTHS = [
  "Jan",
  "Feb",
  "Mar",
  "Apr",
  "May",
  "Jun",
  "Jul",
  "Aug",
  "Sep",
  "Oct",
  "Nov",
  "Dec",
];

const pad2 = (n: number) => String(n).padStart(2, "0");

/**
 * Absolute local timestamp for table cells: "YYYY-MM-DD HH:mm:ss".
 * Built manually (not Intl) so the format is identical across locales.
 */
export function formatDateTime(date: string | Date): string {
  const d = new Date(date);
  return (
    `${d.getFullYear()}-${pad2(d.getMonth() + 1)}-${pad2(d.getDate())} ` +
    `${pad2(d.getHours())}:${pad2(d.getMinutes())}:${pad2(d.getSeconds())}`
  );
}

/** Absolute local timestamp for summaries/detail views: "DD MMM YYYY, HH:mm". */
export function formatDateTimeShort(date: string | Date): string {
  const d = new Date(date);
  return (
    `${pad2(d.getDate())} ${MONTHS[d.getMonth()]} ${d.getFullYear()}, ` +
    `${pad2(d.getHours())}:${pad2(d.getMinutes())}`
  );
}

/** Compact relative time, e.g. "now", "5m", "3h", "2d", falling back to a date. */
export function formatRelativeTime(date: string | Date): string {
  const then = new Date(date).getTime();
  const seconds = Math.floor((Date.now() - then) / 1000);
  if (seconds < 45) return "now";
  const minutes = Math.floor(seconds / 60);
  if (minutes < 60) return `${minutes}m`;
  const hours = Math.floor(minutes / 60);
  if (hours < 24) return `${hours}h`;
  const days = Math.floor(hours / 24);
  if (days < 7) return `${days}d`;
  return new Intl.DateTimeFormat("en-US", {
    month: "short",
    day: "numeric",
  }).format(new Date(date));
}

export function formatBytes(bytes: number): string {
  const units = ["B", "KB", "MB", "GB", "TB"];
  let i = 0;
  while (bytes >= 1024 && i < units.length - 1) {
    bytes /= 1024;
    i++;
  }
  return `${bytes.toFixed(i > 0 ? 1 : 0)} ${units[i]}`;
}

export function formatDuration(ms: number): string {
  if (ms < 1000) return `${ms}ms`;
  if (ms < 60000) return `${(ms / 1000).toFixed(1)}s`;
  return `${(ms / 60000).toFixed(1)}m`;
}

/** Uptime in seconds → "3d 4h" / "5h 12m" / "8m" / "42s". */
export function formatUptime(seconds: number): string {
  if (seconds < 60) return `${seconds}s`;
  const m = Math.floor(seconds / 60);
  if (m < 60) return `${m}m`;
  const h = Math.floor(m / 60);
  if (h < 24) return `${h}h ${m % 60}m`;
  const d = Math.floor(h / 24);
  return `${d}d ${h % 24}h`;
}

const listFormatter = new Intl.ListFormat("en", {
  style: "long",
  type: "conjunction",
});

/** Renders names as "a", "a and b", "a, b, and c". */
export function formatList(items: string[]): string {
  return listFormatter.format(items);
}

/**
 * A timestamp in a specific IANA zone, with the zone's short name — for times
 * that mean something in a zone other than the viewer's (a cron's next run).
 * Falls back to the viewer's zone if the name is unknown to this browser.
 */
export function formatInTimezone(
  date: string | Date,
  timeZone: string,
): string {
  const opts: Intl.DateTimeFormatOptions = {
    day: "2-digit",
    month: "short",
    year: "numeric",
    hour: "2-digit",
    minute: "2-digit",
    hour12: false,
    timeZoneName: "short",
  };
  try {
    return new Date(date).toLocaleString("en-GB", { ...opts, timeZone });
  } catch {
    return new Date(date).toLocaleString("en-GB", opts);
  }
}
