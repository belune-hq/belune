import { useState } from "react";
import { toast } from "sonner";
import { Loader2, Play, ScrollText } from "lucide-react";
import {
  Sheet,
  SheetContent,
  SheetDescription,
  SheetHeader,
  SheetTitle,
} from "@/components/ui/sheet";
import { Button } from "@/components/ui/button";
import { IconAction } from "@/components/ui/icon-action";
import { LiveLogDialog } from "@/components/ui/live-log-dialog";
import {
  useRunScheduledTask,
  useScheduledTaskRuns,
} from "@/lib/hooks/use-scheduled-tasks";
import { formatDateTimeShort } from "@/lib/utils/format";
import { describeCron } from "@/lib/utils/schedule";
import { cn } from "@/lib/utils";
import type { ScheduledTask } from "@/lib/types";
import { runLabel, runTone } from "./scheduled-task-status";

interface Props {
  projectId: string;
  applicationId: string;
  task: ScheduledTask;
  open: boolean;
  onOpenChange: (open: boolean) => void;
}

function duration(startedAt: string, finishedAt?: string): string | null {
  if (!finishedAt) return null;
  const ms = new Date(finishedAt).getTime() - new Date(startedAt).getTime();
  return ms < 1000 ? "<1s" : `${Math.round(ms / 1000)}s`;
}

export function ScheduledTaskRunsSheet({
  projectId,
  applicationId,
  task,
  open,
  onOpenChange,
}: Props) {
  const { data: runs, isLoading } = useScheduledTaskRuns(
    projectId,
    applicationId,
    task.id,
    open,
  );
  const runNow = useRunScheduledTask(projectId, applicationId);
  const [viewId, setViewId] = useState<string | null>(null);

  // Looked up live from the polling query so output lands while a run finishes.
  const viewed = viewId ? runs?.find((r) => r.id === viewId) : undefined;

  const run = () => {
    toast.promise(runNow.mutateAsync(task.id), {
      loading: "Starting task…",
      success: "Task started",
      error: (err) => err.message,
    });
  };

  return (
    <Sheet open={open} onOpenChange={onOpenChange}>
      <SheetContent className="sm:max-w-lg">
        <SheetHeader>
          <SheetTitle>{task.name}</SheetTitle>
          <SheetDescription>
            {describeCron(task.cron)} ({task.timezone}) ·{" "}
            <code className="font-mono">{task.cron}</code>
          </SheetDescription>
        </SheetHeader>

        <div className="space-y-4 overflow-y-auto px-4 pb-4">
          <div className="flex items-start justify-between gap-3 rounded-lg border p-3 text-sm">
            <code className="min-w-0 font-mono text-xs break-all">
              {task.command}
            </code>
            <Button
              size="sm"
              className="shrink-0"
              onClick={run}
              disabled={runNow.isPending}
            >
              <Play aria-hidden="true" className="size-4" />
              Run now
            </Button>
          </div>

          <div className="space-y-2">
            <div className="text-sm font-medium">Recent runs</div>
            {isLoading ? (
              <p className="text-text-faint text-sm">Loading runs…</p>
            ) : !runs || runs.length === 0 ? (
              <p className="text-muted-foreground text-sm">No runs yet.</p>
            ) : (
              <ul className="divide-border divide-y">
                {runs.map((r) => (
                  <li
                    key={r.id}
                    className="flex items-center justify-between gap-3 py-3"
                  >
                    <div className="min-w-0">
                      <span
                        className={cn("text-sm font-medium", runTone(r.status))}
                      >
                        {r.status === "running" && (
                          <Loader2 className="mr-1 inline h-3 w-3 animate-spin" />
                        )}
                        {runLabel(r, task)}
                      </span>
                      <p className="text-text-faint text-xs">
                        {formatDateTimeShort(r.started_at)}
                        {" · "}
                        {r.trigger === "manual" ? "manual" : "scheduled"}
                        {duration(r.started_at, r.finished_at) &&
                          ` · ${duration(r.started_at, r.finished_at)}`}
                      </p>
                      {r.skip_reason && (
                        <p className="text-text-faint mt-0.5 text-xs">
                          {r.skip_reason}
                        </p>
                      )}
                    </div>
                    {r.status !== "running" && r.status !== "skipped" && (
                      <IconAction
                        label="View output"
                        size="icon-sm"
                        onClick={() => setViewId(r.id)}
                      >
                        <ScrollText aria-hidden="true" className="size-4" />
                      </IconAction>
                    )}
                  </li>
                ))}
              </ul>
            )}
          </div>
        </div>
      </SheetContent>

      <LiveLogDialog
        open={viewed != null}
        onOpenChange={(o) => !o && setViewId(null)}
        title={`${task.name} — output`}
        log={viewed?.output ?? ""}
        running={viewed?.status === "running"}
      />
    </Sheet>
  );
}
