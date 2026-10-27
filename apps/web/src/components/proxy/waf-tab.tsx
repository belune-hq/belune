import { useEffect, useState } from "react";
import {
  ChevronDownIcon,
  ChevronRightIcon,
  EyeIcon,
  ShieldCheckIcon,
  ShieldIcon,
  XIcon,
} from "lucide-react";
import { toast } from "sonner";
import {
  AlertDialog,
  AlertDialogAction,
  AlertDialogCancel,
  AlertDialogContent,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogHeader,
  AlertDialogTitle,
} from "@/components/ui/alert-dialog";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import {
  Card,
  CardAction,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@/components/ui/card";
import { Field, FieldError } from "@/components/ui/field";
import { IconAction } from "@/components/ui/icon-action";
import { Input } from "@/components/ui/input";
import {
  SegmentedControl,
  SegmentedControlItem,
} from "@/components/ui/segmented-control";
import { Skeleton } from "@/components/ui/skeleton";
import { StatusPill } from "@/components/ui/status-pill";
import { Switch } from "@/components/ui/switch";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import { LIST_PAGE_SIZES, ListPagination } from "@/components/ui/data-table";
import { StatCard } from "@/lib/components/stats/stat-card";
import { WafDetectionSheet } from "@/components/proxy/waf-detection-sheet";
import {
  BADGE_VARIANT,
  CATEGORY_COLUMN_HELP,
  CLIENT_WOULD_BLOCK,
  describeCategory,
  describeClientList,
  describeDetectionRequest,
  describeEventVerdict,
  describeRecorder,
  describeUrlCount,
  describeWaf,
  isIpAddress,
  statusTone,
  WAF_ENABLE_POINTS,
  WAF_WINDOWS,
  windowPhrase,
  type WafTone,
} from "@/components/proxy/waf-view";
import {
  useSetWafEnabled,
  useWafClients,
  useWafEvents,
  useWafStatus,
} from "@/lib/hooks/use-waf";
import { cn } from "@/lib/utils";
import { formatDateTime, formatRelativeTime } from "@/lib/utils/format";
import type { WafEventFilters } from "@/lib/api/waf";
import type {
  WafClientGroup,
  WafEvent,
  WafStatus,
  WafWindow,
} from "@/lib/types";

const PILL_TONE = {
  neutral: "neutral",
  attention: "building",
  ready: "ready",
  error: "error",
} as const;

const TEXT_TONE: Record<WafTone, string> = {
  neutral: "text-muted-foreground",
  attention: "text-status-building",
  ready: "text-status-ready",
  error: "text-status-error",
};

/**
 * The WAF: detection only, so what it offers is a switch, the state it is in
 * and an honest account of what it has seen. It never blocks anything, and the
 * page says so rather than leaving it to be inferred.
 */
export function WafTab() {
  const [windowKey, setWindowKey] = useState<WafWindow>("24h");
  const { data: status, isLoading } = useWafStatus(windowKey);

  return (
    <div className="space-y-6">
      <WafCard status={status} loading={isLoading} />
      {status && (
        <>
          <Summary
            status={status}
            windowKey={windowKey}
            onWindow={setWindowKey}
          />
          <EventsCard status={status} windowKey={windowKey} />
        </>
      )}
    </div>
  );
}

function WafCard({
  status,
  loading,
}: {
  status: WafStatus | undefined;
  loading: boolean;
}) {
  const setEnabled = useSetWafEnabled();
  const [confirming, setConfirming] = useState(false);
  const d = status ? describeWaf(status) : null;
  const Icon = status?.state.active ? ShieldCheckIcon : ShieldIcon;

  const apply = (enabled: boolean) =>
    toast.promise(setEnabled.mutateAsync(enabled), {
      loading: enabled ? "Turning the WAF on…" : "Turning the WAF off…",
      success: enabled ? "WAF turned on" : "WAF turned off",
      error: (e: Error) => e.message || "Could not change the WAF",
    });

  return (
    <Card>
      <CardHeader>
        <CardTitle className="flex flex-wrap items-center gap-2">
          <Icon aria-hidden="true" className="size-4" />
          Web Application Firewall
          {d && (
            <StatusPill
              status={d.pill.label}
              label={d.pill.label}
              tone={PILL_TONE[d.pill.tone]}
            />
          )}
        </CardTitle>
        <CardDescription>
          Checks requests to your applications against the OWASP Core Rule Set.
          Detection only.
        </CardDescription>
        {status && d && (
          <CardAction>
            <Switch
              aria-label="Detect attacks with the WAF"
              checked={status.enabled}
              disabled={
                setEnabled.isPending || (!status.enabled && !d.canEnable)
              }
              onCheckedChange={(on) =>
                on ? setConfirming(true) : apply(false)
              }
            />
          </CardAction>
        )}
      </CardHeader>
      <CardContent className="space-y-3">
        {loading || !status || !d ? (
          <Skeleton className="h-10 w-2/3" />
        ) : (
          <>
            <p className="text-muted-foreground text-sm">{d.body}</p>
            {d.notice && (
              <p className={cn("text-sm", TEXT_TONE[d.notice.tone])}>
                {d.notice.text}
              </p>
            )}
            {describeRecorder(status.recorder).map((w) => (
              <p key={w.text} className={cn("text-sm", TEXT_TONE[w.tone])}>
                {w.text}
              </p>
            ))}
            <p className="text-text-faint text-xs">
              Events are kept for {status.retention_days} days, and at most{" "}
              {status.max_events.toLocaleString()} are kept.
            </p>
          </>
        )}
      </CardContent>

      <AlertDialog open={confirming} onOpenChange={setConfirming}>
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>Turn on attack detection?</AlertDialogTitle>
            <AlertDialogDescription>
              Before you do, what this does and does not do:
            </AlertDialogDescription>
          </AlertDialogHeader>
          <ul className="text-muted-foreground list-disc space-y-1.5 pl-5 text-sm">
            {WAF_ENABLE_POINTS.map((p) => (
              <li key={p}>{p}</li>
            ))}
          </ul>
          <AlertDialogFooter>
            <AlertDialogCancel>Cancel</AlertDialogCancel>
            <AlertDialogAction onClick={() => apply(true)}>
              Turn on
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </Card>
  );
}

/**
 * The summary over a chosen period.
 *
 * It used to render nothing whenever the period was empty, over a hardcoded 24
 * hours — so a day after an attack the whole dashboard vanished and left a bare
 * table. An empty period now says so, and says how much is kept, which is the
 * difference between "nothing happened" and "nothing happened lately".
 */
function Summary({
  status,
  windowKey,
  onWindow,
}: {
  status: WafStatus;
  windowKey: WafWindow;
  onWindow: (w: WafWindow) => void;
}) {
  const s = status.summary;
  const period = windowPhrase(windowKey);
  // Nothing has ever been recorded: the Detections card below already says that,
  // and three zeroes above an empty table only say it twice.
  if (s.total === 0) return null;

  return (
    <div className="space-y-4">
      <div className="flex flex-wrap items-center justify-between gap-2">
        <h2 className="text-sm font-medium">Detected in {period}</h2>
        <SegmentedControl
          size="sm"
          value={windowKey}
          onValueChange={(v) => onWindow(v as WafWindow)}
          aria-label="Summary period"
        >
          {WAF_WINDOWS.map((w) => (
            <SegmentedControlItem key={w.value} value={w.value}>
              {w.label}
            </SegmentedControlItem>
          ))}
        </SegmentedControl>
      </div>

      {s.events === 0 ? (
        <Card>
          <CardContent className="text-muted-foreground py-6 text-sm">
            No detections in {period}. {s.total.toLocaleString()} kept in total
            — widen the period to see them.
          </CardContent>
        </Card>
      ) : (
        <>
          <div className="grid gap-4 sm:grid-cols-3">
            <StatCard
              label="Detections"
              value={s.events.toLocaleString()}
              hint={period}
            />
            <StatCard
              label="Requests"
              value={s.requests.toLocaleString()}
              hint="that tripped a rule"
            />
            <StatCard
              label="Clients"
              value={s.clients.toLocaleString()}
              hint={period}
            />
          </div>
          {/* No "Top clients" card beside this one: the detections table below
              is that list, over the same period and in the same order, with the
              requests, URLs and rules each client touched. Two of them stacked
              read as a bug in the page. `top_clients` stays in the API, which
              other callers already consume. */}
          <Card>
            <CardHeader>
              <CardTitle className="text-base">Top rules</CardTitle>
              <CardDescription>
                The rules that matched most. A noisy one is usually a false
                positive, not an attack.
              </CardDescription>
            </CardHeader>
            <CardContent>
              <ul className="space-y-2 text-sm">
                {s.top_rules.map((r) => (
                  <li key={r.rule_id} className="flex items-baseline gap-2">
                    <span className="text-muted-foreground font-mono text-xs">
                      {r.rule_id}
                    </span>
                    <span className="min-w-0 flex-1 truncate">{r.message}</span>
                    <span className="tabular-nums">
                      {r.events.toLocaleString()}
                    </span>
                  </li>
                ))}
              </ul>
            </CardContent>
          </Card>
        </>
      )}
    </div>
  );
}

/**
 * Detections, grouped by the client that caused them.
 *
 * A flat feed was unreadable exactly when it mattered: one scanner writes
 * thousands of rows, so the pages it filled said only that something was
 * happening. One line per client says who, how hard, how broadly, and whether
 * any of it would have been refused — and expanding it gives back the per-rule
 * detail the flat list had.
 *
 * The grouped counts cover the period chosen above, which is what makes that
 * control the recency filter; the list behind an expanded client is scoped to
 * the same period, or three hits would expand into forty rows.
 */
function EventsCard({
  status,
  windowKey,
}: {
  status: WafStatus;
  windowKey: WafWindow;
}) {
  // What is typed, and what the queries actually filter on. They are separate
  // because the second only moves once typing pauses: the address is part of
  // the query key, so following every keystroke would be a request per
  // character, each one a GROUP BY over the window.
  const [draft, setDraft] = useState("");
  const [clientIp, setClientIp] = useState("");
  const [page, setPage] = useState(0);
  const [pageSize, setPageSize] = useState<number>(LIST_PAGE_SIZES[0]);
  const typed = draft.trim();
  const draftError =
    typed !== "" && !isIpAddress(typed)
      ? "Enter an IPv4 or IPv6 address"
      : null;

  useEffect(() => {
    // A half-typed address matches nothing, so the last good filter is held
    // rather than emptying the table between the first octet and the last.
    if (typed !== "" && !isIpAddress(typed)) return;
    const timer = setTimeout(() => {
      setClientIp(typed);
      setPage(0);
    }, FILTER_DEBOUNCE_MS);
    return () => clearTimeout(timer);
  }, [typed]);

  const filters = {
    clientIp: clientIp || undefined,
    window: windowKey,
  };
  const clients = useWafClients(windowKey, filters);
  const groups = clients.data?.clients ?? [];
  const pageCount = Math.max(1, Math.ceil(groups.length / pageSize));
  // Clamped rather than reset in an effect: narrowing the filter while on page
  // four shortens the list under the cursor, and a page past the end should
  // show the last one, not an empty table.
  const [pagedWindow, setPagedWindow] = useState(windowKey);
  if (pagedWindow !== windowKey) {
    setPagedWindow(windowKey);
    setPage(0);
  }
  const current = Math.min(page, pageCount - 1);
  const visible = groups.slice(current * pageSize, (current + 1) * pageSize);
  const filtered = clientIp !== "";
  const capped = describeClientList(
    groups.length,
    clients.data?.has_more ?? false,
    windowKey,
  );

  // Immediate rather than through the debounce: clearing is deliberate, and
  // waiting 300ms to act on a button press reads as a dropped click.
  const clear = () => {
    setDraft("");
    setClientIp("");
    setPage(0);
  };

  return (
    <Card>
      <CardHeader>
        <CardTitle>Detections</CardTitle>
        <CardDescription>
          One row per client, over {windowPhrase(windowKey)}. A single request
          can trip several rules, so hits outnumber requests. The matched
          payload is never stored.
        </CardDescription>
      </CardHeader>
      <CardContent>
        {/* Above the table and on its left, because it filters this table and
            nothing else on the card. Outside the loading and empty branches
            below, or a filter that matches nothing would take away the control
            needed to undo it. */}
        <div className="mb-3 flex items-start gap-2">
          <Field data-invalid={!!draftError} className="w-56">
            <Input
              aria-label="Filter by client address"
              placeholder="Client address"
              value={draft}
              onChange={(e) => setDraft(e.target.value)}
            />
            {draftError && <FieldError>{draftError}</FieldError>}
          </Field>
          {draft !== "" && (
            <Button type="button" variant="ghost" onClick={clear}>
              <XIcon aria-hidden="true" className="size-4" />
              Clear
            </Button>
          )}
        </div>
        {clients.isLoading ? (
          <Skeleton className="h-24 w-full" />
        ) : groups.length === 0 ? (
          <p className="text-muted-foreground text-sm">
            {/* Deliberately not "No detections in the last 24 hours": the
                summary above says exactly that, with the guidance to widen the
                period, and the same sentence twice reads as a rendering bug.
                This table lists clients, so it says so in its own terms. */}
            {filtered
              ? "No client matches that filter in this period."
              : !status.enabled
                ? "The WAF is off. Detections recorded while it was on appear here."
                : status.summary.total > 0
                  ? "No client tripped a rule in this period."
                  : "No detections yet."}
          </p>
        ) : (
          <>
            <Table>
              <TableHeader>
                <TableRow>
                  <TableHead className="w-9">
                    <span className="sr-only">Expand</span>
                  </TableHead>
                  <TableHead>Client</TableHead>
                  <TableHead className="text-right">Hits</TableHead>
                  <TableHead className="text-right">Requests</TableHead>
                  <TableHead className="text-right">URLs</TableHead>
                  <TableHead className="text-right">Rules</TableHead>
                  <TableHead>Last seen</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {visible.map((g) => (
                  <ClientRow key={g.client_ip} group={g} filters={filters} />
                ))}
              </TableBody>
            </Table>
            <ListPagination
              page={current}
              pageSize={pageSize}
              total={groups.length}
              onPageChange={setPage}
              onPageSizeChange={(n) => {
                setPageSize(n);
                setPage(0);
              }}
              noun="clients"
            />
            {capped && <p className="text-text-faint mt-3 text-xs">{capped}</p>}
          </>
        )}
      </CardContent>
    </Card>
  );
}

/** How many columns the row spans, so the panel below it lines up. */
const CLIENT_COLUMNS = 7;

/**
 * How long typing has to pause before the address filter is applied. The
 * address is part of the query key, so each change is a request — a GROUP BY
 * over the whole window — and following every keystroke would fire one per
 * character typed.
 */
const FILTER_DEBOUNCE_MS = 300;

function ClientRow({
  group: g,
  filters,
}: {
  group: WafClientGroup;
  filters: WafEventFilters;
}) {
  const [open, setOpen] = useState(false);
  const panelId = `waf-client-${g.client_ip.replace(/[^a-zA-Z0-9]/g, "-")}`;
  const toggle = () => setOpen((o) => !o);
  const urls = describeUrlCount(g.urls, g.requests);
  const Chevron = open ? ChevronDownIcon : ChevronRightIcon;
  const label = `${open ? "Hide" : "Show"} detections from ${g.client_ip}`;

  return (
    <>
      <TableRow>
        <TableCell className="w-9 p-1">
          <Button
            variant="ghost"
            size="icon-sm"
            aria-label={label}
            aria-expanded={open}
            aria-controls={open ? panelId : undefined}
            onClick={toggle}
          >
            <Chevron aria-hidden="true" className="size-3.5" />
          </Button>
        </TableCell>
        <TableCell>
          <div className="flex items-center gap-2">
            <button
              type="button"
              className="text-brand-text font-mono text-sm hover:underline"
              aria-expanded={open}
              aria-controls={open ? panelId : undefined}
              onClick={toggle}
              title={label}
            >
              {g.client_ip}
            </button>
            {g.would_block && (
              <Badge variant="destructive-solid" title={CLIENT_WOULD_BLOCK}>
                Would block
              </Badge>
            )}
          </div>
        </TableCell>
        <TableCell className="text-right tabular-nums">
          {g.events.toLocaleString()}
        </TableCell>
        <TableCell className="text-right tabular-nums">
          {g.requests.toLocaleString()}
        </TableCell>
        <TableCell
          className={cn(
            "text-right tabular-nums",
            urls.title && "text-muted-foreground cursor-help",
          )}
          title={urls.title ?? undefined}
        >
          {urls.text}
        </TableCell>
        <TableCell className="text-right tabular-nums">
          {g.rules.toLocaleString()}
        </TableCell>
        <TableCell
          className="text-muted-foreground"
          title={new Date(g.last_seen).toLocaleString()}
        >
          {formatRelativeTime(g.last_seen)}
        </TableCell>
      </TableRow>
      {open && (
        <TableRow id={panelId} className="hover:bg-transparent">
          {/* Tinted and ruled down its left edge, or the nested header row reads
              as a new section of the outer table rather than as this row's. */}
          <TableCell
            colSpan={CLIENT_COLUMNS}
            className="bg-muted/40 border-brand/40 border-l-2 p-0"
          >
            <ClientDetections filters={{ ...filters, clientIp: g.client_ip }} />
          </TableCell>
        </TableRow>
      )}
    </>
  );
}

/**
 * One client's detections, rule by rule. Mounted only while the row is open, so
 * a table of fifty clients is one request rather than fifty.
 */
function ClientDetections({ filters }: { filters: WafEventFilters }) {
  const events = useWafEvents(filters);
  const rows = events.data?.pages.flatMap((p) => p.events) ?? [];
  const [viewId, setViewId] = useState<number | null>(null);
  const [page, setPage] = useState(0);
  const [pageSize, setPageSize] = useState<number>(LIST_PAGE_SIZES[0]);
  const pageCount = Math.max(1, Math.ceil(rows.length / pageSize));
  const current = Math.min(page, pageCount - 1);
  const visible = rows.slice(current * pageSize, (current + 1) * pageSize);

  // Two layers of paging: the server hands over 50 detections at a time and
  // this pages through them ten at a time, so stepping off the end of what is
  // loaded fetches the next 50 rather than stopping. Setting the page before
  // the rows arrive is safe — `current` clamps until they do, so the table
  // holds still instead of blanking.
  const changePage = (p: number) => {
    if (p >= pageCount && events.hasNextPage && !events.isFetchingNextPage) {
      events.fetchNextPage();
    }
    setPage(p);
  };

  // Looked up from the live rows rather than held as state: retention sweeps
  // this table while the panel is open, and a detection that is gone should
  // close its own panel instead of freezing a copy of a row that no longer is.
  const viewed =
    viewId !== null ? rows.find((e) => e.id === viewId) : undefined;

  if (events.isLoading) {
    return <Skeleton className="m-3 h-16 w-[calc(100%-1.5rem)]" />;
  }
  if (rows.length === 0) {
    return (
      <p className="text-muted-foreground p-3 text-sm">
        These detections were swept since the counts above were taken.
      </p>
    );
  }
  return (
    <div className="px-3 py-2">
      <Table>
        <TableHeader>
          <TableRow>
            <TableHead>Date</TableHead>
            {/* Left of the rule because it is the rule's class and the two read
                as one fact: what kind, then which one. */}
            <TableHead title={CATEGORY_COLUMN_HELP}>Category</TableHead>
            <TableHead>Rule</TableHead>
            <TableHead>Severity</TableHead>
            {/* Not "URL": the path is in the detail panel, so what this column
                shows is the request, not its address. */}
            <TableHead>Request</TableHead>
            <TableHead className="w-0" />
          </TableRow>
        </TableHeader>
        <TableBody>
          {visible.map((e) => (
            <DetectionRow key={e.id} event={e} onView={() => setViewId(e.id)} />
          ))}
        </TableBody>
      </Table>
      <ListPagination
        page={current}
        pageSize={pageSize}
        total={rows.length}
        onPageChange={changePage}
        onPageSizeChange={(n) => {
          setPageSize(n);
          setPage(0);
        }}
        hasMore={events.hasNextPage}
        isLoadingMore={events.isFetchingNextPage}
        noun="detections"
      />
      {viewed && (
        <WafDetectionSheet
          event={viewed}
          open
          onOpenChange={(open) => !open && setViewId(null)}
        />
      )}
    </div>
  );
}

function DetectionRow({
  event: e,
  onView,
}: {
  event: WafEvent;
  onView: () => void;
}) {
  const category = describeCategory(e);
  const verdict = describeEventVerdict(e);
  const req = describeDetectionRequest(e);
  return (
    <TableRow>
      {/* Absolute, not relative: the outer row answers "when was this client
          last here", which reads better as "3d", while these rows are the
          evidence for one request and are read against a log somewhere else. */}
      <TableCell className="text-muted-foreground font-mono text-xs whitespace-nowrap tabular-nums">
        {formatDateTime(e.recorded_at)}
      </TableCell>
      <TableCell title={category.title}>
        {/* Monospace lowercase, like the autoban policy's own checkboxes: these
            are tag names, not prose. Not a button — the rule id filters, and a
            category filter does not exist on the events endpoint, so making
            this look like the clickable one would promise what it cannot do. */}
        {category.label ? (
          <Badge variant="outline" className="font-mono text-xs">
            {category.label}
          </Badge>
        ) : (
          <span className="text-muted-foreground cursor-help">—</span>
        )}
      </TableCell>
      {/* These two maxima are what set the nested table's natural width, and the
          nested table is what makes the OUTER client table scroll sideways when
          it does not fit — scrolling the client address out of view, which is
          the row you just expanded. Together with the other columns they come
          to ~905px, so the panel fits a 1280-wide window. */}
      <TableCell className="max-w-[17rem]">
        <div className="flex items-baseline gap-2">
          {/* Plain text, not a control: it reads as an identifier, the way the
              timestamp does, and the brand colour it used to carry promised a
              link it never was. */}
          <span className="text-muted-foreground font-mono text-xs">
            {e.rule_id}
          </span>
          {/* The message is the one field here with no second home: the panel's
              own title carries it, but only once the row has been opened. */}
          <span className="truncate" title={e.message}>
            {e.message}
          </span>
        </div>
      </TableCell>
      <TableCell>
        <Badge
          variant={
            verdict.strong ? "destructive-solid" : BADGE_VARIANT[verdict.tone]
          }
          title={verdict.title ?? undefined}
        >
          {verdict.label}
        </Badge>
      </TableCell>
      {/* Narrower than it was: the eye now carries the whole URL, so the column
          only has to be wide enough to recognise a request, not to read one. */}
      <TableCell className="max-w-[15rem]">
        <div
          className="flex items-baseline gap-1.5 font-mono text-xs"
          title={req.title}
        >
          {req.kept ? (
            <>
              <span className="text-muted-foreground">{req.method}</span>
              {req.status !== null && (
                <Badge variant={BADGE_VARIANT[statusTone(req.status)]}>
                  {req.status}
                </Badge>
              )}
            </>
          ) : (
            // The same dash the Category column uses for "nothing on record",
            // with the reason in the hover. The host is still known, because it
            // comes from the detection rather than from the request log.
            <span className="text-muted-foreground cursor-help">—</span>
          )}
          <span className="truncate">{req.host}</span>
        </div>
      </TableCell>
      <TableCell className="text-right">
        <IconAction label="Detection details" onClick={onView} size="icon-sm">
          <EyeIcon aria-hidden="true" className="size-4" />
        </IconAction>
      </TableCell>
    </TableRow>
  );
}
