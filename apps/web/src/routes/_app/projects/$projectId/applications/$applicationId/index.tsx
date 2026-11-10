import type { ReactNode } from "react";
import { createFileRoute, Link } from "@tanstack/react-router";
import { useApplication, useDeployHook } from "@/lib/hooks/use-applications";
import { useDeployments } from "@/lib/hooks/use-deployments";
import { useAppMetricsStream } from "@/lib/hooks/use-metrics";
import { useAuditLogs } from "@/lib/hooks/use-audit-logs";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { Badge } from "@/components/ui/badge";
import { Skeleton } from "@/components/ui/skeleton";
import { Sparkline } from "@/components/ui/sparkline";
import { MetricCard as StatMetricCard } from "@/lib/components/stats/stat-card";
import { OverviewProcessesCard } from "@/components/applications/overview-processes-card";
import {
  comparableCeiling,
  describeCoverage,
  sumAllContainers,
} from "@/lib/utils/process-usage";
import { CpuIcon, MemoryStickIcon, NetworkIcon } from "lucide-react";
import { formatRelativeTime, formatDuration } from "@/lib/utils/format";
import { actionLabel, actionClass } from "@/lib/utils/audit-format";
import { cn } from "@/lib/utils";
import type { Deployment, Application } from "@/lib/types";

export const Route = createFileRoute(
  "/_app/projects/$projectId/applications/$applicationId/",
)({
  component: ApplicationOverview,
});

const CPU_COLOR = "hsl(221, 83%, 53%)";
const MEM_COLOR = "hsl(262, 83%, 58%)";
const NET_COLOR = "hsl(142, 71%, 45%)";

function formatBytes(bytes: number | null | undefined) {
  if (bytes == null) return "—";
  const gb = bytes / (1024 * 1024 * 1024);
  if (gb >= 1) return `${gb.toFixed(1)} GB`;
  const mb = bytes / (1024 * 1024);
  if (mb >= 1) return `${mb.toFixed(0)} MB`;
  return `${(bytes / 1024).toFixed(0)} KB`;
}

function formatRate(bytesPerSec: number | null | undefined) {
  if (bytesPerSec == null) return "—";
  const mb = bytesPerSec / (1024 * 1024);
  if (mb >= 1) return `${mb.toFixed(1)} MB/s`;
  return `${(bytesPerSec / 1024).toFixed(0)} KB/s`;
}

function MetricCard({
  label,
  icon,
  value,
  sub,
  values,
  color,
}: {
  label: string;
  icon?: ReactNode;
  value: string;
  sub?: string;
  values: (number | null)[];
  color: string;
}) {
  return (
    <StatMetricCard
      label={label}
      icon={icon}
      aside={sub && <span className="text-text-faint text-xs">{sub}</span>}
    >
      <p className="mt-1 font-mono text-2xl font-semibold">{value}</p>
      <Sparkline className="mt-2" height={36} values={values} color={color} />
    </StatMetricCard>
  );
}

/**
 * Summarises how this app deploys itself, so the answer to "is auto-deploy on?"
 * is visible without hunting through Settings. Git apps can have both paths
 * active at once (push webhook and deploy hook), so both are listed.
 */
function AutoDeployValue({ app }: { app: Application }) {
  const { data: hook } = useDeployHook(app.project_id, app.id);
  const pushEnabled = app.type === "git" && app.has_webhook_secret;

  const active: string[] = [];
  if (pushEnabled) active.push(`push to ${app.auto_deploy_branch || "main"}`);
  if (hook?.enabled) active.push("deploy hook");

  if (active.length === 0) {
    return (
      // Deployments, not Settings: the Auto Deploy card lives on the
      // Deployments tab, so pointing at Settings dropped the reader on a page
      // that has no auto-deploy control at all.
      <Link
        to="/projects/$projectId/applications/$applicationId/deployments"
        params={{ projectId: app.project_id, applicationId: app.id }}
        className="text-muted-foreground hover:text-foreground underline"
      >
        Off — configure
      </Link>
    );
  }
  // Uppercase the first letter in JS rather than with CSS: `capitalize` would
  // title-case every word ("Push To Main"), and `first-letter:uppercase` maps to
  // ::first-letter, which browsers ignore on an inline element like this span.
  const summary = active.join(" + ");
  return <span>{summary.charAt(0).toUpperCase() + summary.slice(1)}</span>;
}

function BuildInfoCard({
  app,
  deploy,
}: {
  app: Application;
  deploy: Deployment | undefined;
}) {
  const duration =
    deploy?.build_started_at && deploy?.build_ended_at
      ? formatDuration(
          new Date(deploy.build_ended_at).getTime() -
            new Date(deploy.build_started_at).getTime(),
        )
      : null;

  const rows: { label: string; value: React.ReactNode }[] = [
    {
      label: "Image",
      value: deploy?.image_tag ? (
        <span className="font-mono text-xs">{deploy.image_tag}</span>
      ) : (
        (app.source_image ?? "—")
      ),
    },
    {
      label: "Build Method",
      value: (
        <Badge variant="outline" className="capitalize">
          {app.build_type}
        </Badge>
      ),
    },
    ...(app.type === "git"
      ? [
          {
            label: "Branch",
            value: app.branch ?? app.auto_deploy_branch ?? "—",
          },
          {
            label: "Commit",
            value: deploy?.commit_sha ? (
              <span className="font-mono text-xs">
                {deploy.commit_sha.slice(0, 7)}
              </span>
            ) : (
              "—"
            ),
          },
        ]
      : []),
    {
      label: "Built",
      value: deploy ? formatRelativeTime(deploy.started_at) : "—",
    },
    { label: "Duration", value: duration ?? "—" },
    { label: "Auto deploy", value: <AutoDeployValue app={app} /> },
  ];

  return (
    <Card>
      <CardHeader>
        <CardTitle>Build Info</CardTitle>
      </CardHeader>
      <CardContent className="space-y-2.5 text-sm">
        {rows.map((r) => (
          <div
            key={r.label}
            className="flex items-center justify-between gap-3"
          >
            <span className="text-muted-foreground">{r.label}</span>
            <span className="min-w-0 truncate text-right">{r.value}</span>
          </div>
        ))}
      </CardContent>
    </Card>
  );
}

