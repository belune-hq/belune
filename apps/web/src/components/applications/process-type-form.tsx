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
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import { Textarea } from "@/components/ui/textarea";
import { useDialogBody } from "@/lib/hooks/use-dialog-body";
import {
  useCreateProcessType,
  useUpdateProcessType,
} from "@/lib/hooks/use-process-types";
import { fieldError } from "@/lib/utils/field-error";
import {
  PROCESS_TYPE_NAME,
  allowedHealthKinds,
  type HealthKind,
} from "@/lib/utils/process-types";
import type { ProcessType } from "@/lib/types";

interface Props {
  projectId: string;
  applicationId: string;
  // null = create a new (always non-routable) type.
  processType: ProcessType | null;
  open: boolean;
  onOpenChange: (open: boolean) => void;
}

export function ProcessTypeForm({
  projectId,
  applicationId,
  processType,
  open,
  onOpenChange,
}: Props) {
  // Fields initialise from props on each open; see useDialogBody.
  const body = useDialogBody(open, processType);
  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="max-h-[calc(100dvh-2rem)] overflow-y-auto sm:max-w-lg">
        <TypeForm
          key={body.key}
          projectId={projectId}
          applicationId={applicationId}
          processType={body.target}
          onDone={() => onOpenChange(false)}
        />
      </DialogContent>
    </Dialog>
  );
}

// The select's "use the default" choice. Sent as "", which the API reads as
// "clear it back to the rule" (an absent field would keep the stored value).
const DEFAULT = "default";

const HEALTH_LABEL: Record<HealthKind, string> = {
  http: "HTTP probe",
  command: "Command (runs inside the container)",
  none: "None",
};

