import { useState } from "react";
import { useForm } from "@tanstack/react-form";
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
import { StatCard } from "@/lib/components/stats/stat-card";
import { WafDetectionSheet } from "@/components/proxy/waf-detection-sheet";
import {
  BADGE_VARIANT,
  categoryLabel,
  CLIENT_WOULD_BLOCK,
  describeClientList,
  describeDetectionUrl,
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
import { fieldError } from "@/lib/utils/field-error";
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
  const [clientIp, setClientIp] = useState("");
  // The events endpoint has always taken rule_id; nothing in the page set it, so
  // one noisy rule could not be isolated or excluded from view.
  const [ruleId, setRuleId] = useState<number | null>(null);
  const filters = {
    clientIp: clientIp || undefined,
    ruleId: ruleId ?? undefined,
    window: windowKey,
  };
  const clients = useWafClients(windowKey, filters);
  const groups = clients.data?.clients ?? [];
  const filtered = clientIp !== "" || ruleId !== null;
  const capped = describeClientList(
    groups.length,
    clients.data?.has_more ?? false,
    windowKey,
  );

  const form = useForm({
    defaultValues: { ip: "" },
    onSubmit: ({ value }) => setClientIp(value.ip.trim()),
  });
  const clear = () => {
    form.reset();
    setClientIp("");
    setRuleId(null);
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
        <CardAction>
          <form
            className="flex items-start gap-2"
            onSubmit={(e) => {
              e.preventDefault();
              e.stopPropagation();
              form.handleSubmit();
            }}
          >
            <form.Field
              name="ip"
              validators={{
                onChange: ({ value }) =>
                  value === "" || isIpAddress(value)
                    ? undefined
                    : "Enter an IPv4 or IPv6 address",
              }}
              children={(field) => {
                const error = fieldError(field.state.meta.errors);
                return (
                  <Field data-invalid={!!error} className="w-44">
                    <Input
                      aria-label="Filter by client address"
                      placeholder="Client address"
                      value={field.state.value}
                      onBlur={field.handleBlur}
                      onChange={(e) => field.handleChange(e.target.value)}
                    />
                    {error && <FieldError>{error}</FieldError>}
                  </Field>
                );
              }}
            />
            <Button type="submit" variant="outline">
              Filter
            </Button>
            {filtered && (
              <Button type="button" variant="ghost" onClick={clear}>
                <XIcon aria-hidden="true" className="size-4" />
                Clear
              </Button>
            )}
          </form>
        </CardAction>
      </CardHeader>
      <CardContent>
        {ruleId !== null && (
          <p className="text-muted-foreground mb-3 text-sm">
            Showing rule <span className="font-mono">{ruleId}</span> only.
          </p>
        )}
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
                {groups.map((g) => (
                  <ClientRow
                    key={g.client_ip}
                    group={g}
                    filters={filters}
                    onRule={setRuleId}
                  />
                ))}
              </TableBody>
            </Table>
            {capped && <p className="text-text-faint mt-3 text-xs">{capped}</p>}
          </>
        )}
      </CardContent>
    </Card>
  );
}

/** How many columns the row spans, so the panel below it lines up. */
const CLIENT_COLUMNS = 7;

function ClientRow({
  group: g,
  filters,
  onRule,
}: {
  group: WafClientGroup;
  filters: WafEventFilters;
  onRule: (ruleId: number) => void;
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
            <ClientDetections
              filters={{ ...filters, clientIp: g.client_ip }}
              onRule={onRule}
            />
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
function ClientDetections({
  filters,
  onRule,
}: {
  filters: WafEventFilters;
  onRule: (ruleId: number) => void;
}) {
  const events = useWafEvents(filters);
  const rows = events.data?.pages.flatMap((p) => p.events) ?? [];
  const [viewId, setViewId] = useState<number | null>(null);

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
            <TableHead>Rule</TableHead>
            <TableHead>Severity</TableHead>
            <TableHead>URL</TableHead>
            <TableHead className="w-0" />
          </TableRow>
        </TableHeader>
        <TableBody>
          {rows.map((e) => (
            <DetectionRow
              key={e.id}
              event={e}
              onRule={onRule}
              onView={() => setViewId(e.id)}
            />
          ))}
        </TableBody>
      </Table>
      {events.hasNextPage && (
        <div className="mt-2 flex justify-center">
          <Button
            variant="ghost"
            size="sm"
            onClick={() => events.fetchNextPage()}
            disabled={events.isFetchingNextPage}
          >
            {events.isFetchingNextPage ? "Loading…" : "Load more"}
          </Button>
        </div>
      )}
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
  onRule,
  onView,
}: {
  event: WafEvent;
  onRule: (ruleId: number) => void;
  onView: () => void;
}) {
  const category = categoryLabel(e.category);
  const verdict = describeEventVerdict(e);
  const url = describeDetectionUrl(e);
  return (
    <TableRow>
      {/* Absolute, not relative: the outer row answers "when was this client
          last here", which reads better as "3d", while these rows are the
          evidence for one request and are read against a log somewhere else. */}
      <TableCell className="text-muted-foreground font-mono text-xs whitespace-nowrap tabular-nums">
        {formatDateTime(e.recorded_at)}
      </TableCell>
      <TableCell className="max-w-xs">
        <div className="flex items-baseline gap-2">
          <button
            type="button"
            className="text-brand-text font-mono text-xs hover:underline"
            onClick={() => onRule(e.rule_id)}
            title="Show only this rule"
          >
            {e.rule_id}
          </button>
          <span className="truncate">{e.message}</span>
          {category && <Badge variant="outline">{category}</Badge>}
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
      <TableCell className="max-w-md">
        <div className="flex items-baseline gap-2" title={url.title}>
          <span className="truncate font-mono text-xs">
            <span className="text-muted-foreground">{url.prefix}</span>
            {url.path}
          </span>
          {url.kept ? (
            url.status !== null && (
              <Badge variant={BADGE_VARIANT[statusTone(url.status)]}>
                {url.status}
              </Badge>
            )
          ) : (
            <span className="text-muted-foreground cursor-help text-xs italic">
              URL not kept
            </span>
          )}
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
