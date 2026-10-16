import { ScheduleText } from "@/components/ui/schedule-text";
import { useMemo, useState } from "react";
import { toast } from "sonner";
import {
  Cloud,
  DatabaseBackup as DatabaseBackupIcon,
  Loader2,
  RotateCcw,
  ScrollText,
  Trash2,
} from "lucide-react";
import {
  Sheet,
  SheetContent,
  SheetHeader,
  SheetTitle,
  SheetDescription,
} from "@/components/ui/sheet";
import { LiveLogDialog } from "@/components/ui/live-log-dialog";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Separator } from "@/components/ui/separator";
import {
  Tooltip,
  TooltipContent,
  TooltipPositioner,
  TooltipTrigger,
} from "@/components/ui/tooltip";
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
  useDatabaseBackups,
  useDatabaseRestores,
  useRestoreDatabase,
  useDeleteDatabaseBackup,
} from "@/lib/hooks/use-databases";
import { useRunBackupConfig } from "@/lib/hooks/use-database-backup-configs";
import {
  formatBytes,
  formatDateTimeShort,
  formatRelativeTime,
} from "@/lib/utils/format";
import { cn } from "@/lib/utils";
import type { Database, DatabaseBackupConfig } from "@/lib/types";

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

interface Props {
  db: Database;
  config: DatabaseBackupConfig | null;
  destinationName?: string;
  open: boolean;
  onOpenChange: (open: boolean) => void;
  canDelete: boolean;
}

