import { useForm, useStore } from "@tanstack/react-form";
import { toast } from "sonner";
import { z } from "zod";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { Button } from "@/components/ui/button";
import { Field, FieldError, FieldLabel } from "@/components/ui/field";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Checkbox } from "@/components/ui/checkbox";
import { Textarea } from "@/components/ui/textarea";
import {
  TimezoneInput,
  browserTimezone,
  isKnownTimezone,
} from "@/components/ui/timezone-input";
import { useDialogBody } from "@/lib/hooks/use-dialog-body";
import {
  useCreateScheduledTask,
  useUpdateScheduledTask,
} from "@/lib/hooks/use-scheduled-tasks";
import { cronError, describeCronOrNull } from "@/lib/utils/schedule";
import { fieldError } from "@/lib/utils/field-error";
import type { ScheduledTask } from "@/lib/types";

interface Props {
  projectId: string;
  applicationId: string;
  task: ScheduledTask | null;
  open: boolean;
  onOpenChange: (open: boolean) => void;
}

export function ScheduledTaskForm({
  projectId,
  applicationId,
  task,
  open,
  onOpenChange,
}: Props) {
  // Fields initialise from props on each open; see useDialogBody.
  const body = useDialogBody(open, task);
  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="max-h-[calc(100dvh-2rem)] overflow-y-auto sm:max-w-lg">
        <TaskForm
          key={body.key}
          projectId={projectId}
          applicationId={applicationId}
          task={body.target}
          onDone={() => onOpenChange(false)}
        />
      </DialogContent>
    </Dialog>
  );
}

