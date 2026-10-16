import {
  TimezoneInput,
  browserTimezone,
  isKnownTimezone,
} from "@/components/ui/timezone-input";
import { useDialogBody } from "@/lib/hooks/use-dialog-body";
import { useState } from "react";
import { useForm, useStore } from "@tanstack/react-form";
import { toast } from "sonner";
import { z } from "zod";
import { ClockIcon, CloudIcon, X } from "lucide-react";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { Button } from "@/components/ui/button";
import { Checkbox } from "@/components/ui/checkbox";
import { Field, FieldError, FieldLabel } from "@/components/ui/field";
import { cronError } from "@/lib/utils/schedule";
import { fieldError } from "@/lib/utils/field-error";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import {
  useCreateBackupConfig,
  useUpdateBackupConfig,
} from "@/lib/hooks/use-database-backup-configs";
import { useBackupDestinations } from "@/lib/hooks/use-backup-destinations";
import type { DatabaseBackupConfig } from "@/lib/types";

interface Props {
  projectId: string;
  databaseId: string;
  config?: DatabaseBackupConfig | null;
  open: boolean;
  onOpenChange: (open: boolean) => void;
}

const SCHEDULE_PRESETS: { value: string; label: string }[] = [
  { value: "0 * * * *", label: "Every hour" },
  { value: "0 0 * * *", label: "Every day at midnight" },
  { value: "0 13 * * *", label: "Every day at 1:00 PM" },
  { value: "0 0 * * 0", label: "Every week (Sunday midnight)" },
  { value: "0 0 1 * *", label: "Every month (1st, midnight)" },
];

export function BackupConfigFormDialog({
  projectId,
  databaseId,
  config,
  open,
  onOpenChange,
}: Props) {
  // Fields initialise from props on each open; see useDialogBody.
  const body = useDialogBody(open, config);

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="sm:max-w-md">
        <BackupConfigForm
          key={body.key}
          projectId={projectId}
          databaseId={databaseId}
          config={body.target}
          onDone={() => onOpenChange(false)}
        />
      </DialogContent>
    </Dialog>
  );
}

