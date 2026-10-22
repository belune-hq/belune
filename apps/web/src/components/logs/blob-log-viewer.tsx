import { useMemo, useState } from "react";
import {
  LevelFilter,
  type LevelFilterValue,
} from "@/components/logs/level-filter";
import { LogView } from "@/components/logs/log-view";
import { parseLogBlob, type LogEntry } from "@/components/logs/parse";
import { cn } from "@/lib/utils";

/**
 * A leveled viewer for blob-style logs (build logs, backup/restore logs). Splits
 * a text blob (or accepts pre-built entries) into leveled lines, colors them on
 * the shared terminal surface, and offers a client-side level filter.
 */
export function BlobLogViewer({
  blob,
  entries: entriesProp,
  running = false,
  showFilter = true,
  showLevel = true,
  wrap = true,
  follow,
  emptyMessage = "No log output.",
  heightClass = "max-h-64",
  className,
}: {
  blob?: string;
  entries?: LogEntry[];
  running?: boolean;
  showFilter?: boolean;
  showLevel?: boolean;
  wrap?: boolean;
  // Whether to stick to the bottom (defaults to `running`). Pass true to open a
  // finished log at the bottom too — e.g. a completed build, for continuity
  // with the live viewer it replaces.
  follow?: boolean;
  emptyMessage?: string;
  heightClass?: string;
  className?: string;
}) {
  const [level, setLevel] = useState<LevelFilterValue>("");

  const allEntries = useMemo<LogEntry[]>(
    () => entriesProp ?? parseLogBlob(blob ?? ""),
    [entriesProp, blob],
  );

  const entries = useMemo(
    () => (level ? allEntries.filter((e) => e.level === level) : allEntries),
    [allEntries, level],
  );

  const empty = running ? "Waiting for output..." : emptyMessage;

  return (
    <div className={cn("space-y-2", className)}>
      {showFilter && allEntries.length > 0 && (
        <LevelFilter value={level} onChange={setLevel} />
      )}
      <LogView
        entries={entries}
        follow={follow ?? running}
        wrap={wrap}
        showLevel={showLevel}
        showTimestamp
        emptyMessage={level ? "No lines match this level." : empty}
        className={cn("rounded", heightClass)}
      />
    </div>
  );
}