export function BackupConfigRunsSheet({
  db,
  config,
  destinationName,
  open,
  onOpenChange,
  canDelete,
}: Props) {
  const { data: allBackups, isLoading } = useDatabaseBackups(
    db.project_id,
    db.id,
  );
  const { data: allRestores } = useDatabaseRestores(db.project_id, db.id);
  const runConfig = useRunBackupConfig(db.project_id, db.id);
  const restore = useRestoreDatabase(db.project_id, db.id);
  const deleteBackup = useDeleteDatabaseBackup(db.project_id, db.id);

  const [logView, setLogView] = useState<{
    kind: "backup" | "restore";
    id: string;
  } | null>(null);

  const runs = useMemo(
    () => (allBackups ?? []).filter((b) => b.config_id === config?.id),
    [allBackups, config?.id],
  );
  const restores = useMemo(() => {
    const ids = new Set(runs.map((b) => b.id));
    return (allRestores ?? []).filter(
      (r) => r.backup_id && ids.has(r.backup_id),
    );
  }, [allRestores, runs]);

  // Looked up live from the polling query so the log grows in the dialog while
  // the run is still in progress.
  const viewedRun = logView
    ? logView.kind === "backup"
      ? runs.find((b) => b.id === logView.id)
      : restores.find((r) => r.id === logView.id)
    : undefined;

  if (!config) return null;

  const cfgId = config.id;
  const runNow = () => {
    toast.promise(runConfig.mutateAsync(cfgId), {
      loading: "Starting backup…",
      success: "Backup started",
      error: (err) => err.message,
    });
  };

  const handleRestore = (backupId: string) => {
    toast.promise(restore.mutateAsync(backupId), {
      loading: "Starting restore…",
      success: "Restore started — the database stays online",
      error: (err) => err.message,
    });
  };

  const handleDeleteRun = (backupId: string) => {
    toast.promise(deleteBackup.mutateAsync(backupId), {
      loading: "Deleting backup…",
      success: "Backup deleted",
      error: (err) => err.message,
    });
  };

  return (
    <Sheet open={open} onOpenChange={onOpenChange}>
      <SheetContent className="sm:max-w-lg">
        <SheetHeader>
          <SheetTitle>Backups — {db.name}</SheetTitle>
          <SheetDescription>
            Scheduled backups of <span className="font-medium">{db.name}</span>{" "}
            to {destinationName ?? "the destination"}, and restore.
          </SheetDescription>
        </SheetHeader>

        <div className="space-y-4">
          {/* Config summary */}
          <div className="flex items-start justify-between gap-3 rounded-lg border p-3 text-sm">
            <div className="min-w-0">
              <div className="flex items-center gap-2">
                <Cloud aria-hidden="true" className="size-4" />
                <span className="truncate font-medium">
                  {destinationName ?? "Destination"}
                </span>
                {config.enabled ? (
                  <Badge variant="outline">Active</Badge>
                ) : (
                  <Badge variant="secondary">Disabled</Badge>
                )}
              </div>
              <div className="text-text-faint mt-0.5 text-xs">
                {config.schedule ? (
                  <ScheduleText
                    cron={config.schedule}
                    timezone={config.timezone}
                  />
                ) : (
                  "Manual only"
                )}
                {config.keep_latest != null && ` · keep ${config.keep_latest}`}
                {config.last_run_at &&
                  ` · last run ${formatRelativeTime(config.last_run_at)}`}
              </div>
            </div>
            <Button
              size="sm"
              className="shrink-0"
              onClick={runNow}
              disabled={runConfig.isPending}
            >
              <DatabaseBackupIcon aria-hidden="true" className="size-4" />
              Back up now
            </Button>
          </div>

          <Separator />

          {/* Recent backups */}
          <div className="space-y-2">
            <div className="text-sm font-medium">Recent backups</div>
            {isLoading ? (
              <p className="text-text-faint text-sm">Loading runs…</p>
            ) : runs.length === 0 ? (
              <p className="text-muted-foreground text-sm">No backups yet.</p>
            ) : (
              <ul className="divide-border divide-y">
                {runs.map((b) => (
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
                            <Loader2 className="mr-1 inline h-3 w-3 animate-spin" />
                          )}
                          {b.status}
                        </span>
                        {b.has_remote && (
                          <Cloud
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
                      {b.status !== "running" && (b.log || b.error) && (
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
                            <ScrollText className="h-4 w-4" />
                          </TooltipTrigger>
                          <TooltipPositioner>
                            <TooltipContent>View log</TooltipContent>
                          </TooltipPositioner>
                        </Tooltip>
                      )}
                      {b.status === "succeeded" && (
                        <AlertDialog>
                          <Tooltip>
                            <TooltipTrigger
                              render={
                                <AlertDialogTrigger
                                  render={
                                    <Button
                                      variant="ghost"
                                      size="icon-sm"
                                      aria-label="Restore"
                                    />
                                  }
                                />
                              }
                            >
                              <RotateCcw className="h-4 w-4" />
                            </TooltipTrigger>
                            <TooltipPositioner>
                              <TooltipContent>Restore</TooltipContent>
                            </TooltipPositioner>
                          </Tooltip>
                          <AlertDialogContent>
                            <AlertDialogHeader>
                              <AlertDialogTitle>
                                Restore this backup?
                              </AlertDialogTitle>
                              <AlertDialogDescription>
                                This replaces the current contents of{" "}
                                <span className="font-medium">{db.name}</span>{" "}
                                with the backup from{" "}
                                {formatDateTimeShort(b.started_at)}. Data
                                written since then will be lost.
                              </AlertDialogDescription>
                            </AlertDialogHeader>
                            <AlertDialogFooter>
                              <AlertDialogCancel>Cancel</AlertDialogCancel>
                              <AlertDialogAction
                                onClick={() => handleRestore(b.id)}
                              >
                                Restore
                              </AlertDialogAction>
                            </AlertDialogFooter>
                          </AlertDialogContent>
                        </AlertDialog>
                      )}
                      {b.status !== "running" && canDelete && (
                        <AlertDialog>
                          <Tooltip>
                            <TooltipTrigger
                              render={
                                <AlertDialogTrigger
                                  render={
                                    <Button
                                      variant="ghost"
                                      size="icon-sm"
                                      aria-label="Delete backup"
                                      className="text-destructive hover:bg-destructive/10 hover:text-destructive"
                                    />
                                  }
                                />
                              }
                            >
                              <Trash2 className="h-4 w-4" />
                            </TooltipTrigger>
                            <TooltipPositioner>
                              <TooltipContent>Delete backup</TooltipContent>
                            </TooltipPositioner>
                          </Tooltip>
                          <AlertDialogContent>
                            <AlertDialogHeader>
                              <AlertDialogTitle>
                                Delete this backup?
                              </AlertDialogTitle>
                              <AlertDialogDescription>
                                Permanently removes this backup (local file and
                                any remote copy). This can't be undone.
                              </AlertDialogDescription>
                            </AlertDialogHeader>
                            <AlertDialogFooter>
                              <AlertDialogCancel>Cancel</AlertDialogCancel>
                              <AlertDialogAction
                                onClick={() => handleDeleteRun(b.id)}
                              >
                                Delete
                              </AlertDialogAction>
                            </AlertDialogFooter>
                          </AlertDialogContent>
                        </AlertDialog>
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
                  {restores.map((r) => (
                    <li
                      key={r.id}
                      className="flex items-center justify-between gap-3 py-3"
                    >
                      <div className="min-w-0">
                        <span
                          className={cn(
                            "text-sm font-medium capitalize",
                            statusTone(r.status),
                          )}
                        >
                          {r.status === "running" && (
                            <Loader2 className="mr-1 inline h-3 w-3 animate-spin" />
                          )}
                          {r.status}
                        </span>
                        <p className="text-text-faint text-xs">
                          {formatDateTimeShort(r.started_at)}
                        </p>
                        {r.error && (
                          <p className="text-status-error mt-0.5 truncate text-xs">
                            {r.error}
                          </p>
                        )}
                      </div>
                      {(r.log || r.error) && (
                        <Tooltip>
                          <TooltipTrigger
                            render={
                              <Button
                                variant="ghost"
                                size="icon-sm"
                                aria-label="View restore log"
                                onClick={() =>
                                  setLogView({ kind: "restore", id: r.id })
                                }
                              />
                            }
                          >
                            <ScrollText className="h-4 w-4" />
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

        <LiveLogDialog
          open={logView !== null}
          onOpenChange={(o) => !o && setLogView(null)}
          title={logView?.kind === "restore" ? "Restore log" : "Backup log"}
          log={viewedRun?.log?.trim() || viewedRun?.error || ""}
          running={viewedRun?.status === "running"}
        />
      </SheetContent>
    </Sheet>
  );
}