function RecentEventsCard({
  projectId,
  applicationId,
}: {
  projectId: string;
  applicationId: string;
}) {
  const { data } = useAuditLogs({
    limit: 6,
    offset: 0,
    resource_type: "application",
    resource_id: applicationId,
  });
  void projectId;

  return (
    <Card>
      <CardHeader>
        <CardTitle>Recent Events</CardTitle>
      </CardHeader>
      <CardContent>
        {!data || data.items.length === 0 ? (
          <p className="text-muted-foreground py-4 text-center text-sm">
            No recent events.
          </p>
        ) : (
          <div className="space-y-2">
            {data.items.map((e) => (
              <div key={e.id} className="flex items-center gap-2 text-sm">
                <span
                  className={cn(
                    "shrink-0 rounded-md px-2 py-0.5 text-xs font-medium",
                    actionClass(e.action),
                  )}
                >
                  {actionLabel(e.action)}
                </span>
                <span className="text-muted-foreground ml-auto shrink-0 text-xs">
                  {e.user_email ?? "system"}
                </span>
                <span className="text-text-faint w-20 shrink-0 text-right text-xs">
                  {formatRelativeTime(e.created_at)}
                </span>
              </div>
            ))}
          </div>
        )}
      </CardContent>
    </Card>
  );
}

function ApplicationOverview() {
  const { projectId, applicationId } = Route.useParams();
  const { data: application } = useApplication(projectId, applicationId);
  const { data: deployments } = useDeployments(projectId, applicationId);
  const { data: metrics } = useAppMetricsStream(
    projectId,
    applicationId,
    application?.status === "running",
  );

  if (!application) {
    return (
      <div className="space-y-4">
        <div className="grid gap-4 sm:grid-cols-3">
          {[1, 2, 3].map((i) => (
            <Skeleton key={i} className="h-28 rounded-xl" />
          ))}
        </div>
      </div>
    );
  }

  // The sum across EVERY container (web replicas and workers), from the latest point's
  // per-instance samples. The point's own totals are the routable type's alone, which is
  // what this page used to show; the caption below says which one this is.
  const latest = metrics.length > 0 ? metrics[metrics.length - 1] : undefined;
  const usage = sumAllContainers(latest);
  const coverage = describeCoverage(latest);
  // Null unless the ceiling describes the same containers the usage does.
  const ceiling = comparableCeiling(latest);
  const series = metrics.map((p) => sumAllContainers(p));
  const lastDeploy = deployments?.[0];

  return (
    <div className="space-y-4">
      {/* Live resource metrics: the sum across every container, and what it covers */}
      {coverage && (
        <p
          className={cn(
            "text-xs",
            coverage.partial ? "text-status-building" : "text-text-faint",
          )}
        >
          {coverage.text}
        </p>
      )}
      <div className="grid gap-4 sm:grid-cols-3">
        <MetricCard
          label="CPU"
          icon={<CpuIcon className="size-3.5" />}
          value={usage.cpu != null ? `${usage.cpu.toFixed(0)}%` : "—"}
          values={series.map((u) => u.cpu)}
          color={CPU_COLOR}
        />
        <MetricCard
          label="Memory"
          icon={<MemoryStickIcon className="size-3.5" />}
          value={formatBytes(usage.memory)}
          sub={ceiling ? `/ ${formatBytes(ceiling)}` : undefined}
          values={series.map((u) => u.memory)}
          color={MEM_COLOR}
        />
        <MetricCard
          label="Network"
          icon={<NetworkIcon className="size-3.5" />}
          value={`↓ ${formatRate(usage.rx)}`}
          sub={`↑ ${formatRate(usage.tx)}`}
          values={series.map((u) => u.rx)}
          color={NET_COLOR}
        />
      </div>

      <OverviewProcessesCard
        projectId={projectId}
        applicationId={applicationId}
      />

      {/* Build info + recent events */}
      <div className="grid gap-4 lg:grid-cols-2">
        <BuildInfoCard app={application} deploy={lastDeploy} />
        <RecentEventsCard projectId={projectId} applicationId={applicationId} />
      </div>

      <p className="text-text-faint text-center text-xs">
        {application.status === "running"
          ? "Live metrics stream every ~2s while this page is open"
          : "Live metrics are only collected while the application is running"}{" "}
        ·{" "}
        <Link
          to="/projects/$projectId/applications/$applicationId/metrics"
          params={{ projectId, applicationId }}
          className="hover:text-foreground underline"
        >
          full metrics
        </Link>
      </p>
    </div>
  );
}