function BackupConfigForm({
  projectId,
  databaseId,
  config,
  onDone,
}: {
  projectId: string;
  databaseId: string;
  config?: DatabaseBackupConfig | null;
  onDone: () => void;
}) {
  const editing = !!config;
  const { data: destinations } = useBackupDestinations(projectId);
  const create = useCreateBackupConfig(projectId, databaseId);
  const update = useUpdateBackupConfig(projectId, databaseId);

  const [dbInput, setDbInput] = useState("");

  const form = useForm({
    defaultValues: {
      destinationId: config?.destination_id ?? "",
      schedule: config?.schedule ?? "0 0 * * *",
      timezone: config?.timezone ?? browserTimezone(),
      prefix: config?.prefix ?? "",
      keepLatest: config?.keep_latest != null ? String(config.keep_latest) : "",
      enabled: config?.enabled ?? true,
      databases: config?.databases ?? ([] as string[]),
    },
    onSubmit: ({ value }) => {
      const keep =
        value.keepLatest.trim() === "" ? null : Number(value.keepLatest);
      // Include a pending typed-but-not-committed value so it isn't lost on submit.
      const pendingDb = dbInput.trim();
      const finalDatabases =
        pendingDb && !value.databases.includes(pendingDb)
          ? [...value.databases, pendingDb]
          : value.databases;
      const data = {
        destination_id: value.destinationId,
        schedule: value.schedule.trim(),
        timezone: value.timezone.trim(),
        prefix: value.prefix.trim(),
        keep_latest: keep,
        enabled: value.enabled,
        databases: finalDatabases,
      };
      const action =
        editing && config
          ? update.mutateAsync({ configId: config.id, data })
          : create.mutateAsync(data);
      toast.promise(action, {
        loading: editing ? "Saving backup…" : "Creating backup…",
        success: () => {
          onDone();
          return editing ? "Backup saved" : "Backup created";
        },
        error: (err) => err.message,
      });
    },
  });

  const databases = useStore(form.store, (s) => s.values.databases);

  const commitDbInput = () => {
    const v = dbInput.trim();
    if (v && !databases.includes(v)) {
      form.setFieldValue("databases", [...databases, v]);
    }
    setDbInput("");
  };
  const handleDbKeyDown = (e: React.KeyboardEvent<HTMLInputElement>) => {
    if (e.key === "Enter" || e.key === ",") {
      e.preventDefault();
      commitDbInput();
    } else if (e.key === "Backspace" && dbInput === "" && databases.length) {
      form.setFieldValue("databases", databases.slice(0, -1));
    }
  };

  const pending = create.isPending || update.isPending;
  const attempted = useStore(form.store, (s) => s.submissionAttempts > 0);

  return (
    <>
      <DialogHeader>
        <DialogTitle>{editing ? "Edit Backup" : "Add Backup"}</DialogTitle>
        <DialogDescription>
          Schedule recurring backups to a project destination.
        </DialogDescription>
      </DialogHeader>

      <form
        onSubmit={(e) => {
          e.preventDefault();
          e.stopPropagation();
          form.handleSubmit();
        }}
        className="space-y-3"
      >
        <form.Field
          name="destinationId"
          validators={{
            onChange: z.string().min(1, "Select a destination"),
          }}
          children={(field) => {
            const error = fieldError(field.state.meta.errors);
            return (
              <Field data-invalid={!!error}>
                <FieldLabel>Destination</FieldLabel>
                <Select
                  value={field.state.value}
                  onValueChange={(v) => field.handleChange(v ?? "")}
                >
                  <SelectTrigger aria-invalid={!!error}>
                    <SelectValue placeholder="Select destination" />
                  </SelectTrigger>
                  <SelectContent>
                    {(destinations ?? []).map((d) => (
                      <SelectItem key={d.id} value={d.id} icon={<CloudIcon />}>
                        {d.name}
                      </SelectItem>
                    ))}
                  </SelectContent>
                </Select>
                {error ? (
                  <FieldError>{error}</FieldError>
                ) : (
                  destinations &&
                  destinations.length === 0 && (
                    <p className="text-muted-foreground text-xs">
                      No destinations yet — add one on the project Backups tab
                      first.
                    </p>
                  )
                )}
              </Field>
            );
          }}
        />

        <form.Field
          name="schedule"
          validators={{
            onChange: z
              .string()
              .min(1, "Schedule is required")
              .superRefine((v, ctx) => {
                const msg = cronError(v);
                if (msg) ctx.addIssue({ code: "custom", message: msg });
              }),
          }}
          children={(field) => {
            // Gated on isBlurred (or a submit attempt), not isTouched: TanStack sets
            // isTouched on every change, so it would flash red while a valid
            // expression is still being typed (the v0.1.21 failure mode).
            const error =
              field.state.meta.isBlurred || attempted
                ? fieldError(field.state.meta.errors)
                : undefined;
            const presetValue =
              SCHEDULE_PRESETS.find((p) => p.value === field.state.value)
                ?.value ?? "custom";
            return (
              <Field data-invalid={!!error}>
                <FieldLabel>Schedule</FieldLabel>
                <Select
                  value={presetValue}
                  onValueChange={(v) => {
                    if (v && v !== "custom") field.handleChange(v);
                  }}
                >
                  <SelectTrigger className="capitalize">
                    <SelectValue placeholder="Select a predefined schedule" />
                  </SelectTrigger>
                  <SelectContent>
                    {SCHEDULE_PRESETS.map((p) => (
                      <SelectItem
                        key={p.value}
                        value={p.value}
                        icon={<ClockIcon />}
                        className="capitalize"
                      >
                        {p.label}
                      </SelectItem>
                    ))}
                    <SelectItem
                      value="custom"
                      icon={<ClockIcon />}
                      className="capitalize"
                    >
                      Custom…
                    </SelectItem>
                  </SelectContent>
                </Select>
                <Input
                  value={field.state.value}
                  onBlur={field.handleBlur}
                  onChange={(e) => field.handleChange(e.target.value)}
                  placeholder="Custom cron (e.g. 0 0 * * *)"
                  aria-invalid={!!error}
                />
                {error ? (
                  <FieldError>{error}</FieldError>
                ) : (
                  <p className="text-muted-foreground text-xs">
                    Standard 5-field cron expression.
                  </p>
                )}
              </Field>
            );
          }}
        />

        <form.Field
          name="timezone"
          validators={{
            onChange: z
              .string()
              .trim()
              .refine(isKnownTimezone, "Unknown timezone"),
          }}
          children={(field) => {
            const error = fieldError(field.state.meta.errors);
            return (
              <Field data-invalid={!!error}>
                <FieldLabel htmlFor="cfg-tz">Timezone</FieldLabel>
                <TimezoneInput
                  id="cfg-tz"
                  value={field.state.value}
                  onBlur={field.handleBlur}
                  onChange={(e) => field.handleChange(e.target.value)}
                  aria-invalid={!!error}
                />
                {error ? (
                  <FieldError>{error}</FieldError>
                ) : (
                  <p className="text-muted-foreground text-xs">
                    The schedule above is read in this timezone.
                  </p>
                )}
              </Field>
            );
          }}
        />

        <div className="space-y-1.5">
          <Label htmlFor="cfg-database">Databases</Label>
          <div className="border-input focus-within:ring-ring flex flex-wrap items-center gap-1 rounded-md border px-2 py-1.5 focus-within:ring-1">
            {databases.map((d, i) => (
              <span
                key={d}
                className="bg-muted inline-flex items-center gap-1 rounded px-1.5 py-0.5 text-xs"
              >
                <code className="font-mono">{d}</code>
                <button
                  type="button"
                  aria-label={`Remove ${d}`}
                  className="text-muted-foreground hover:text-foreground"
                  onClick={() =>
                    form.setFieldValue(
                      "databases",
                      databases.filter((_, idx) => idx !== i),
                    )
                  }
                >
                  <X className="h-3 w-3" />
                </button>
              </span>
            ))}
            <input
              id="cfg-database"
              value={dbInput}
              onChange={(e) => setDbInput(e.target.value)}
              onKeyDown={handleDbKeyDown}
              onBlur={commitDbInput}
              placeholder={databases.length ? "" : "All databases"}
              className="placeholder:text-muted-foreground min-w-24 flex-1 bg-transparent text-sm outline-none"
            />
          </div>
          <p className="text-muted-foreground text-xs">
            Databases in this container to back up (type a name, press Enter).
            Leave empty to back up <span className="font-medium">all</span>{" "}
            databases.
          </p>
        </div>

        <form.Field
          name="prefix"
          children={(field) => (
            <div className="space-y-1.5">
              <Label htmlFor="cfg-prefix">Prefix</Label>
              <Input
                id="cfg-prefix"
                value={field.state.value}
                onBlur={field.handleBlur}
                onChange={(e) => field.handleChange(e.target.value)}
                placeholder="/my-project"
              />
              <p className="text-muted-foreground text-xs">
                Optional path inside the bucket.
              </p>
            </div>
          )}
        />

        <form.Field
          name="keepLatest"
          validators={{
            onChange: z
              .string()
              .refine(
                (v) => v.trim() === "" || /^\d+$/.test(v.trim()),
                "Must be a whole number",
              )
              .refine(
                (v) => v.trim() === "" || Number(v) >= 1,
                "Must be at least 1",
              ),
          }}
          children={(field) => {
            const error = fieldError(field.state.meta.errors);
            return (
              <Field data-invalid={!!error}>
                <FieldLabel htmlFor="cfg-keep">Keep latest</FieldLabel>
                <Input
                  id="cfg-keep"
                  type="number"
                  min={1}
                  value={field.state.value}
                  onBlur={field.handleBlur}
                  onChange={(e) => field.handleChange(e.target.value)}
                  placeholder="Keeps all if empty"
                  aria-invalid={!!error}
                />
                {error ? (
                  <FieldError>{error}</FieldError>
                ) : (
                  <p className="text-muted-foreground text-xs">
                    Optional. Only keep the latest N backups in the destination.
                  </p>
                )}
              </Field>
            );
          }}
        />

        <form.Field
          name="enabled"
          children={(field) => (
            <Label className="flex items-center gap-2 text-sm font-normal">
              <Checkbox
                checked={field.state.value}
                onCheckedChange={(v) => field.handleChange(v === true)}
              />
              Enabled (run on schedule)
            </Label>
          )}
        />

        <DialogFooter>
          <Button type="submit" disabled={pending}>
            {pending ? "Saving…" : editing ? "Save" : "Create"}
          </Button>
        </DialogFooter>
      </form>
    </>
  );
}
