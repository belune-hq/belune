import { domainScheme } from "@/lib/domain-tls";
import { createFileRoute, Outlet } from "@tanstack/react-router";
import {
  useApplication,
  useDeployApplication,
  useStopApplication,
  useStartApplication,
  useRestartApplication,
  useReloadApplication,
  useRebuildApplication,
} from "@/lib/hooks/use-applications";
import { useProject } from "@/lib/hooks/use-projects";
import { useDeployments } from "@/lib/hooks/use-deployments";
import { useDomains } from "@/lib/hooks/use-domains";
import { useProjectMetrics } from "@/lib/hooks/use-project-metrics";
import { useAppMetricsStream } from "@/lib/hooks/use-metrics";
import { useChannel } from "@/lib/hooks/use-websocket";
import { useQueryClient } from "@tanstack/react-query";
import { queryKeys } from "@/lib/hooks/query-keys";
import { useCallback } from "react";
import { toast } from "sonner";
import { Button } from "@/components/ui/button";
import {
  AlertDialog,
  AlertDialogAction,
  AlertDialogCancel,
  AlertDialogContent,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogHeader,
  AlertDialogTitle,
  AlertDialogTrigger,
} from "@/components/ui/alert-dialog";
import {
  AppWindowIcon,
  ExternalLinkIcon,
  HammerIcon,
  LayoutDashboard,
  PlayIcon,
  RefreshCwIcon,
  RotateCcwIcon,
  SquareIcon,
  SlidersHorizontal,
  Globe,
  Rocket,
  ScrollText,
  Activity,
  SquareTerminal,
  HardDrive,
  CalendarClock,
  Settings,
} from "lucide-react";
import { ButtonGroup } from "@/components/ui/button-group";
import { PageTabLinks, type PageTabLink } from "@/components/ui/page-tabs";
import { formatUptime } from "@/lib/utils/format";
import { Skeleton } from "@/components/ui/skeleton";
import { useBreadcrumbLabel } from "@/lib/hooks/use-breadcrumb";
import { StatusBadge } from "@/lib/components/status-badge";
import { PendingChangeBadge } from "@/lib/components/pending-change-badge";
import { ProvenanceNote } from "@/lib/components/provenance-note";
import { AppMetricsContext } from "@/lib/contexts/app-metrics-context";
import { RouteError } from "@/lib/components/route-error";

export const Route = createFileRoute(
  "/_app/projects/$projectId/applications/$applicationId",
)({
  component: ApplicationLayout,
  errorComponent: RouteError,
});

