import { BoxesIcon, ClockIcon, LayersIcon, ListIcon } from "lucide-react";
import { useCallback, useMemo, useRef, useState } from "react";
import {
  LevelFilter,
  type LevelFilterValue,
} from "@/components/logs/level-filter";
import { LogView } from "@/components/logs/log-view";
import { mergeLogEntries } from "@/components/logs/merge";
import { stripAnsi, type LogEntry } from "@/components/logs/parse";
import { Button } from "@/components/ui/button";
import { Card, CardContent } from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import type { ContainerLogSource } from "@/lib/api/container-logs";
import {
  useContainerLogs,
  useContainerLogSessions,
} from "@/lib/hooks/use-container-logs";
import { useChannel } from "@/lib/hooks/use-websocket";
import { normalizeLevel } from "@/lib/logs/level";
import type { ContainerLogSession } from "@/lib/types";
import { formatRelativeTime } from "@/lib/utils/format";

// Sentinel session-selector values distinct from any deployment UUID.
const SESSION_ALL = "all";
const SESSION_NONE = "none"; // the unassigned / "earlier logs" bucket
// Sentinel for the process-type selector. It contains an underscore, which a real
// name (^[a-z][a-z0-9-]{0,19}$) can never, so no type can collide with it.
const TYPE_ALL = "__all__";

// Identity of a session is the container generation. Rows collected before
// sessions existed have none and fall into the unassigned bucket.
function sessionKey(s: ContainerLogSession): string {
  return s.container_id ?? SESSION_NONE;
}

// A short, human-readable label. An application session is named by the deploy
// that produced it ("#a1b2c3d · rollback · 2h ago"); a database has no
// deployment, so its runs are named by when they started ("Run · 2h ago").
//
// All of an application's process types come from ONE deployment, so the deploy
// alone labels three containers identically; the type is what tells them apart.
function sessionLabel(s: ContainerLogSession): string {
  if (!s.container_id) return "Earlier logs";
  const when = s.started_at ?? s.first_at ?? s.last_at;
  if (s.deployment_id) {
    const parts = [`#${s.deployment_id.slice(0, 7)}`];
    if (s.process_type) parts.push(s.process_type);
    if (s.triggered_by) parts.push(s.triggered_by);
    if (when) parts.push(formatRelativeTime(when));
    return parts.join(" · ");
  }
  return when
    ? `Run · ${formatRelativeTime(when)}`
    : `Run · ${s.container_id.slice(0, 12)}`;
}

const MAX_LIVE = 5000;

const LIMIT_OPTIONS = [100, 300, 500, 1000] as const;

const TIME_RANGES = [
  { value: "all", label: "All time" },
  { value: "15m", label: "Last 15 min" },
  { value: "1h", label: "Last hour" },
  { value: "6h", label: "Last 6 hours" },
  { value: "24h", label: "Last 24 hours" },
  { value: "7d", label: "Last 7 days" },
] as const;

const RANGE_MS: Record<string, number> = {
  "15m": 15 * 60_000,
  "1h": 60 * 60_000,
  "6h": 6 * 60 * 60_000,
  "24h": 24 * 60 * 60_000,
  "7d": 7 * 24 * 60 * 60_000,
};

/**
 * Shared live + historical log viewer for application and database containers.
 * Merges the paginated history query with the live WebSocket stream
 * (container-logs:{sourceId}), and offers level / keyword / limit / time-range
 * filters plus a line-wrap toggle.
 */
