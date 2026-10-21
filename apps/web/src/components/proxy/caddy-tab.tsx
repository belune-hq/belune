import { useState } from "react";
import { Link } from "@tanstack/react-router";
import {
  ActivityIcon,
  ArrowUpRightIcon,
  CodeIcon,
  RouteIcon,
  ServerIcon,
} from "lucide-react";
import { Button } from "@/components/ui/button";
import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@/components/ui/card";
import { Skeleton } from "@/components/ui/skeleton";
import { StatusPill } from "@/components/ui/status-pill";
import { StatCard } from "@/lib/components/stats/stat-card";
import {
  describeCaddy,
  summarize,
  type CaddyRow,
} from "@/components/proxy/caddy-panel";
import { describeTraffic } from "@/components/proxy/proxy-traffic";
import {
  useProxyConfig,
  useProxyMetrics,
  useReconcilerStatus,
} from "@/lib/hooks/use-maintenance";
import { cn } from "@/lib/utils";
import { formatRelativeTime } from "@/lib/utils/format";
import type { ProxyDegradation, ReconcilerStatus } from "@/lib/types";

/**
 * Caddy's state, as one rich view. It is read-only on purpose: Belune owns the
 * route table and the reconciler rewrites anything it does not recognise within
 * 30 seconds, so an edit made here would look like it worked and silently not.
 * The one remedial action — Reconcile Routes — lives on the Server page beside
 * the other fix-it controls, and is linked to rather than repeated.
 */
export function CaddyTab() {
  const { data: status, dataUpdatedAt, isLoading } = useReconcilerStatus();

  return (
    <div className="space-y-6">
      <HealthCard
        status={status}
        snapshotAt={dataUpdatedAt}
        loading={isLoading}
      />
      <TrafficCard />
      <RoutesCard status={status} />
      <ConfigCard />
    </div>
  );
}

function HealthCard({
  status,
  snapshotAt,
  loading,
}: {
  status: ReconcilerStatus | undefined;
  snapshotAt: number;
  loading: boolean;
}) {
  // Judged at the moment of the snapshot, not render time, so the grace period
  // for a missing log cannot flip a row between polls.
  const rows = status?.caddy ? describeCaddy(status.caddy, snapshotAt) : null;
  const summary = rows ? summarize(rows) : null;

  return (
    <Card>
      <CardHeader>
        <CardTitle className="flex flex-wrap items-center gap-2">
          <ServerIcon aria-hidden="true" className="size-4" />
          Health
          {summary && (
            <StatusPill
              status={summary.label}
              label={summary.label}
              tone={summary.tone}
            />
          )}
        </CardTitle>
        <CardDescription>
          What Belune can tell about the proxy itself.{" "}
          <Link
            to="/server"
            search={{ tab: "configuration" }}
            className="text-brand-text inline-flex items-center gap-0.5 hover:underline"
          >
            Reconcile Routes on the Server page
            <ArrowUpRightIcon aria-hidden="true" className="size-3" />
          </Link>
        </CardDescription>
      </CardHeader>
      <CardContent className="space-y-4">
        {loading || !rows ? (
          <div className="space-y-3">
            <Skeleton className="h-4 w-2/3" />
            <Skeleton className="h-4 w-1/2" />
            <Skeleton className="h-4 w-3/5" />
          </div>
        ) : (
          <dl className="grid gap-x-6 gap-y-3 sm:grid-cols-[8rem_1fr]">
            {rows.map((r) => (
              <CaddyFact key={r.key} row={r} />
            ))}
          </dl>
        )}
        {status?.caddy.modules.known && (
          <details className="text-xs">
            <summary className="text-text-muted cursor-pointer select-none">
              All {status.caddy.modules.ids.length} modules
            </summary>
            <p className="text-text-faint mt-2 font-mono break-words">
              {status.caddy.modules.ids.join(" · ")}
            </p>
          </details>
        )}
      </CardContent>
    </Card>
  );
}

const FACT_DOT: Record<CaddyRow["tone"], string> = {
  ready: "bg-status-ready",
  building: "bg-status-building",
  error: "bg-status-error",
  neutral: "bg-text-faint",
};

function CaddyFact({ row }: { row: CaddyRow }) {
  return (
    <>
      <dt className="text-text-muted flex items-center gap-2 text-xs">
        <span
          aria-hidden
          className={cn("size-1.5 shrink-0 rounded-full", FACT_DOT[row.tone])}
        />
        {row.label}
      </dt>
      <dd className="min-w-0 text-sm">
        <span className="font-mono break-all">{row.value}</span>
        {row.hint && (
          <span className="text-muted-foreground mt-0.5 block text-xs break-words">
            {row.hint}
          </span>
        )}
      </dd>
    </>
  );
}

