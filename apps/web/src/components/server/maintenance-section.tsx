import { useState } from "react";
import { toast } from "sonner";
import {
  ChevronDownIcon,
  FileTextIcon,
  HardDriveIcon,
  RouteIcon,
  ListChecksIcon,
  PowerIcon,
  RefreshCwIcon,
} from "lucide-react";
import { Button } from "@/components/ui/button";
import {
  Dialog,
  DialogContent,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { BlobLogViewer } from "@/components/logs/blob-log-viewer";
import { HostShellBlock } from "@/components/server/host-shell-block";
import { ButtonGroup } from "@/components/ui/button-group";
import {
  DropdownMenu,
  DropdownMenuCheckboxItem,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuSeparator,
  DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu";
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
import { Separator } from "@/components/ui/separator";
import { useDockerOverview } from "@/lib/hooks/use-docker";
import {
  useClearPendingQueue,
  useClearQueue,
  usePlatformLogs,
  useQueueStatus,
  useReconcileProxy,
  useReconcilerStatus,
  useRestartService,
  useRunCleanup,
} from "@/lib/hooks/use-maintenance";
import { useSettings, useUpdateSettings } from "@/lib/hooks/use-settings";
import type {
  CleanupAction,
  PlatformService,
  RestartableService,
} from "@/lib/api/maintenance";
import { formatBytes, formatRelativeTime } from "@/lib/utils/format";
import { cn } from "@/lib/utils";

type CleanTarget = {
  actions?: CleanupAction[]; // undefined = full cleanup
  label: string;
  description: string;
};

export function MaintenanceSection() {
  const { data: overview } = useDockerOverview();
  const runCleanup = useRunCleanup();
  const [pendingClean, setPendingClean] = useState<CleanTarget | null>(null);

  const du = overview?.disk_usage;
  const reclaimable =
    (du?.images.reclaimable ?? 0) +
    (du?.volumes.reclaimable ?? 0) +
    (du?.build_cache.reclaimable ?? 0);

  const doClean = (t: CleanTarget) => {
    setPendingClean(null);
    toast.promise(runCleanup.mutateAsync(t.actions), {
      loading: `Cleaning ${t.label}…`,
      success: "Cleanup queued",
      error: (err) => err.message,
    });
  };

  return (
    <>
      <div className="space-y-6">
        {/* ---- Disk cleanup ---- */}
        <div className="flex flex-wrap items-center justify-between gap-3">
          <div className="min-w-0">
            <div className="flex items-center gap-2">
              <HardDriveIcon className="text-text-muted size-4" />
              <p className="text-sm font-medium">Disk Cleanup</p>
              <span className="text-status-ready text-sm">
                {formatBytes(reclaimable)} reclaimable
              </span>
            </div>
            <p className="text-muted-foreground mt-1 text-xs">
              Images {formatBytes(du?.images.reclaimable ?? 0)} · Volumes{" "}
              {formatBytes(du?.volumes.reclaimable ?? 0)} · Build cache{" "}
              {formatBytes(du?.build_cache.reclaimable ?? 0)}. Protects app
              data, caches in use, and running containers.
            </p>
          </div>
          {/* Split button: the whole-job action is the button, everything
              narrower hangs off the chevron. Replaces a "Clean ▾" and a "Run
              full cleanup" sitting side by side, which read as two unrelated
              controls when one is just the broadest case of the other. */}
          <ButtonGroup>
            <Button
              size="sm"
              variant="outline"
              disabled={runCleanup.isPending}
              onClick={() =>
                setPendingClean({
                  actions: undefined,
                  label: "everything",
                  description:
                    "Runs all cleanup steps: prune old deployments, dangling images, unused volumes, build caches, and orphaned containers.",
                })
              }
            >
              Full Clean
            </Button>
            <DropdownMenu>
              <DropdownMenuTrigger
                render={
                  <Button
                    size="icon-sm"
                    variant="outline"
                    // Icon-only, so it is nameless without this.
                    aria-label="More cleanup options"
                  />
                }
              >
                <ChevronDownIcon aria-hidden="true" />
              </DropdownMenuTrigger>
              {/* min-w-60 here rather than on DropdownMenuContent: the shared
                  default (min-w-36) is sized for short menus, and widening it
                  would stretch every dropdown in the app. */}
              <DropdownMenuContent align="end" className="min-w-60">
                <DropdownMenuItem
                  onClick={() =>
                    setPendingClean({
                      actions: ["images"],
                      label: "dangling images",
                      description:
                        "Removes unreferenced (dangling) images. In-use images are kept.",
                    })
                  }
                >
                  Dangling images
                </DropdownMenuItem>
                <DropdownMenuItem
                  onClick={() =>
                    setPendingClean({
                      actions: ["volumes"],
                      label: "unused volumes",
                      description:
                        "Removes dangling volumes not owned by the platform. App data (belune-data) and in-use volumes are preserved.",
                    })
                  }
                >
                  Unused volumes
                </DropdownMenuItem>
                <DropdownMenuItem
                  onClick={() =>
                    setPendingClean({
                      actions: ["build_cache"],
                      label: "build caches",
                      description:
                        "Removes CNB cache volumes and the BuildKit builder cache. The next build repopulates them.",
                    })
                  }
                >
                  Build caches
                </DropdownMenuItem>
                <DropdownMenuItem
                  onClick={() =>
                    setPendingClean({
                      actions: ["containers"],
                      label: "orphaned containers",
                      description:
                        "Removes platform containers that no longer have a matching application. Running and stopped apps are kept.",
                    })
                  }
                >
                  Orphaned containers
                </DropdownMenuItem>
                {/* Separated because everything above runs a cleanup once, and
                    this schedules them. */}
                <DropdownMenuSeparator />
                <DailyCleanupMenuItem />
              </DropdownMenuContent>
            </DropdownMenu>
          </ButtonGroup>
        </div>

        <Separator />

        {/* ---- Proxy (Caddy) ---- */}
        <ProxyBlock />

        <Separator />

        {/* ---- Platform Logs ---- */}
        <PlatformLogsBlock />

        <Separator />

        {/* ---- Job Queue ---- */}
        <QueueBlock />

        <Separator />

        {/* ---- Services ---- */}
        <ServicesBlock />

        <Separator />

        {/* ---- Host Shell ---- */}
        {/* Daily Automatic Cleanup used to sit here; it now lives in the Disk
            Cleanup split button's menu, beside the cleanups it schedules. */}
        <HostShellBlock />
      </div>

      <AlertDialog
        open={pendingClean !== null}
        onOpenChange={(open) => !open && setPendingClean(null)}
      >
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>Clean {pendingClean?.label}?</AlertDialogTitle>
            <AlertDialogDescription>
              {pendingClean?.description}
            </AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel>Cancel</AlertDialogCancel>
            <AlertDialogAction
              onClick={() => pendingClean && doClean(pendingClean)}
            >
              Clean
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </>
  );
}

function ProxyBlock() {
  const { data: status } = useReconcilerStatus();
  const reconcile = useReconcileProxy();

  const lastRun =
    status && status.run_count > 0 && status.last_run_at
      ? formatRelativeTime(status.last_run_at)
      : null;

  const handleReconcile = () => {
    toast.promise(reconcile.mutateAsync(), {
      loading: "Reconciling routes…",
      success: (s) =>
        s.last_error
          ? `Reconciled with warnings: ${s.last_error}`
          : `Routes reconciled (+${s.last_added} / −${s.last_removed})`,
      error: (err) => err.message,
    });
  };

  return (
    <div className="flex flex-wrap items-center justify-between gap-3">
      <div className="min-w-0">
        <div className="flex items-center gap-2">
          <RouteIcon className="text-text-muted size-4" />
          <p className="text-sm font-medium">Proxy (Caddy)</p>
          {lastRun && (
            <span
              className={cn(
                "text-xs",
                status?.last_error ? "text-status-error" : "text-text-faint",
              )}
            >
              reconciled {lastRun}
              {status?.last_error ? " · error" : ""}
            </span>
          )}
        </div>
        <p className="text-muted-foreground mt-1 text-xs">
          Re-push all app routes to Caddy to fix any drift between the database
          and the live proxy.
        </p>
      </div>
      <Button
        size="sm"
        variant="outline"
        disabled={reconcile.isPending}
        onClick={handleReconcile}
      >
        Reconcile Routes
      </Button>
    </div>
  );
}

const RESTARTABLE: {
  key: RestartableService;
  label: string;
  warning: string;
}[] = [
  {
    key: "caddy",
    label: "Caddy",
    warning:
      "Restarting the proxy briefly drops connections on ports 80 and 443 while it comes back up.",
  },
  {
    key: "redis",
    label: "Redis",
    warning:
      "Redis is the job broker. Restarting it while a deploy is queued will drop any queued (not-yet-running) jobs.",
  },
];

function ServicesBlock() {
  const restart = useRestartService();
  const [pending, setPending] = useState<(typeof RESTARTABLE)[number] | null>(
    null,
  );

  const doRestart = (svc: (typeof RESTARTABLE)[number]) => {
    setPending(null);
    toast.promise(restart.mutateAsync(svc.key), {
      loading: `Restarting ${svc.label}…`,
      success: `${svc.label} restarted`,
      error: (err) => err.message,
    });
  };

  return (
    <>
      <div className="flex flex-wrap items-center justify-between gap-3">
        <div className="min-w-0">
          <div className="flex items-center gap-2">
            <PowerIcon className="text-text-muted size-4" />
            <p className="text-sm font-medium">Services</p>
          </div>
          <p className="text-muted-foreground mt-1 text-xs">
            Restart a platform service in place when it wedges. Postgres and
            Belune are intentionally not restartable from here.
          </p>
        </div>
        <div className="flex items-center gap-2">
          {RESTARTABLE.map((svc) => (
            <Button
              key={svc.key}
              size="sm"
              variant="outline"
              disabled={restart.isPending}
              onClick={() => setPending(svc)}
            >
              Restart {svc.label}
            </Button>
          ))}
        </div>
      </div>

      <AlertDialog
        open={pending !== null}
        onOpenChange={(open) => !open && setPending(null)}
      >
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>Restart {pending?.label}?</AlertDialogTitle>
            <AlertDialogDescription>{pending?.warning}</AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel>Cancel</AlertDialogCancel>
            <AlertDialogAction onClick={() => pending && doRestart(pending)}>
              Restart
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </>
  );
}

const PLATFORM_SERVICES: { key: PlatformService; label: string }[] = [
  { key: "belune", label: "Belune" },
  { key: "caddy", label: "Caddy" },
  { key: "redis", label: "Redis" },
  { key: "postgres", label: "Postgres" },
  { key: "buildkit", label: "BuildKit" },
];

function PlatformLogsBlock() {
  const [selected, setSelected] = useState<PlatformService | null>(null);
  const { data, isFetching, isError, error, refetch } =
    usePlatformLogs(selected);

  const label = PLATFORM_SERVICES.find((s) => s.key === selected)?.label ?? "";

  return (
    <>
      <div className="flex flex-wrap items-center justify-between gap-3">
        <div className="min-w-0">
          <div className="flex items-center gap-2">
            <FileTextIcon className="text-text-muted size-4" />
            <p className="text-sm font-medium">Platform Logs</p>
          </div>
          <p className="text-muted-foreground mt-1 text-xs">
            Read the last {platformLogTail.toLocaleString()} lines from a
            platform service, without SSH.
          </p>
        </div>
        <DropdownMenu>
          <DropdownMenuTrigger render={<Button size="sm" variant="outline" />}>
            View Logs
            <ChevronDownIcon aria-hidden="true" className="size-4" />
          </DropdownMenuTrigger>
          <DropdownMenuContent align="end">
            {PLATFORM_SERVICES.map((s) => (
              <DropdownMenuItem key={s.key} onClick={() => setSelected(s.key)}>
                {s.label}
              </DropdownMenuItem>
            ))}
          </DropdownMenuContent>
        </DropdownMenu>
      </div>

      <Dialog
        open={selected !== null}
        onOpenChange={(open) => !open && setSelected(null)}
      >
        <DialogContent className="sm:max-w-5xl">
          <DialogHeader>
            <DialogTitle className="flex items-center gap-2">
              {label} logs
              <Button
                size="sm"
                variant="ghost"
                disabled={isFetching}
                onClick={() => refetch()}
              >
                <RefreshCwIcon
                  aria-hidden="true"
                  className={cn("size-4", isFetching && "animate-spin")}
                />
                Refresh
              </Button>
            </DialogTitle>
          </DialogHeader>
          {isError ? (
            <p className="text-status-error text-sm">
              {(error as Error)?.message ?? "Failed to load logs."}
            </p>
          ) : (
            <BlobLogViewer
              blob={data?.content ?? ""}
              running={isFetching}
              follow
              heightClass="h-[60vh]"
              emptyMessage="No recent log output."
            />
          )}
        </DialogContent>
      </Dialog>
    </>
  );
}

const platformLogTail = 1000;

function QueueBlock() {
  const { data: status } = useQueueStatus();
  const clear = useClearQueue();
  const clearPending = useClearPendingQueue();
  const [confirm, setConfirm] = useState(false);
  const [confirmPending, setConfirmPending] = useState(false);

  const stuck = status?.total_stuck ?? 0;
  const pending = (status?.queues ?? []).reduce((n, q) => n + q.pending, 0);

  const handleClear = () => {
    setConfirm(false);
    toast.promise(clear.mutateAsync(), {
      loading: "Clearing stuck jobs…",
      success: (r) =>
        `Cleared ${r.cleared} stuck job${r.cleared === 1 ? "" : "s"}`,
      error: (err) => err.message,
    });
  };

  const handleClearPending = () => {
    setConfirmPending(false);
    toast.promise(clearPending.mutateAsync(), {
      loading: "Cancelling queued jobs…",
      success: (r) =>
        `Cancelled ${r.cleared} queued job${r.cleared === 1 ? "" : "s"}`,
      error: (err) => err.message,
    });
  };

  return (
    <>
      <div className="flex flex-wrap items-center justify-between gap-3">
        <div className="min-w-0">
          <div className="flex items-center gap-2">
            <ListChecksIcon className="text-text-muted size-4" />
            <p className="text-sm font-medium">Job Queue</p>
            <span
              className={cn(
                "text-xs",
                stuck > 0 ? "text-status-building" : "text-text-faint",
              )}
            >
              {stuck} stuck
            </span>
            <span
              className={cn(
                "text-xs",
                pending > 0 ? "text-status-building" : "text-text-faint",
              )}
            >
              · {pending} queued
            </span>
          </div>
          <p className="text-muted-foreground mt-1 text-xs">
            Clear stuck (failed/retrying) jobs, or cancel the queued backlog.
            In-flight jobs are never touched.
          </p>
        </div>
        <div className="flex items-center gap-2">
          <Button
            size="sm"
            variant="outline"
            disabled={clearPending.isPending || pending === 0}
            onClick={() => setConfirmPending(true)}
          >
            Cancel Queued Jobs
          </Button>
          <Button
            size="sm"
            variant="outline"
            disabled={clear.isPending || stuck === 0}
            onClick={() => setConfirm(true)}
          >
            Clear Stuck Jobs
          </Button>
        </div>
      </div>

      <AlertDialog open={confirm} onOpenChange={setConfirm}>
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>Clear {stuck} stuck jobs?</AlertDialogTitle>
            <AlertDialogDescription>
              Removes failed and retrying tasks from all queues. Pending and
              running tasks are left untouched.
            </AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel>Cancel</AlertDialogCancel>
            <AlertDialogAction onClick={handleClear}>Clear</AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>

      <AlertDialog open={confirmPending} onOpenChange={setConfirmPending}>
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>Cancel {pending} queued jobs?</AlertDialogTitle>
            <AlertDialogDescription>
              Removes pending (not-yet-started) tasks from all queues. A job
              that is already running is not affected.
            </AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel>Keep them</AlertDialogCancel>
            <AlertDialogAction onClick={handleClearPending}>
              Cancel jobs
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </>
  );
}

/**
 * Daily automatic cleanup, as a menu checkbox item.
 *
 * A checkbox item rather than the <Switch> this used to be: a Switch inside a
 * menu would nest an interactive control inside a menuitem, which is both an
 * accessibility problem and a click that closes the menu out from under you.
 * menuitemcheckbox is the menu-native control for a boolean, and it keeps the
 * project's toggle convention intact — this still applies immediately, with no
 * submit step.
 */
function DailyCleanupMenuItem() {
  const { data: settings } = useSettings();
  const updateSettings = useUpdateSettings();

  // Absent or any value other than "false" = enabled (matches the backend default).
  const enabled =
    settings?.find((s) => s.key === "daily_cleanup_enabled")?.value !== "false";

  const handleToggle = () => {
    const next = !enabled;
    toast.promise(
      updateSettings.mutateAsync([
        { key: "daily_cleanup_enabled", value: next ? "true" : "false" },
      ]),
      {
        loading: "Saving…",
        success: `Daily cleanup ${next ? "enabled" : "disabled"}`,
        error: (err) => err.message,
      },
    );
  };

  return (
    <DropdownMenuCheckboxItem
      checked={enabled}
      disabled={updateSettings.isPending}
      onCheckedChange={handleToggle}
      // Carries the explanation the help icon used to hold. On the item itself
      // rather than an aria-hidden icon, so it is actually reachable.
      title="Runs the full cleanup once a day. When off, disk is only reclaimed when you run cleanup manually."
    >
      Daily automatic cleanup
    </DropdownMenuCheckboxItem>
  );
}