function ApplicationLayout() {
  const { projectId, applicationId } = Route.useParams();
  const { data: application, isLoading } = useApplication(
    projectId,
    applicationId,
  );
  const { data: project } = useProject(projectId);
  const { data: deployments } = useDeployments(projectId, applicationId);
  const { data: domains } = useDomains(projectId, applicationId);
  // ⚠️ 30s, not the hook's 5s default. On this route the whole project-metrics
  // snapshot is read for ONE field — uptime_seconds, below — and formatUptime
  // renders minutes above a minute ("5h 12m", "3d 4h"), so 11 of every 12
  // fetches at 5s changed nothing on screen. It is also the heaviest query in
  // the app, and this layout wraps every application tab including Settings,
  // Env Vars and Mounts, which show no live numbers at all. Measured: 12
  // requests/min of a 33/min page, the worst in the dashboard.
  //
  // The project overview keeps the 5s default — it draws live CPU and memory
  // per service, which is what the cadence was for.
  const { data: projectMetrics } = useProjectMetrics(projectId, 30_000);
  // Only subscribe while the container is up. Metrics describe a running
  // process; for a stopped app the stream would sit idle and the last values
  // would stay frozen on screen, reading as current.
  const appMetrics = useAppMetricsStream(
    projectId,
    applicationId,
    application?.status === "running",
  );
  const qc = useQueryClient();

  // Subscribe to real-time container status changes
  const handleContainerStatus = useCallback(() => {
    qc.invalidateQueries({
      queryKey: queryKeys.applications.detail(projectId, applicationId),
    });
  }, [qc, projectId, applicationId]);
  useChannel(`container-status:${applicationId}`, handleContainerStatus);

  const deploy = useDeployApplication(projectId, applicationId);
  const stop = useStopApplication(projectId, applicationId);
  const start = useStartApplication(projectId, applicationId);
  const restart = useRestartApplication(projectId, applicationId);
  const reload = useReloadApplication(projectId, applicationId);
  const rebuild = useRebuildApplication(projectId, applicationId);

  useBreadcrumbLabel(projectId, project?.name);
  useBreadcrumbLabel(applicationId, application?.name);

  if (isLoading) {
    return (
      <div className="space-y-6">
        <Skeleton className="h-4 w-64" />
        <div className="flex items-center gap-3">
          <Skeleton className="h-8 w-48" />
          <Skeleton className="h-6 w-16 rounded-full" />
        </div>
        <div className="flex gap-1 border-b">
          {[1, 2, 3, 4, 5].map((i) => (
            <Skeleton key={i} className="h-9 w-20" />
          ))}
        </div>
      </div>
    );
  }

  if (!application) {
    return <div className="text-destructive">Application not found.</div>;
  }

  const basePath = `/projects/${projectId}/applications/${applicationId}`;
  const primaryDomain = domains?.[0]?.hostname;
  // The scheme the domain is actually served on: an ssl_mode=off domain has no
  // HTTPS listener, so a hardcoded https:// link was dead for every one of them.
  const primaryScheme = domainScheme(domains?.[0]?.ssl_mode ?? "automatic");
  const uptimeSeconds = projectMetrics?.[applicationId]?.uptime_seconds;
  const isDeploying = deployments?.some(
    (d) =>
      d.status === "pending" ||
      d.status === "building" ||
      d.status === "deploying",
  );
  const tabs: PageTabLink[] = [
    { to: basePath, label: "Overview", exact: true, icon: LayoutDashboard },
    { to: `${basePath}/env`, label: "Env Vars", icon: SlidersHorizontal },
    { to: `${basePath}/domains`, label: "Domains", icon: Globe },
    {
      to: `${basePath}/deployments`,
      label: "Deployments",
      icon: Rocket,
      live: isDeploying,
      liveLabel: "Deployment in progress",
    },
    // Previews are unlinked until the feature is finished (planned v0.1.x). The
    // route and its components are left intact, so /previews still resolves by
    // direct URL — preview children are filtered out of the app list
    // (parent_application_id IS NULL), making that page the only way to reach
    // any that already exist. Nothing new can be created while this is hidden:
    // materialising a preview requires preview_branch_pattern +
    // preview_domain_template, which only this tab can set.
    { to: `${basePath}/logs`, label: "Logs", icon: ScrollText },
    { to: `${basePath}/metrics`, label: "Metrics", icon: Activity },
    { to: `${basePath}/terminal`, label: "Terminal", icon: SquareTerminal },
    { to: `${basePath}/mounts`, label: "Mounts", icon: HardDrive },
    { to: `${basePath}/tasks`, label: "Tasks", icon: CalendarClock },
    { to: `${basePath}/settings`, label: "Settings", icon: Settings },
  ];

  return (
    <div className="space-y-6">
      <div className="flex flex-wrap items-start justify-between gap-3">
        <div className="flex min-w-0 items-center gap-3">
          <div className="bg-elev text-text-muted grid size-11 shrink-0 place-items-center rounded-xl">
            <AppWindowIcon aria-hidden="true" className="size-5" />
          </div>
          <div className="min-w-0">
            <div className="flex items-center gap-2.5">
              <h1 className="truncate text-2xl font-semibold tracking-tight">
                {application.name}
              </h1>
              <StatusBadge status={application.status} />
              <PendingChangeBadge app={application} />
              {application.status === "running" && uptimeSeconds ? (
                <span className="text-text-faint text-sm">
                  Up {formatUptime(uptimeSeconds)}
                </span>
              ) : null}
            </div>
            <div className="text-text-faint flex flex-wrap items-center gap-x-2 gap-y-0.5 text-sm">
              <span className="truncate font-mono">{application.slug}</span>
              <ProvenanceNote
                sourceKind={application.source_kind}
                sourceRef={application.source_ref}
              />
            </div>
          </div>
        </div>
        {/* Nested ButtonGroups: each cluster is joined into one segmented
            control, and the outer group spaces the clusters apart (ButtonGroup
            adds a gap when its own children are groups). This keeps "open the
            site" and "deploy" from reading as part of the lifecycle controls
            they sit beside. */}
        <ButtonGroup aria-label="Application actions">
          {primaryDomain && (
            <ButtonGroup>
              <Button
                size="sm"
                variant="outline"
                nativeButton={false}
                render={
                  <a
                    href={`${primaryScheme}://${primaryDomain}`}
                    target="_blank"
                    rel="noopener noreferrer"
                  />
                }
              >
                {/* No size class on any icon in this row: the `sm` size variant
                    already sets svg to size-3.5, and an explicit size-4 here
                    made this one icon larger than its neighbours. */}
                <ExternalLinkIcon aria-hidden="true" />
                Open URL
              </Button>
            </ButtonGroup>
          )}
          <ButtonGroup>
            <Button
              size="sm"
              onClick={() => {
                toast.promise(deploy.mutateAsync(), {
                  loading: "Deploying...",
                  success: "Deployment started",
                  error: (err) => err.message,
                });
              }}
              disabled={deploy.isPending}
            >
              <Rocket aria-hidden="true" />
              {deploy.isPending ? "Deploying..." : "Deploy"}
            </Button>
          </ButtonGroup>
          <ButtonGroup>
            <Button
              size="sm"
              variant="outline"
              onClick={() => {
                toast.promise(restart.mutateAsync(), {
                  loading: "Restarting...",
                  success: "Application restarted",
                  error: (err) => err.message,
                });
              }}
              disabled={restart.isPending || application.status !== "running"}
            >
              <RotateCcwIcon aria-hidden="true" />
              Restart
            </Button>
            <Button
              size="sm"
              variant="outline"
              onClick={() => {
                toast.promise(reload.mutateAsync(), {
                  loading: "Reloading...",
                  success: "Reload started — applying current config",
                  error: (err) => err.message,
                });
              }}
              disabled={reload.isPending || isDeploying}
              title="Recreate the container from the current image to apply config changes (volumes, file mounts, env) — no rebuild"
            >
              <RefreshCwIcon aria-hidden="true" />
              Reload
            </Button>
            {application.type === "git" && (
              <Button
                size="sm"
                variant="outline"
                onClick={() => {
                  toast.promise(rebuild.mutateAsync(), {
                    loading: "Rebuilding...",
                    success: "Rebuild started — building the current commit",
                    error: (err) => err.message,
                  });
                }}
                disabled={rebuild.isPending || isDeploying}
                title="Rebuild the currently-deployed commit (not the latest) — picks up base-image and dependency updates"
              >
                <HammerIcon aria-hidden="true" />
                Rebuild
              </Button>
            )}
            {application.status === "running" ? (
              <AlertDialog>
                <AlertDialogTrigger
                  render={
                    <Button
                      size="sm"
                      variant="outline"
                      disabled={stop.isPending}
                    />
                  }
                >
                  <SquareIcon aria-hidden="true" />
                  {stop.isPending ? "Stopping..." : "Stop"}
                </AlertDialogTrigger>
                <AlertDialogContent>
                  <AlertDialogHeader>
                    <AlertDialogTitle>
                      Stop {application.name}?
                    </AlertDialogTitle>
                    <AlertDialogDescription>
                      This will stop the running container. You can start it
                      again later.
                    </AlertDialogDescription>
                  </AlertDialogHeader>
                  <AlertDialogFooter>
                    <AlertDialogCancel>Cancel</AlertDialogCancel>
                    <AlertDialogAction
                      onClick={() => {
                        toast.promise(stop.mutateAsync(), {
                          loading: "Stopping...",
                          success: "Application stopped",
                          error: (err) => err.message,
                        });
                      }}
                    >
                      Stop
                    </AlertDialogAction>
                  </AlertDialogFooter>
                </AlertDialogContent>
              </AlertDialog>
            ) : (
              <Button
                size="sm"
                variant="outline"
                onClick={() => {
                  toast.promise(start.mutateAsync(), {
                    loading: "Starting...",
                    success: "Application started",
                    error: (err) => err.message,
                  });
                }}
                disabled={start.isPending || isDeploying}
              >
                <PlayIcon aria-hidden="true" />
                {start.isPending ? "Starting..." : "Start"}
              </Button>
            )}
          </ButtonGroup>
        </ButtonGroup>
      </div>

      <PageTabLinks ariaLabel="Application navigation" items={tabs} />

      <AppMetricsContext value={appMetrics}>
        <Outlet />
      </AppMetricsContext>
    </div>
  );
}
