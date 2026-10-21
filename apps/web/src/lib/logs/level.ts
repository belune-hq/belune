// Canonical log-severity vocabulary + heuristic detector, mirroring the Go
// implementation in apps/api/internal/pkg/loglevel/loglevel.go so a line is
// classified identically on both sides. Used by every log viewer (container,
// build, backup/restore) for per-line coloring and client-side level filtering.

export type LogLevel = "debug" | "info" | "warning" | "error";

export const LEVELS: LogLevel[] = ["debug", "info", "warning", "error"];

export const LEVEL_LABELS: Record<LogLevel, string> = {
  debug: "Debug",
  info: "Info",
  warning: "Warning",
  error: "Error",
};

// Text-color class per level, tuned for the dark terminal surface
// (bg-terminal-bg). Info inherits the container's default text color.
export const LEVEL_TEXT_CLASS: Record<LogLevel, string> = {
  debug: "text-terminal-dbg",
  info: "",
  warning: "text-terminal-warn",
  error: "text-terminal-err",
};

// Row background highlight per level. Only warning (yellow) and error (red) are
// highlighted so they stand out; debug/info stay on the plain terminal surface.
export const LEVEL_BG_CLASS: Record<LogLevel, string> = {
  debug: "",
  info: "",
  warning: "bg-terminal-warn/15",
  error: "bg-terminal-err/15",
};

// Maps an arbitrary level string (any casing / common alias) to a canonical
// LogLevel. Levels are assigned server-side (Go internal/pkg/loglevel); the
// frontend only normalizes what it receives, so no heuristic detector lives
// here.
export function normalizeLevel(s: string): LogLevel {
  switch (s.trim().toLowerCase()) {
    case "debug":
    case "trace":
    case "dbg":
      return "debug";
    case "warn":
    case "warning":
      return "warning";
    case "error":
    case "err":
    case "fatal":
    case "critical":
    case "crit":
    case "panic":
      return "error";
    default:
      return "info";
  }
}