function TrafficCard() {
  const { data, isLoading } = useProxyMetrics();
  const traffic = data ? describeTraffic(data) : null;

  return (
    <Card>
      <CardHeader>
        <CardTitle className="flex items-center gap-2">
          <ActivityIcon aria-hidden="true" className="size-4" />
          Traffic
        </CardTitle>
        <CardDescription>
          Requests Caddy proxied to apps, from its own metrics. Rates cover the
          last minute. Requests answered without reaching an app, such as
          redirects and the dashboard, are not counted.
        </CardDescription>
      </CardHeader>
      <CardContent>
        {isLoading || !traffic ? (
          <Skeleton className="h-20 w-full" />
        ) : traffic.message ? (
          <p
            className={cn(
              "text-sm",
              traffic.message.tone === "error" && "text-status-error",
              traffic.message.tone === "attention" && "text-status-building",
              traffic.message.tone === "neutral" && "text-muted-foreground",
            )}
          >
            {traffic.message.text}
          </p>
        ) : (
          <div className="grid grid-cols-2 gap-3 lg:grid-cols-3">
            {traffic.tiles.map((t) => (
              <StatCard
                key={t.key}
                label={t.label}
                value={t.value}
                hint={t.hint}
                tone={t.tone}
              />
            ))}
          </div>
        )}
      </CardContent>
    </Card>
  );
}

const REASON_TEXT: Record<ProxyDegradation["reason"], string> = {
  module_absent: "this Caddy lacks the module",
  modules_unknown: "Belune could not list Caddy's modules",
};

function RoutesCard({ status }: { status: ReconcilerStatus | undefined }) {
  const ran = status && status.run_count > 0 && status.last_run_at;
  const degraded = status?.degraded ?? [];

  return (
    <Card>
      <CardHeader>
        <CardTitle className="flex items-center gap-2">
          <RouteIcon aria-hidden="true" className="size-4" />
          Routes
        </CardTitle>
        <CardDescription>
          How the live route table compares with the database. The reconciler
          checks every {status?.interval_seconds ?? 30} seconds.
        </CardDescription>
      </CardHeader>
      <CardContent className="space-y-4">
        {!status ? (
          <Skeleton className="h-16 w-full" />
        ) : (
          <>
            <dl className="grid gap-x-6 gap-y-2 text-sm sm:grid-cols-[10rem_1fr]">
              <dt className="text-text-muted text-xs">Last pass</dt>
              <dd>
                {ran
                  ? `${formatRelativeTime(status.last_run_at)} · ${status.last_duration_ms} ms`
                  : "Not run yet"}
              </dd>
              <dt className="text-text-muted text-xs">Changed that pass</dt>
              <dd>
                {ran
                  ? `${status.last_added} added · ${status.last_updated} rewritten · ${status.last_removed} removed`
                  : "—"}
              </dd>
              <dt className="text-text-muted text-xs">Drift since start</dt>
              <dd>
                {status.total_drift} over {status.run_count} passes
                <span className="text-muted-foreground block text-xs">
                  A steady number means Caddy matches the database.
                </span>
              </dd>
              {status.last_error && (
                <>
                  <dt className="text-text-muted text-xs">Last error</dt>
                  <dd className="text-status-error font-mono text-xs break-words">
                    {status.last_error}
                  </dd>
                </>
              )}
            </dl>

            {degraded.length > 0 && (
              <div className="space-y-2">
                <p className="text-sm font-medium">
                  Served without a feature ({degraded.length})
                </p>
                <ul className="divide-y rounded-md border text-sm">
                  {degraded.map((d) => (
                    <li
                      key={`${d.hostname}${d.path}${d.feature}`}
                      className="flex flex-wrap items-baseline justify-between gap-2 px-3 py-2"
                    >
                      <span className="font-mono">
                        {d.hostname}
                        {d.path}
                      </span>
                      <span className="text-muted-foreground text-xs">
                        {d.feature.replace(/_/g, " ")} withheld —{" "}
                        {REASON_TEXT[d.reason]}
                      </span>
                    </li>
                  ))}
                </ul>
              </div>
            )}
          </>
        )}
      </CardContent>
    </Card>
  );
}

function ConfigCard() {
  const [shown, setShown] = useState(false);
  const { data, isFetching, error, refetch } = useProxyConfig(shown);

  return (
    <Card>
      <CardHeader>
        <CardTitle className="flex items-center gap-2">
          <CodeIcon aria-hidden="true" className="size-4" />
          Running Configuration
        </CardTitle>
        <CardDescription>
          Read-only. Belune manages this and rewrites changes within 30 seconds,
          so it cannot be edited here. Private keys and password hashes are
          removed before it leaves the server.
        </CardDescription>
      </CardHeader>
      <CardContent className="space-y-3">
        {!shown ? (
          <Button size="sm" variant="outline" onClick={() => setShown(true)}>
            Show Configuration
          </Button>
        ) : (
          <>
            <div className="flex items-center justify-between gap-3">
              <p className="text-muted-foreground text-xs">
                {data
                  ? `${data.redacted_fields} secret value${data.redacted_fields === 1 ? "" : "s"} redacted`
                  : isFetching
                    ? "Loading…"
                    : ""}
              </p>
              <div className="flex gap-2">
                <Button
                  size="sm"
                  variant="outline"
                  disabled={isFetching}
                  onClick={() => refetch()}
                >
                  Refresh
                </Button>
                <Button
                  size="sm"
                  variant="ghost"
                  onClick={() => setShown(false)}
                >
                  Hide
                </Button>
              </div>
            </div>
            {error ? (
              <p className="text-status-error text-sm">
                {error instanceof Error
                  ? error.message
                  : "Could not read Caddy's configuration."}
              </p>
            ) : data ? (
              <pre className="bg-elev max-h-[32rem] overflow-auto rounded-md p-3 font-mono text-xs">
                {JSON.stringify(data.config, null, 2)}
              </pre>
            ) : (
              <Skeleton className="h-40 w-full" />
            )}
          </>
        )}
      </CardContent>
    </Card>
  );
}