function TypeForm({
  projectId,
  applicationId,
  processType,
  onDone,
}: {
  projectId: string;
  applicationId: string;
  processType: ProcessType | null;
  onDone: () => void;
}) {
  const editing = !!processType;
  // Creating always makes a non-routable type: the application already has its one
  // routable type, so offering the choice would only offer a 409.
  const routable = processType?.routable ?? false;
  const create = useCreateProcessType(projectId, applicationId);
  const update = useUpdateProcessType(projectId, applicationId);

  const form = useForm({
    defaultValues: {
      name: processType?.name ?? "",
      command: processType?.command ?? "",
      healthType: processType?.health_check_type ?? DEFAULT,
      healthCommand: processType?.health_check_command ?? "",
    },
    onSubmit: ({ value }) => {
      const healthType = value.healthType === DEFAULT ? "" : value.healthType;
      const data = {
        command: value.command.trim(),
        health_check_type: healthType,
        // Only meaningful for a command check; cleared otherwise so a stale
        // command is not left behind a type that no longer uses it.
        health_check_command:
          healthType === "command" ? value.healthCommand.trim() : "",
      };
      const action = processType
        ? update.mutateAsync({ processTypeId: processType.id, data })
        : create.mutateAsync({ name: value.name.trim(), ...data });
      toast.promise(action, {
        loading: editing ? "Saving process type…" : "Creating process type…",
        success: () => {
          onDone();
          return editing
            ? "Process type saved — deploy to apply"
            : "Process type created — deploy to start it";
        },
        error: (err) => err.message,
      });
    },
  });

  const pending = create.isPending || update.isPending;
  const attempted = useStore(form.store, (st) => st.submissionAttempts > 0);
  const healthType = useStore(form.store, (st) => st.values.healthType);

  return (
    <>
      <DialogHeader>
        <DialogTitle>
          {editing ? `Edit ${processType.name}` : "Add Process Type"}
        </DialogTitle>
        <DialogDescription>
          {routable
            ? "The routable type: domains send traffic here. It runs the image's own command unless you set one."
            : "Another container from the same built image, with its own command. It receives no traffic."}
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
            onChange: z
              .string()
              .refine(
                (v) => editing || PROCESS_TYPE_NAME.test(v),
                "Lowercase letters, digits and hyphens, starting with a letter (max 20)",
              ),
          }}
        >
          {(field) => {
            const error = fieldError(field.state.meta.errors);
            // isBlurred, not isTouched: TanStack sets isTouched on the first
            // keystroke, which would flag a half-typed name as invalid.
            const invalid =
              !!error && (field.state.meta.isBlurred || attempted);
            return (
              <Field data-invalid={invalid}>
                <FieldLabel htmlFor="process-type-name">Name</FieldLabel>
                <Input
                  id="process-type-name"
                  value={field.state.value}
                  onBlur={field.handleBlur}
                  onChange={(e) => field.handleChange(e.target.value)}
                  placeholder="worker"
                  className="font-mono"
                  disabled={editing}
                  aria-invalid={invalid}
                />
                {invalid ? (
                  <FieldError>{error}</FieldError>
                ) : (
                  <p className="text-muted-foreground text-xs">
                    {editing
                      ? "A name is fixed once created."
                      : "Becomes part of the container name and cannot be changed."}
                  </p>
                )}
              </Field>
            );
          }}
        </form.Field>

        <form.Field name="command">
          {(field) => (
            <Field>
              <FieldLabel htmlFor="process-type-command">Command</FieldLabel>
              <Textarea
                id="process-type-command"
                rows={3}
                spellCheck={false}
                className="field-sizing-fixed resize-y font-mono text-xs"
                value={field.state.value}
                onBlur={field.handleBlur}
                onChange={(e) => field.handleChange(e.target.value)}
                placeholder="bundle exec sidekiq"
              />
              <p className="text-muted-foreground text-xs">
                Leave empty to run the image&apos;s own command. Otherwise it
                runs as <code>sh -c</code> with the image&apos;s entrypoint
                cleared, so the image needs a <code>/bin/sh</code>. A pipeline
                or <code>&amp;&amp;</code> chain will not stop gracefully: it is
                killed after the grace period, not asked to finish.
              </p>
            </Field>
          )}
        </form.Field>

        <form.Field name="healthType">
          {(field) => {
            const kinds = allowedHealthKinds(routable);
            return (
              <Field>
                <FieldLabel>Health check</FieldLabel>
                <Select
                  value={field.state.value}
                  onValueChange={(v) => field.handleChange(v ?? DEFAULT)}
                >
                  <SelectTrigger>
                    <SelectValue placeholder="Default" />
                  </SelectTrigger>
                  <SelectContent>
                    <SelectItem value={DEFAULT}>
                      {routable
                        ? "Default — the application's own check"
                        : "Default — none"}
                    </SelectItem>
                    {kinds.map((k) => (
                      <SelectItem key={k} value={k}>
                        {HEALTH_LABEL[k]}
                      </SelectItem>
                    ))}
                  </SelectContent>
                </Select>
                <p className="text-muted-foreground text-xs">
                  {routable
                    ? "HTTP probes the application's health path after the deploy."
                    : "An HTTP probe needs a port, so it is only offered on the routable type. Use a command check to gate a worker."}
                </p>
              </Field>
            );
          }}
        </form.Field>

        {healthType === "command" && (
          <form.Field
            name="healthCommand"
            validators={{
              // On submit only. A change validator here would run as the field
              // mounts and unmounts with the select above, and park an error on
              // a field nobody has touched.
              onSubmit: ({ value }) =>
                value.trim() === ""
                  ? "A health check command is required"
                  : undefined,
            }}
          >
            {(field) => {
              const error = fieldError(field.state.meta.errors);
              const invalid = !!error && attempted;
              return (
                <Field data-invalid={invalid}>
                  <FieldLabel htmlFor="process-type-health-command">
                    Health check command
                  </FieldLabel>
                  <Input
                    id="process-type-health-command"
                    value={field.state.value}
                    onBlur={field.handleBlur}
                    onChange={(e) => field.handleChange(e.target.value)}
                    placeholder="pgrep -f sidekiq"
                    className="font-mono"
                    aria-invalid={invalid}
                  />
                  {invalid ? (
                    <FieldError>{error}</FieldError>
                  ) : (
                    <p className="text-muted-foreground text-xs">
                      Exit 0 means healthy. Docker runs it inside the container
                      and the deploy waits for its verdict.
                    </p>
                  )}
                </Field>
              );
            }}
          </form.Field>
        )}

        <DialogFooter>
          <Button type="button" variant="outline" onClick={onDone}>
            Cancel
          </Button>
          <Button type="submit" disabled={pending}>
            {pending ? "Saving…" : editing ? "Save" : "Add Process Type"}
          </Button>
        </DialogFooter>
      </form>
    </>
  );
}
