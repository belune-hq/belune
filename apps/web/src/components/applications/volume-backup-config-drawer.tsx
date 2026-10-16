import { useMemo, useState } from "react";
import { toast } from "sonner";
import {
  CloudIcon,
  DatabaseBackupIcon,
  Loader2Icon,
  RotateCcwIcon,
  ScrollTextIcon,
} from "lucide-react";
import type { AppVolumeBackupConfig } from "@/lib/types";
import {
  useVolumeBackups,
  useVolumeRestores,
  useRunVolumeBackup,
  useRestoreVolumeBackup,
} from "@/lib/hooks/use-volume-backups";
import { formatBytes, formatDateTimeShort, formatRelativeTime } from "@/lib/utils/format";
import { humanizeSchedule } from "@/lib/utils/schedule";
import { cn } from "@/lib/utils";
import { Button } from "@/components/ui/button";
import { Badge } from "@/components/ui/badge";
import { Separator } from "@/components/ui/separator";
import {
  Tooltip,
  TooltipContent,
  TooltipPositioner,
  TooltipTrigger,
} from "@/components/ui/tooltip";
import {
  Sheet,
  SheetContent,
  SheetDescription,
  SheetHeader,
  SheetTitle,
} from "@/components/ui/sheet";
import { LiveLogDialog } from "@/components/ui/live-log-dialog";
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

interface Props {
  projectId: string;
  applicationId: string;
  config: AppVolumeBackupConfig;
  destinationName?: string;
  open: boolean;
  onOpenChange: (open: boolean) => void;
}

function statusTone(status: string): string {
  switch (status) {
    case "succeeded":
      return "text-status-ready";
    case "failed":
    case "timed_out":
      return "text-status-error";
    // Neutral, not red: a skip is normal operation. Named here so it cannot
    // fall into the in-progress default below.
    case "skipped":
      return "text-text-faint";
    default:
      return "text-status-building";
  }
}

