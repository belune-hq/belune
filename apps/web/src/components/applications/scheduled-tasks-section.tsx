import { ScheduleText } from "@/components/ui/schedule-text";
import { useState } from "react";
import { toast } from "sonner";
import {
  CalendarClockIcon,
  HistoryIcon,
  Loader2Icon,
  PencilIcon,
  PlayIcon,
  PlusIcon,
  Trash2Icon,
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
import { IconAction } from "@/components/ui/icon-action";
import { Switch } from "@/components/ui/switch";
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
import {
  useDeleteScheduledTask,
  useRunScheduledTask,
  useScheduledTasks,
  useUpdateScheduledTask,
} from "@/lib/hooks/use-scheduled-tasks";
import { formatInTimezone } from "@/lib/utils/format";
import { cn } from "@/lib/utils";
import type { ScheduledTask } from "@/lib/types";
import { ScheduledTaskForm } from "./scheduled-task-form";
import { ScheduledTaskRunsSheet } from "./scheduled-task-runs-sheet";
import { runLabel, runTone } from "./scheduled-task-status";

interface Props {
  projectId: string;
  applicationId: string;
}

export function ScheduledTasksSection({ projectId, applicationId }: Props) {
  const { data: tasks, isLoading } = useScheduledTasks(
    projectId,
    applicationId,
  );
  const [formOpen, setFormOpen] = useState(false);
  const [editing, setEditing] = useState<ScheduledTask | null>(null);
  const [historyId, setHistoryId] = useState<string | null>(null);
  const historyTask = tasks?.find((t) => t.id === historyId);

  return (
    <Card>
      <CardHeader>
        <div className="flex flex-wrap items-start justify-between gap-3">
          <div>
            <CardTitle className="flex items-center gap-2">
              <CalendarClockIcon aria-hidden="true" className="size-4" />
              Scheduled Tasks
            </CardTitle>
            <CardDescription className="mt-1">
              Run a command inside the running container on a cron schedule —
              for example <code>php artisan schedule:run</code>. Tasks are
              skipped while the app is stopped, and preview apps don&apos;t
              inherit them.
            </CardDescription>
          </div>
          <Button
            size="sm"
            className="shrink-0"
            onClick={() => {
              setEditing(null);
              setFormOpen(true);
            }}
          >
            <PlusIcon aria-hidden="true" className="size-4" />
            Add Task
          </Button>
        </div>
      </CardHeader>
      <CardContent>
        {isLoading ? (
          <Skeleton className="h-16 w-full" />
        ) : !tasks || tasks.length === 0 ? (
          <p className="text-muted-foreground py-4 text-center text-sm">
            No scheduled tasks yet.
          </p>
        ) : (
          <ul className="divide-border divide-y">
            {tasks.map((t) => (
              <TaskRow
                key={t.id}
                projectId={projectId}
                applicationId={applicationId}
                task={t}
                onEdit={() => {
                  setEditing(t);
                  setFormOpen(true);
                }}
                onHistory={() => setHistoryId(t.id)}
              />
            ))}
          </ul>
        )}
      </CardContent>

      <ScheduledTaskForm
        projectId={projectId}
        applicationId={applicationId}
        task={editing}
        open={formOpen}
        onOpenChange={setFormOpen}
      />
      {historyTask && (
        <ScheduledTaskRunsSheet
          projectId={projectId}
          applicationId={applicationId}
          task={historyTask}
          open={true}
          onOpenChange={(o) => !o && setHistoryId(null)}
        />
      )}
    </Card>
  );
}

function TaskRow({
  projectId,
  applicationId,
  task,
  onEdit,
  onHistory,
}: {
  projectId: string;
  applicationId: string;
  task: ScheduledTask;
  onEdit: () => void;
  onHistory: () => void;
}) {
  const update = useUpdateScheduledTask(projectId, applicationId);
  const del = useDeleteScheduledTask(projectId, applicationId);
  const run = useRunScheduledTask(projectId, applicationId);
  const [confirmOpen, setConfirmOpen] = useState(false);
  const latest = task.latest_run;

  const toggle = (enabled: boolean) => {
    toast.promise(update.mutateAsync({ taskId: task.id, data: { enabled } }), {
      loading: enabled ? "Enabling…" : "Disabling…",
      success: enabled ? "Task enabled" : "Task disabled",
      error: (err) => err.message,
    });
  };
  const runNow = () => {
    toast.promise(run.mutateAsync(task.id), {
      loading: "Starting task…",
      success: "Task started",
      error: (err) => err.message,
    });
  };
  const remove = () => {
    toast.promise(del.mutateAsync(task.id), {
      loading: "Removing task…",
      success: "Task removed",
      error: (err) => err.message,
    });
  };

  return (
    <li className="flex items-center justify-between gap-3 py-3">
      <div className="min-w-0 space-y-1">
        <div className="flex flex-wrap items-center gap-2">
          <span className="font-medium">{task.name}</span>
          {latest && (
            <span className={cn("text-xs font-medium", runTone(latest.status))}>
              {latest.status === "running" && (
                <Loader2Icon className="mr-1 inline size-3 animate-spin" />
              )}
              {runLabel(latest, task)}
            </span>
          )}
        </div>
        <p className="text-text-faint text-xs">
          <ScheduleText cron={task.cron} timezone={task.timezone} />
        </p>
        <p className="text-text-faint text-xs">
          {task.enabled && task.next_run_at
            ? `Next run ${formatInTimezone(task.next_run_at, task.timezone)}`
            : "Disabled"}
          {latest?.status === "skipped" && latest.skip_reason
            ? ` · last skipped: ${latest.skip_reason}`
            : ""}
        </p>
      </div>
      <div className="flex shrink-0 items-center gap-1">
        <Switch
          checked={task.enabled}
          onCheckedChange={toggle}
          disabled={update.isPending}
          aria-label={`${task.name} enabled`}
        />
        <IconAction
          label="Run now"
          size="icon-sm"
          // A second click while one is in flight would exec a possibly
          // non-idempotent command twice; the server check is not atomic.
          disabled={run.isPending || latest?.status === "running"}
          onClick={runNow}
        >
          <PlayIcon aria-hidden="true" className="size-4" />
        </IconAction>
        <IconAction label="Run history" size="icon-sm" onClick={onHistory}>
          <HistoryIcon aria-hidden="true" className="size-4" />
        </IconAction>
        <IconAction label="Edit" size="icon-sm" onClick={onEdit}>
          <PencilIcon aria-hidden="true" className="size-4" />
        </IconAction>
        <IconAction
          label="Delete"
          size="icon-sm"
          destructive
          onClick={() => setConfirmOpen(true)}
        >
          <Trash2Icon aria-hidden="true" className="size-4" />
        </IconAction>
      </div>

      <AlertDialog open={confirmOpen} onOpenChange={setConfirmOpen}>
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>Delete this task?</AlertDialogTitle>
            <AlertDialogDescription>
              <span className="font-medium">{task.name}</span> and its run
              history will be removed. A run already in progress is not stopped.
            </AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel>Cancel</AlertDialogCancel>
            <AlertDialogAction variant="destructive" onClick={remove}>
              Delete
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </li>
  );
}
