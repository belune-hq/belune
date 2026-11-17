import { useEffect, useRef } from "react";
import {
  LEVEL_BG_CLASS,
  LEVEL_LABELS,
  LEVEL_TEXT_CLASS,
} from "@/lib/logs/level";
import { cn } from "@/lib/utils";
import { formatDateTime } from "@/lib/utils/format";
import type { LogEntry } from "./parse";

function LogLine({
  entry,
  showTimestamp,
  showLevel,
  sourceWidth,
  wrap,
}: {
  entry: LogEntry;
  showTimestamp?: boolean;
  showLevel?: boolean;
  sourceWidth: number;
  wrap?: boolean;
}) {
  // A session divider: a full-width labeled rule separating one deployment's
  // logs from the next in the merged ("all sessions") view.
  if (entry.divider !== undefined) {
    return (
      <div className="flex items-center gap-2 py-1 text-zinc-500 select-none">
        <span className="h-px flex-1 bg-zinc-700" />
        <span className="shrink-0 tracking-wide uppercase">
          {entry.divider}
        </span>
        <span className="h-px flex-1 bg-zinc-700" />
      </div>
    );
  }

  // When wrapping, hang-indent continuation lines so they align under the
  // message column (timestamp = 20 chars incl. trailing space, level = 10).
  // The source column (when there is one) sits between them: its width plus the
  // trailing space feeds this same sum, or wrapped lines misalign.
  const sourceCh = sourceWidth > 0 ? sourceWidth + 1 : 0;
  const indentCh = (showTimestamp ? 20 : 0) + sourceCh + (showLevel ? 10 : 0);
  const hangingIndent =
    wrap && indentCh > 0
      ? { paddingLeft: `${indentCh}ch`, textIndent: `-${indentCh}ch` }
      : undefined;

  return (
    <div
      style={hangingIndent}
      className={cn(
        LEVEL_TEXT_CLASS[entry.level],
        LEVEL_BG_CLASS[entry.level],
        "hover:bg-white/5",
      )}
    >
      {showTimestamp && entry.recordedAt && (
        <span className="text-zinc-500">
          {formatDateTime(entry.recordedAt)}{" "}
        </span>
      )}
      {sourceWidth > 0 && (
        // Padded to the column width even when this line has no source of its own, so
        // the level and message columns stay aligned down the view.
        <span className="text-zinc-400">
          {(entry.source ?? "")
            .slice(0, sourceWidth)
            .padEnd(sourceWidth, " ")}{" "}
        </span>
      )}
      {showLevel &&
        // Pad to a fixed width so the message column aligns across levels
        // ("[Warning]" is the widest label at 9 chars). The parent <pre>
        // preserves the padding spaces.
        `[${LEVEL_LABELS[entry.level]}]`.padEnd(10, " ")}
      {entry.message}
    </div>
  );
}

/**
 * Shared log surface: a scrollable dark terminal `<pre>` that renders leveled
 * entries with per-level coloring. Used by the container log viewer and by the
 * build / backup / restore blob viewers so every log reads consistently.
 *
 * When `follow` is true, it auto-scrolls to the bottom as entries change.
 */
export function LogView({
  entries,
  follow = true,
  showTimestamp = false,
  showLevel = false,
  sourceWidth = 0,
  wrap = false,
  isLoading = false,
  error,
  emptyMessage = "No logs.",
  className,
}: {
  entries: LogEntry[];
  follow?: boolean;
  showTimestamp?: boolean;
  showLevel?: boolean;
  // Width in characters of the per-line source column; 0 (the default) renders none.
  // The viewer computes it, so the hanging-indent math stays in one place. Off by
  // default because the build, backup and restore viewers share this component.
  sourceWidth?: number;
  wrap?: boolean;
  isLoading?: boolean;
  error?: string | null;
  emptyMessage?: string;
  className?: string;
}) {
  const scrollRef = useRef<HTMLPreElement>(null);

  useEffect(() => {
    if (follow && scrollRef.current) {
      scrollRef.current.scrollTop = scrollRef.current.scrollHeight;
    }
  }, [entries, follow]);

  return (
    <pre
      ref={scrollRef}
      className={cn(
        "bg-terminal-bg overflow-auto p-4 font-mono text-xs text-zinc-200",
        wrap
          ? "[overflow-wrap:anywhere] whitespace-pre-wrap"
          : "whitespace-pre",
        className,
      )}
    >
      {isLoading ? (
        <span className="text-zinc-500">Loading logs...</span>
      ) : error ? (
        <span className="text-terminal-err">{error}</span>
      ) : entries.length === 0 ? (
        <span className="text-zinc-500">{emptyMessage}</span>
      ) : (
        // ⚠️ A scroll-content box, and the rows must be inside it.
        //
        // Each row is a block whose backgrounds — the per-level tint and the
        // hover highlight — paint across its own box. Directly inside the
        // scroller that box is the VISIBLE width, while `whitespace-pre` lets
        // the text run past it, so scrolling right left the text sitting on
        // bare terminal background with its level colour and hover gone.
        // Measured on a real stack: 104px of dead strip at full scroll.
        //
        // `w-max` makes this box as wide as the widest row's content, so every
        // row stretches to that one width and every background spans the whole
        // scrollable area. Putting `w-max min-w-full` on the ROWS instead does
        // not work: `min-width: 100%` resolves against the scroller's visible
        // width, so every row narrower than the widest is still cut short —
        // checked in the browser before writing this.
        //
        // ⛔ Only when not wrapping. `whitespace-pre-wrap` inside a
        // `width: max-content` box lays out against the longest unbroken line
        // and never wraps, which is the opposite of what the Wrap toggle asks
        // for.
        <div className={cn("min-w-full", wrap ? "w-full" : "w-max")}>
          {entries.map((entry) => (
            <LogLine
              key={entry.id}
              entry={entry}
              showTimestamp={showTimestamp}
              showLevel={showLevel}
              sourceWidth={sourceWidth}
              wrap={wrap}
            />
          ))}
        </div>
      )}
    </pre>
  );
}
