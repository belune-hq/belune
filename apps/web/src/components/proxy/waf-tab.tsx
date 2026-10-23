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
  describeRecorder,
  describeWaf,
  eventHost,
  isIpAddress,
  severityTone,
  WAF_ENABLE_POINTS,
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
import type { WafEvent, WafStatus } from "@/lib/types";

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
  const { data: status, isLoading } = useWafStatus();

  return (
    <div className="space-y-6">
      <WafCard status={status} loading={isLoading} />
      {status && (
        <>
          <Summary status={status} />
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

function Summary({ status }: { status: WafStatus }) {
  const s = status.summary;
  const window = `last ${s.window_hours} hours`;
  if (s.events === 0) return null;

  return (
    <div className="space-y-4">
      <div className="grid gap-4 sm:grid-cols-3">
        <StatCard
          label="Detections"
          value={s.events.toLocaleString()}
          hint={window}
        />
        <StatCard
          label="Requests"
          value={s.requests.toLocaleString()}
          hint="that tripped a rule"
        />
        <StatCard
          label="Clients"
          value={s.clients.toLocaleString()}
          hint={window}
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
                  <span className="min-w-0 flex-1 truncate">{r.message}</span>
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
    </div>
  );
}

function EventsCard({ status }: { status: WafStatus }) {
  const [clientIp, setClientIp] = useState("");
  const events = useWafEvents({ clientIp: clientIp || undefined });
  const rows = events.data?.pages.flatMap((p) => p.events) ?? [];

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
            {clientIp && (
              <Button type="button" variant="ghost" onClick={clear}>
                <XIcon aria-hidden="true" className="size-4" />
                Clear
              </Button>
            )}
          </form>
        </CardAction>
      </CardHeader>
      <CardContent>
        {events.isLoading ? (
          <Skeleton className="h-24 w-full" />
        ) : rows.length === 0 ? (
          <p className="text-muted-foreground text-sm">
            {clientIp
              ? `No detections from ${clientIp}.`
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
                  <EventRow key={e.id} event={e} onClient={filterBy} />
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
}: {
  event: WafEvent;
  onClient: (ip: string) => void;
}) {
  const category = categoryLabel(e.category);
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
          <span className="text-muted-foreground font-mono text-xs">
            {e.rule_id}
          </span>
          <span className="truncate">{e.message}</span>
          {category && <Badge variant="outline">{category}</Badge>}
        </div>
      </TableCell>
      <TableCell>
        <Badge variant={BADGE_VARIANT[severityTone(e.severity)]}>
          {e.severity}
        </Badge>
      </TableCell>
      <TableCell className={cn(!e.host && "text-muted-foreground italic")}>
        {eventHost(e)}
      </TableCell>
    </TableRow>
  );
}
