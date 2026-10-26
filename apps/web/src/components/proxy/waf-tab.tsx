import { useState } from "react";
import { useForm } from "@tanstack/react-form";
import { ShieldCheckIcon, ShieldIcon, XIcon } from "lucide-react";
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
import {
  categoryLabel,
  describeEventVerdict,
  describeRecorder,
  describeWaf,
  eventHost,
  isIpAddress,
  WAF_ENABLE_POINTS,
  WAF_WINDOWS,
  windowPhrase,
  type WafTone,
} from "@/components/proxy/waf-view";
import {
  useSetWafEnabled,
  useWafEvents,
  useWafStatus,
} from "@/lib/hooks/use-waf";
import { cn } from "@/lib/utils";
import { fieldError } from "@/lib/utils/field-error";
import { formatRelativeTime } from "@/lib/utils/format";
import type { WafEvent, WafStatus, WafWindow } from "@/lib/types";

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

const BADGE_VARIANT = {
  error: "destructive",
  attention: "secondary",
  neutral: "outline",
  ready: "light",
} as const;

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
          <EventsCard status={status} />
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
          <div className="grid gap-4 lg:grid-cols-2">
            <Card>
              <CardHeader>
                <CardTitle className="text-base">Top rules</CardTitle>
              </CardHeader>
              <CardContent>
                <ul className="space-y-2 text-sm">
                  {s.top_rules.map((r) => (
                    <li key={r.rule_id} className="flex items-baseline gap-2">
                      <span className="text-muted-foreground font-mono text-xs">
                        {r.rule_id}
                      </span>
                      <span className="min-w-0 flex-1 truncate">
                        {r.message}
                      </span>
                      <span className="tabular-nums">
                        {r.events.toLocaleString()}
                      </span>
                    </li>
                  ))}
                </ul>
              </CardContent>
            </Card>
            <Card>
              <CardHeader>
                <CardTitle className="text-base">Top clients</CardTitle>
              </CardHeader>
              <CardContent>
                <ul className="space-y-2 text-sm">
                  {s.top_clients.map((c) => (
                    <li key={c.client_ip} className="flex items-baseline gap-2">
                      <span className="min-w-0 flex-1 truncate font-mono">
                        {c.client_ip}
                      </span>
                      <span className="text-text-faint text-xs">
                        {formatRelativeTime(c.last_seen)}
                      </span>
                      <span className="tabular-nums">
                        {c.events.toLocaleString()}
                      </span>
                    </li>
                  ))}
                </ul>
              </CardContent>
            </Card>
          </div>
        </>
      )}
    </div>
  );
}

function EventsCard({ status }: { status: WafStatus }) {
  const [clientIp, setClientIp] = useState("");
  // The events endpoint has always taken rule_id; nothing in the page set it, so
  // one noisy rule could not be isolated or excluded from view.
  const [ruleId, setRuleId] = useState<number | null>(null);
  const events = useWafEvents({
    clientIp: clientIp || undefined,
    ruleId: ruleId ?? undefined,
  });
  const rows = events.data?.pages.flatMap((p) => p.events) ?? [];
  const filtered = clientIp !== "" || ruleId !== null;

  const form = useForm({
    defaultValues: { ip: "" },
    onSubmit: ({ value }) => setClientIp(value.ip.trim()),
  });
  const filterBy = (ip: string) => {
    form.setFieldValue("ip", ip);
    setClientIp(ip);
  };
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
          Each row is one rule matching one request; a request can match
          several. The request itself is not stored.
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
        {events.isLoading ? (
          <Skeleton className="h-24 w-full" />
        ) : rows.length === 0 ? (
          <p className="text-muted-foreground text-sm">
            {filtered
              ? "No detections match that filter."
              : status.enabled
                ? "No detections yet."
                : "The WAF is off. Detections recorded while it was on appear here."}
          </p>
        ) : (
          <>
            <Table>
              <TableHeader>
                <TableRow>
                  <TableHead>When</TableHead>
                  <TableHead>Client</TableHead>
                  <TableHead>Rule</TableHead>
                  <TableHead>Severity</TableHead>
                  <TableHead>Host</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {rows.map((e) => (
                  <EventRow
                    key={e.id}
                    event={e}
                    onClient={filterBy}
                    onRule={setRuleId}
                  />
                ))}
              </TableBody>
            </Table>
            {events.hasNextPage && (
              <div className="mt-4 flex justify-center">
                <Button
                  variant="outline"
                  onClick={() => events.fetchNextPage()}
                  disabled={events.isFetchingNextPage}
                >
                  {events.isFetchingNextPage ? "Loading…" : "Load more"}
                </Button>
              </div>
            )}
          </>
        )}
      </CardContent>
    </Card>
  );
}

function EventRow({
  event: e,
  onClient,
  onRule,
}: {
  event: WafEvent;
  onClient: (ip: string) => void;
  onRule: (ruleId: number) => void;
}) {
  const category = categoryLabel(e.category);
  const verdict = describeEventVerdict(e);
  return (
    <TableRow>
      <TableCell
        className="text-muted-foreground whitespace-nowrap"
        title={new Date(e.recorded_at).toLocaleString()}
      >
        {formatRelativeTime(e.recorded_at)}
      </TableCell>
      <TableCell>
        <button
          type="button"
          className="text-brand-text font-mono text-sm hover:underline"
          onClick={() => onClient(e.client_ip)}
          title="Show only this client"
        >
          {e.client_ip}
        </button>
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
      <TableCell className={cn(!e.host && "text-muted-foreground italic")}>
        {eventHost(e)}
      </TableCell>
    </TableRow>
  );
}