export function VolumeBackupConfigDrawer({
  projectId,
  applicationId,
  config,
  destinationName,
  open,
  onOpenChange,
}: Props) {
  const volumeId = config.application_volume_id;
  const { data: allBackups } = useVolumeBackups(
    projectId,
    applicationId,
    volumeId,
    open,
  );
  const { data: allRestores } = useVolumeRestores(
    projectId,
    applicationId,
    volumeId,
    open,
  );
  const runBackup = useRunVolumeBackup(projectId, applicationId, volumeId);
  const restore = useRestoreVolumeBackup(projectId, applicationId, volumeId);

  const [logView, setLogView] = useState<{
    kind: "backup" | "restore";
    id: string;
  } | null>(null);
  const [restoreTarget, setRestoreTarget] = useState<string | null>(null);

  // Scope runs + restores to this config (a volume may have several configs).
  const backups = useMemo(
    () => (allBackups ?? []).filter((b) => b.config_id === config.id),
    [allBackups, config.id],
  );
  const restores = useMemo(() => {
    const ids = new Set(backups.map((b) => b.id));
    return (allRestores ?? []).filter(
      (r) => r.backup_id && ids.has(r.backup_id),
    );
  }, [allRestores, backups]);

  // The viewed run is looked up live from the polling query, so its log grows in
  // the dialog while the run is still in progress.
  const viewedRun = logView
    ? logView.kind === "backup"
      ? backups.find((b) => b.id === logView.id)
      : restores.find((r) => r.id === logView.id)
    : undefined;

  const backUpNow = () => {
    toast.promise(runBackup.mutateAsync(config.id), {
      loading: "Starting backup...",
      success: "Backup started",
      error: (err) => err.message,
    });
  };

  const doRestore = () => {
    if (!restoreTarget) return;
    toast.promise(
      restore.mutateAsync(restoreTarget).then(() => setRestoreTarget(null)),
      {
        loading: "Starting restore...",
        success: "Restore started — the app will stop, restore, and restart",
        error: (err) => err.message,
      },
    );
  };

  return (
    <Sheet open={open} onOpenChange={onOpenChange}>
      <SheetContent className="sm:max-w-lg">
        <SheetHeader>
          <SheetTitle>Backups — {config.volume_name}</SheetTitle>
          <SheetDescription>
            Snapshot <span className="font-mono">{config.mount_path}</span> to{" "}
            {destinationName ?? "the destination"} and restore it.
          </SheetDescription>
        </SheetHeader>

        <div className="space-y-4">
          {/* Config summary */}
          <div className="flex items-start justify-between gap-3 rounded-lg border p-3 text-sm">
            <div className="min-w-0">
              <div className="flex items-center gap-2">
                <CloudIcon aria-hidden="true" className="size-4" />
                <span className="truncate font-medium">
                  {destinationName ?? "Destination"}
                </span>
                {config.quiesce && <Badge variant="secondary">Quiesce</Badge>}
                {config.enabled ? (
                  <Badge variant="outline">Active</Badge>
                ) : (
                  <Badge variant="secondary">Disabled</Badge>
                )}
              </div>
              <div className="text-text-faint mt-0.5 text-xs">
                {humanizeSchedule(config.schedule)}
                {config.schedule && (
                  <>
                    {" · "}
                    <code className="font-mono">{config.schedule}</code>
                  </>
                )}
                {config.keep_latest != null && ` · keep ${config.keep_latest}`}
                {config.last_run_at &&
                  ` · last run ${formatRelativeTime(config.last_run_at)}`}
              </div>
            </div>
            <Button
              size="sm"
              className="shrink-0"
              onClick={backUpNow}
              disabled={runBackup.isPending}
            >
              <DatabaseBackupIcon aria-hidden="true" className="size-4" />
              Back up now
            </Button>
          </div>

          <Separator />

          {/* Recent backups */}
          <div className="space-y-2">
            <div className="text-sm font-medium">Recent backups</div>
            {backups.length === 0 ? (
              <p className="text-muted-foreground text-sm">No backups yet.</p>
            ) : (
              <ul className="divide-border divide-y">
                {backups.map((b) => (
                  <li
                    key={b.id}
                    className="flex items-center justify-between gap-3 py-3"
                  >
                    <div className="min-w-0">
                      <div className="flex items-center gap-2">
                        <span
                          className={cn(
                            "text-sm font-medium capitalize",
                            statusTone(b.status),
                          )}
                        >
                          {b.status === "running" && (
                            <Loader2Icon className="mr-1 inline h-3 w-3 animate-spin" />
                          )}
                          {b.status}
                        </span>
                        {b.has_remote && (
                          <CloudIcon
                            className="text-text-faint h-3.5 w-3.5"
                            aria-label="Uploaded to destination"
                          />
                        )}
                        {b.status === "succeeded" && (
                          <span className="text-text-faint text-xs">
                            {formatBytes(b.size_bytes)}
                          </span>
                        )}
                      </div>
                      <p className="text-text-faint text-xs">
                        {formatDateTimeShort(b.started_at)}
                      </p>
                      {b.error && (
                        <p className="text-status-error mt-0.5 truncate text-xs">
                          {b.error}
                        </p>
                      )}
                    </div>
                    <div className="flex items-center gap-1">
                      {b.log && (
                        <Tooltip>
                          <TooltipTrigger
                            render={
                              <Button
                                variant="ghost"
                                size="icon-sm"
                                aria-label="View log"
                                onClick={() =>
                                  setLogView({ kind: "backup", id: b.id })
                                }
                              />
                            }
                          >
                            <ScrollTextIcon className="h-4 w-4" />
                          </TooltipTrigger>
                          <TooltipPositioner>
                            <TooltipContent>View log</TooltipContent>
                          </TooltipPositioner>
                        </Tooltip>
                      )}
                      {b.status === "succeeded" && b.has_remote && (
                        <Tooltip>
                          <TooltipTrigger
                            render={
                              <Button
                                variant="ghost"
                                size="icon-sm"
                                aria-label="Restore"
                                onClick={() => setRestoreTarget(b.id)}
                              />
                            }
                          >
                            <RotateCcwIcon className="h-4 w-4" />
                          </TooltipTrigger>
                          <TooltipPositioner>
                            <TooltipContent>Restore</TooltipContent>
                          </TooltipPositioner>
                        </Tooltip>
                      )}
                    </div>
                  </li>
                ))}
              </ul>
            )}
          </div>

          {/* Recent restores */}
          {restores.length > 0 && (
            <>
              <Separator />
              <div className="space-y-2">
                <div className="text-sm font-medium">Recent restores</div>
                <ul className="divide-border divide-y">
                  {restores.map((rr) => (
                    <li
                      key={rr.id}
                      className="flex items-center justify-between gap-3 py-3"
                    >
                      <div className="min-w-0">
                        <span
                          className={cn(
                            "text-sm font-medium capitalize",
                            statusTone(rr.status),
                          )}
                        >
                          {rr.status === "running" && (
                            <Loader2Icon className="mr-1 inline h-3 w-3 animate-spin" />
                          )}
                          {rr.status}
                        </span>
                        <p className="text-text-faint text-xs">
                          {formatDateTimeShort(rr.started_at)}
                        </p>
                        {rr.error && (
                          <p className="text-status-error mt-0.5 truncate text-xs">
                            {rr.error}
                          </p>
                        )}
                      </div>
                      {(rr.log || rr.error) && (
                        <Tooltip>
                          <TooltipTrigger
                            render={
                              <Button
                                variant="ghost"
                                size="icon-sm"
                                aria-label="View restore log"
                                onClick={() =>
                                  setLogView({ kind: "restore", id: rr.id })
                                }
                              />
                            }
                          >
                            <ScrollTextIcon className="h-4 w-4" />
                          </TooltipTrigger>
                          <TooltipPositioner>
                            <TooltipContent>View log</TooltipContent>
                          </TooltipPositioner>
                        </Tooltip>
                      )}
                    </li>
                  ))}
                </ul>
              </div>
            </>
          )}
        </div>

        {/* Log viewer (streams while the run is in progress) */}
        <LiveLogDialog
          open={logView !== null}
          onOpenChange={(o) => !o && setLogView(null)}
          title={logView?.kind === "restore" ? "Restore log" : "Backup log"}
          log={viewedRun?.log?.trim() || viewedRun?.error || ""}
          running={viewedRun?.status === "running"}
        />

        {/* Restore confirm */}
        <AlertDialog
          open={restoreTarget !== null}
          onOpenChange={(o) => !o && setRestoreTarget(null)}
        >
          <AlertDialogContent>
            <AlertDialogHeader>
              <AlertDialogTitle>Restore this backup?</AlertDialogTitle>
              <AlertDialogDescription>
                This stops the application, replaces the current contents of{" "}
                <span className="font-mono">{config.mount_path}</span> with the
                snapshot, and restarts the app. Current data in the volume is
                overwritten.
              </AlertDialogDescription>
            </AlertDialogHeader>
            <AlertDialogFooter>
              <AlertDialogCancel>Cancel</AlertDialogCancel>
              <AlertDialogAction onClick={doRestore} disabled={restore.isPending}>
                Restore
              </AlertDialogAction>
            </AlertDialogFooter>
          </AlertDialogContent>
        </AlertDialog>
      </SheetContent>
    </Sheet>
  );
}