function TaskForm({
  projectId,
  applicationId,
  task,
  onDone,
}: {
  projectId: string;
  applicationId: string;
  task: ScheduledTask | null;
  onDone: () => void;
}) {
  const editing = !!task;
  const create = useCreateScheduledTask(projectId, applicationId);
  const update = useUpdateScheduledTask(projectId, applicationId);

  const form = useForm({
    defaultValues: {
      name: task?.name ?? "",
      command: task?.command ?? "",
      cron: task?.cron ?? "",
      timezone: task?.timezone ?? browserTimezone(),
      timeout: String(task?.timeout_seconds ?? 300),
      enabled: task?.enabled ?? true,
    },
    onSubmit: ({ value }) => {
      const data = {
        name: value.name.trim(),
        command: value.command.trim(),
        cron: value.cron.trim(),
        timezone: value.timezone.trim(),
        timeout_seconds: Number(value.timeout),
        enabled: value.enabled,
      };
      const action = task
        ? update.mutateAsync({ taskId: task.id, data })
        : create.mutateAsync(data);
      toast.promise(action, {
        loading: editing ? "Saving task…" : "Creating task…",
        success: () => {
          onDone();
          return editing ? "Task saved" : "Task created";
        },
        error: (err) => err.message,
      });
    },
  });

  const pending = create.isPending || update.isPending;
  const attempted = useStore(form.store, (st) => st.submissionAttempts > 0);

  return (
    <>
      <DialogHeader>
        <DialogTitle>
          {editing ? "Edit Scheduled Task" : "Add Scheduled Task"}
        </DialogTitle>
        <DialogDescription>
          Runs a command inside this application&apos;s running container, with
          its code, environment and volumes. Nothing runs while the app is
          stopped.
        </DialogDescription>
      </DialogHeader>

      <form
        onSubmit={(e) => {
          e.preventDefault();
          e.stopPropagation();
          void form.handleSubmit();
        }}
        className="space-y-4"
      >
        <form.Field
          name="name"
          validators={{
            onChange: z.string().trim().min(1, "Name is required"),
          }}
        >
          {(field) => {
            const error = fieldError(field.state.meta.errors);
            const invalid = !!error && field.state.meta.isTouched;
            return (
              <Field data-invalid={invalid}>
                <FieldLabel htmlFor="task-name">Name</FieldLabel>
                <Input
                  id="task-name"
                  value={field.state.value}
                  onBlur={field.handleBlur}
                  onChange={(e) => field.handleChange(e.target.value)}
                  placeholder="Laravel scheduler"
                  aria-invalid={invalid}
                />
                {invalid && <FieldError>{error}</FieldError>}
              </Field>
            );
          }}
        </form.Field>

        <form.Field
          name="command"
          validators={{
            onChange: z.string().trim().min(1, "Command is required"),
          }}
        >
          {(field) => {
            const error = fieldError(field.state.meta.errors);
            const invalid = !!error && field.state.meta.isTouched;
            return (
              <Field data-invalid={invalid}>
                <FieldLabel htmlFor="task-command">Command</FieldLabel>
                <Textarea
                  id="task-command"
                  rows={3}
                  spellCheck={false}
                  className="field-sizing-fixed resize-y font-mono text-xs"
                  value={field.state.value}
                  onBlur={field.handleBlur}
                  onChange={(e) => field.handleChange(e.target.value)}
                  placeholder="php artisan schedule:run"
                  aria-invalid={invalid}
                />
                {invalid ? (
                  <FieldError>{error}</FieldError>
                ) : (
                  <p className="text-muted-foreground text-xs">
                    Run with <code>sh -c</code>. The command is stored as plain
                    text, so reference secrets as <code>$VAR</code> from the
                    environment instead of pasting them here.
                  </p>
                )}
              </Field>
            );
          }}
        </form.Field>

        <form.Field
          name="cron"
          validators={{
            onChange: z
              .string()
              .trim()
              .min(1, "Schedule is required")
              .superRefine((v, ctx) => {
                const msg = cronError(v, { minMinute: true });
                if (msg) ctx.addIssue({ code: "custom", message: msg });
              }),
          }}
        >
          {(field) => {
            const error = fieldError(field.state.meta.errors);
            // isBlurred, not isTouched: TanStack sets isTouched on every change,
            // so it would flash an error while a valid expression is still being
            // typed. A submit attempt also reveals it.
            const invalid =
              !!error && (field.state.meta.isBlurred || attempted);
            // cronstrue's output and the preset labels carry no terminating
            // full stop, so the sentence after them ran straight on ("…only in
            // December Checked once a minute…"). The fallback has its own, so
            // only a real description needs one.
            //
            // Deliberately NOT added inside describeCron: the list rows render
            // "<description> (<timezone>)", where a full stop before the zone
            // would read wrong.
            const described = describeCronOrNull(field.state.value);
            return (
              <Field data-invalid={invalid}>
                <FieldLabel htmlFor="task-cron">Schedule</FieldLabel>
                <Input
                  id="task-cron"
                  value={field.state.value}
                  onBlur={field.handleBlur}
                  onChange={(e) => field.handleChange(e.target.value)}
                  placeholder="* * * * *"
                  className="font-mono"
                  aria-invalid={invalid}
                />
                {invalid ? (
                  <FieldError>{error}</FieldError>
                ) : (
                  <p className="text-muted-foreground text-xs">
                    {described ? `${described}.` : "Standard 5-field cron."}{" "}
                    Checked once a minute, so a run can start up to 60 seconds
                    late.
                  </p>
                )}
              </Field>
            );
          }}
        </form.Field>

        <div className="grid grid-cols-2 gap-3">
          <form.Field
            name="timezone"
            validators={{
              onChange: z
                .string()
                .trim()
                .refine(isKnownTimezone, "Unknown timezone"),
            }}
          >
            {(field) => {
              const error = fieldError(field.state.meta.errors);
              const invalid = !!error && field.state.meta.isTouched;
              return (
                <Field data-invalid={invalid}>
                  <FieldLabel htmlFor="task-tz">Timezone</FieldLabel>
                  <TimezoneInput
                    id="task-tz"
                    value={field.state.value}
                    onBlur={field.handleBlur}
                    onChange={(e) => field.handleChange(e.target.value)}
                    aria-invalid={invalid}
                  />
                  {invalid && <FieldError>{error}</FieldError>}
                </Field>
              );
            }}
          </form.Field>

          <form.Field
            name="timeout"
            validators={{
              onChange: z
                .string()
                .refine(
                  (v) =>
                    /^\d+$/.test(v) && Number(v) >= 1 && Number(v) <= 86400,
                  "1 to 86400 seconds",
                ),
            }}
          >
            {(field) => {
              const error = fieldError(field.state.meta.errors);
              const invalid = !!error && field.state.meta.isTouched;
              return (
                <Field data-invalid={invalid}>
                  <FieldLabel htmlFor="task-timeout">Timeout (s)</FieldLabel>
                  <Input
                    id="task-timeout"
                    type="number"
                    min={1}
                    max={86400}
                    value={field.state.value}
                    onBlur={field.handleBlur}
                    onChange={(e) => field.handleChange(e.target.value)}
                    aria-invalid={invalid}
                  />
                  {invalid && <FieldError>{error}</FieldError>}
                </Field>
              );
            }}
          </form.Field>
        </div>

        <form.Field name="enabled">
          {(field) => (
            <div className="flex items-center gap-2">
              <Checkbox
                id="task-enabled"
                checked={field.state.value}
                onCheckedChange={(v) => field.handleChange(v === true)}
              />
              <Label htmlFor="task-enabled" className="font-normal">
                Enabled
              </Label>
            </div>
          )}
        </form.Field>

        <DialogFooter>
          <Button type="submit" disabled={pending}>
            {pending ? "Saving…" : editing ? "Save" : "Create"}
          </Button>
        </DialogFooter>
      </form>
    </>
  );
}