export function ContainerLogViewer({
  source,
  projectId,
  sourceId,
}: {
  source: ContainerLogSource;
  projectId: string;
  sourceId: string;
}) {
  const [q, setQ] = useState("");
  const [inputValue, setInputValue] = useState("");
  const [level, setLevel] = useState<LevelFilterValue>("");
  const [limit, setLimit] = useState(500);
  const [timeRange, setTimeRange] = useState("all");
  const [session, setSession] = useState<string>(SESSION_ALL);
  const [processType, setProcessType] = useState<string>(TYPE_ALL);
  const [wrap, setWrap] = useState(false);
  const [follow, setFollow] = useState(true);
  const [liveLogs, setLiveLogs] = useState<LogEntry[]>([]);
  const liveIdRef = useRef(0);
  const debounceRef = useRef<ReturnType<typeof setTimeout> | null>(null);

  const { data: sessions } = useContainerLogSessions(
    source,
    projectId,
    sourceId,
  );

  // Server-side session filter: one container generation, the "none" bucket, or
  // (for SESSION_ALL) nothing.
  const sessionParam = session === SESSION_ALL ? undefined : session;
  const processTypeParam = processType === TYPE_ALL ? undefined : processType;

  // The selector offers the types the sessions actually contain, so it needs no
  // query of its own and cannot list a type that has produced no output. It is
  // hidden unless there is a choice to make, which keeps databases and
  // single-type applications exactly as they were.
  const processTypes = useMemo(() => {
    const names = new Set<string>();
    for (const s of sessions ?? [])
      if (s.process_type) names.add(s.process_type);
    return [...names].sort();
  }, [sessions]);
  const visibleSessions = useMemo(
    () =>
      (sessions ?? []).filter(
        (s) =>
          processTypeParam === undefined || s.process_type === processTypeParam,
      ),
    [sessions, processTypeParam],
  );

  function handleSearchChange(value: string) {
    setInputValue(value);
    if (debounceRef.current) clearTimeout(debounceRef.current);
    debounceRef.current = setTimeout(() => setQ(value), 400);
  }

  // Snapshot the lower bound when the range changes (kept stable so the query
  // key doesn't churn every render). Computed in the Select's onValueChange
  // below, not from `timeRange` via useMemo — reading the wall clock during
  // render is impure; an event handler has no such restriction.
  const [since, setSince] = useState<string | undefined>(undefined);

  const {
    data: history,
    isLoading,
    error,
  } = useContainerLogs(source, projectId, sourceId, {
    limit,
    q: q || undefined,
    level: level || undefined,
    since,
    session: sessionParam,
    processType: processTypeParam,
  });

  // Capture every live line unfiltered; filters are applied at render time so
  // that changing a filter retroactively re-filters already-received lines.
  const handleMessage = useCallback((_event: string, data: unknown) => {
    if (!data || typeof data !== "object") return;
    const obj = data as {
      level?: string;
      stream?: string;
      message?: string;
      recorded_at?: string;
      container_id?: string;
      process_type?: string | null;
    };
    if (typeof obj.message !== "string") return;

    liveIdRef.current += 1;
    setLiveLogs((prev) =>
      [
        ...prev,
        {
          id: `live-${liveIdRef.current}`,
          level: normalizeLevel(obj.level ?? "info"),
          stream: obj.stream === "stderr" ? "stderr" : "stdout",
          message: stripAnsi(obj.message as string),
          recordedAt: obj.recorded_at ?? new Date().toISOString(),
          // Empty string from the collector means no session → unassigned.
          sessionId: obj.container_id ? obj.container_id : null,
          processType: obj.process_type ?? null,
        },
      ].slice(-MAX_LIVE),
    );
  }, []);

  const { connected } = useChannel(`container-logs:${sourceId}`, handleMessage);

  // Map of session key → label, for divider rendering.
  const sessionLabels = useMemo(() => {
    const map = new Map<string, string>();
    for (const s of sessions ?? []) map.set(sessionKey(s), sessionLabel(s));
    return map;
  }, [sessions]);

  // History is most-recent first; reverse it to chronological, then merge the
  // live buffer in. Merging rather than appending matters because the collector
  // persists a line before it publishes it, so a refetch of history returns
  // lines the live buffer still holds — see mergeLogEntries.
  // History is already filtered server-side; live lines are filtered here
  // (including by the selected session, which the server applies to history).
  // Both sides run their message through stripAnsi, which is what lets the two
  // representations of one line compare equal.
  const entries = useMemo<LogEntry[]>(() => {
    const historical: LogEntry[] = history
      ? [...history].reverse().map((e) => ({
          id: e.id,
          level: normalizeLevel(e.level),
          stream: e.stream,
          message: stripAnsi(e.message),
          recordedAt: e.recorded_at,
          sessionId: e.container_id,
          processType: e.process_type,
        }))
      : [];
    const live = liveLogs.filter((e) => {
      if (level && e.level !== level) return false;
      if (q && !e.message.toLowerCase().includes(q.toLowerCase())) return false;
      if (processTypeParam !== undefined && e.processType !== processTypeParam)
        return false;
      if (session === SESSION_ALL) return true;
      const key = e.sessionId ?? null;
      if (session === SESSION_NONE) return key === null;
      return key === session;
    });
    const merged = mergeLogEntries(historical, live);

    // Only interleave session dividers in the "all sessions" view; when one
    // session is selected the whole surface is already that one run.
    if (session !== SESSION_ALL) return merged;

    // Skip dividers entirely when everything in view belongs to a single
    // session (databases, or an app with just one deployment) — a lone divider
    // header would be noise, and "Earlier logs" would misdescribe all of it.
    const distinctSessions = new Set(merged.map((e) => e.sessionId ?? null));
    if (distinctSessions.size < 2) return merged;

    const withDividers: LogEntry[] = [];
    let prevKey: string | null | undefined = undefined;
    for (let i = 0; i < merged.length; i++) {
      const e = merged[i];
      const key = e.sessionId ?? null;
      if (key !== prevKey) {
        const label =
          sessionLabels.get(e.sessionId ?? SESSION_NONE) ??
          (e.sessionId ? `Run · ${e.sessionId.slice(0, 12)}` : "Earlier logs");
        withDividers.push({
          id: `divider-${e.id}`,
          level: "info",
          message: "",
          divider: label,
        });
        prevKey = key;
      }
      withDividers.push(e);
    }
    return withDividers;
  }, [history, liveLogs, level, q, session, processTypeParam, sessionLabels]);

  const filtered =
    q ||
    level ||
    timeRange !== "all" ||
    session !== SESSION_ALL ||
    processType !== TYPE_ALL;
  const lineCount = entries.reduce(
    (n, e) => (e.divider === undefined ? n + 1 : n),
    0,
  );

  return (
    <div className="space-y-3 pt-3">
      <div className="flex flex-col gap-2 sm:flex-row sm:items-center sm:justify-between">
        <div className="flex flex-wrap items-center gap-2">
          <LevelFilter value={level} onChange={setLevel} />

          <Select
            value={String(limit)}
            onValueChange={(v) => v && setLimit(Number(v))}
          >
            <SelectTrigger
              className="h-8 w-40 capitalize"
              aria-label="Line limit"
            >
              <SelectValue />
            </SelectTrigger>
            <SelectContent>
              {LIMIT_OPTIONS.map((n) => (
                <SelectItem
                  key={n}
                  value={String(n)}
                  icon={<ListIcon />}
                  className="capitalize"
                >
                  {n} lines
                </SelectItem>
              ))}
            </SelectContent>
          </Select>

          <Select
            value={timeRange}
            onValueChange={(v) => {
              if (!v) return;
              setTimeRange(v);
              const ms = RANGE_MS[v];
              setSince(
                ms ? new Date(Date.now() - ms).toISOString() : undefined,
              );
            }}
          >
            <SelectTrigger
              className="h-8 w-44 capitalize"
              aria-label="Time range"
            >
              <SelectValue />
            </SelectTrigger>
            <SelectContent>
              {TIME_RANGES.map((r) => (
                <SelectItem
                  key={r.value}
                  value={r.value}
                  icon={<ClockIcon />}
                  className="capitalize"
                >
                  {r.label}
                </SelectItem>
              ))}
            </SelectContent>
          </Select>

          {processTypes.length > 1 && (
            <Select
              value={processType}
              onValueChange={(v) => {
                if (!v) return;
                setProcessType(v);
                // A session belongs to one type; keeping a stale one selected
                // would show an empty pane for a combination that cannot match.
                setSession(SESSION_ALL);
              }}
            >
              <SelectTrigger className="h-8 w-44" aria-label="Process type">
                <SelectValue />
              </SelectTrigger>
              <SelectContent>
                <SelectItem value={TYPE_ALL} icon={<BoxesIcon />}>
                  All process types
                </SelectItem>
                {processTypes.map((name) => (
                  <SelectItem key={name} value={name} icon={<BoxesIcon />}>
                    {name}
                  </SelectItem>
                ))}
              </SelectContent>
            </Select>
          )}

          {sessions && sessions.length > 1 && (
            <Select value={session} onValueChange={(v) => v && setSession(v)}>
              <SelectTrigger className="h-8 w-56" aria-label="Session">
                <SelectValue />
              </SelectTrigger>
              <SelectContent>
                <SelectItem value={SESSION_ALL} icon={<LayersIcon />}>
                  All sessions
                </SelectItem>
                {visibleSessions.map((s) => (
                  <SelectItem
                    key={sessionKey(s)}
                    value={sessionKey(s)}
                    icon={<LayersIcon />}
                  >
                    {sessionLabel(s)}
                  </SelectItem>
                ))}
              </SelectContent>
            </Select>
          )}

          <Input
            className="h-8 w-48 text-sm"
            placeholder="Search logs..."
            value={inputValue}
            onChange={(e) => handleSearchChange(e.target.value)}
          />
        </div>

        <div className="flex items-center gap-2">
          <span aria-hidden="true" className="relative flex size-2">
            {connected && (
              <span className="bg-status-ready absolute inline-flex size-full animate-ping rounded-full opacity-75" />
            )}
            <span
              className={`relative inline-flex size-2 rounded-full ${connected ? "bg-status-ready" : "bg-text-faint"}`}
            />
          </span>
          <span className="text-muted-foreground text-sm">
            {connected ? "Connected" : "Disconnected"} · {lineCount}{" "}
            {lineCount === 1 ? "entry" : "entries"}
          </span>
          <Button
            size="sm"
            variant={wrap ? "default" : "outline"}
            onClick={() => setWrap(!wrap)}
          >
            Wrap
          </Button>
          <Button
            size="sm"
            variant={follow ? "default" : "outline"}
            onClick={() => setFollow(!follow)}
          >
            {follow ? "Following" : "Follow"}
          </Button>
        </div>
      </div>

      <Card>
        <CardContent className="p-0">
          <LogView
            entries={entries}
            follow={follow}
            wrap={wrap}
            showTimestamp
            showLevel
            isLoading={isLoading}
            error={
              error ? `Failed to load log history: ${error.message}` : null
            }
            emptyMessage={
              filtered
                ? "No logs match the current filter."
                : "Waiting for logs..."
            }
            className="h-[600px]"
          />
        </CardContent>
      </Card>
    </div>
  );
}
