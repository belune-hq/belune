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
  CardAction,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import { Skeleton } from "@/components/ui/skeleton";
import { StatusPill } from "@/components/ui/status-pill";
import { StatCard } from "@/lib/components/stats/stat-card";
import {
  describeCaddy,
  groupModules,
  summarize,
  type CaddyRow,
  type ModuleApp,
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
 * the other fix-it controls, and is linked to (top right of the Health card)
 * rather than repeated.
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
      <RoutesCard status={status} />
      <TrafficCard />
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
          What Belune can tell about the proxy itself.
        </CardDescription>
        <CardAction>
          <Link
            to="/server"
            search={{ tab: "configuration" }}
            className="text-brand-text inline-flex items-center gap-0.5 text-sm whitespace-nowrap hover:underline"
          >
            Reconcile Routes
            <ArrowUpRightIcon aria-hidden="true" className="size-3" />
          </Link>
        </CardAction>
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
          <ModuleList ids={status.caddy.modules.ids} />
        )}
      </CardContent>
    </Card>
  );
}

/**
 * Every module the running Caddy reports, grouped app → namespace. The filter
 * is local state on purpose: the status query polls, and a value derived from
 * props or query data would be wiped mid-sentence on each refetch.
 */
function ModuleList({ ids }: { ids: string[] }) {
  const [query, setQuery] = useState("");
  const apps = groupModules(ids, query);
  const matched = apps.reduce((n, a) => n + a.count, 0);
  const big = apps.filter((a) => a.count > 5);
  const small = apps.filter((a) => a.count <= 5);

  return (
    <details className="text-xs">
      <summary className="text-text-muted cursor-pointer select-none">
        All {ids.length} modules
      </summary>
      <div className="mt-3 space-y-4">
        <div className="flex flex-wrap items-center gap-3">
          <Input
            type="search"
            value={query}
            onChange={(e) => setQuery(e.target.value)}
            placeholder="Filter modules"
            aria-label="Filter modules"
            className="max-w-xs"
          />
          <span className="text-text-muted" aria-live="polite">
            {query.trim()
              ? `${matched} of ${ids.length} match`
              : `${ids.length} modules`}
          </span>
        </div>
        {apps.length === 0 ? (
          <p className="text-text-muted">No module matches “{query.trim()}”.</p>
        ) : (
          <>
            {big.map((a) => (
              <ModuleSection key={a.app} app={a} />
            ))}
            {small.length > 0 && (
              <div className="grid gap-4 sm:grid-cols-2 lg:grid-cols-3">
                {small.map((a) => (
                  <ModuleSection key={a.app} app={a} />
                ))}
              </div>
            )}
          </>
        )}
      </div>
    </details>
  );
}

function ModuleSection({ app }: { app: ModuleApp }) {
  return (
    <section className="space-y-2">
      <h4 className="text-sm font-medium">
        {app.app} <span className="text-text-faint">· {app.count}</span>
      </h4>
      {app.leaves.length > 0 && (
        // Labelled like a group, or a dotless app (`crowdsec`) would print its
        // own name twice. Not called a namespace: these have none.
        <div className="pl-3">
          <p className="text-text-muted">
            (no namespace){" "}
            <span className="text-text-faint">· {app.leaves.length}</span>
          </p>
          <ModuleLeaves leaves={app.leaves} />
        </div>
      )}
      {app.groups.map((g) => (
        <div key={g.namespace} className="pl-3">
          <p className="text-text-muted">
            {g.namespace}{" "}
            <span className="text-text-faint">· {g.leaves.length}</span>
          </p>
          <ModuleLeaves leaves={g.leaves} />
        </div>
      ))}
    </section>
  );
}

function ModuleLeaves({ leaves }: { leaves: string[] }) {
  return (
    <p className="text-text-faint font-mono break-words">
      {leaves.join(" · ")}
    </p>
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
        <CardTitle className="flex flex-wrap items-center gap-2">
          <RouteIcon aria-hidden="true" className="size-4" />
          Routes
          {/* Not the Health pill: that is Caddy's own health, this is whether
              the live table matches the database. Surfaced here so a degraded
              route is visible without scrolling to the list. */}
          {degraded.length > 0 && (
            <StatusPill
              status="degraded"
              label={`${degraded.length} served without a feature`}
              tone="building"
              pulse={false}
            />
          )}
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
                        {REASON_TEXT[d.reason]} · since{" "}
                        {formatRelativeTime(d.since)}
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
